"use client";

// usePhoneCalls manages the phone (voice / video call) sessions, one
// per (channel, peer subscriber) pair. Two deliberately separate layers:
//
// - The session PROTOCOL — the cause. The actions are their own wire
//   frames: a SIP subset (application/x-sip DCMsgs, see
//   datachannel.tsx) with the SDP body stripped — the caller's INVITE
//   opens the dialog (also stored as the call's log entry), the callee
//   answers it with a response (200 OK / 603 Decline), the caller
//   callee answers it with a response (200 OK / 603 Decline), the
//   caller aborts the ring with a CANCEL, either party hangs up with a
//   BYE. Each end folds the dialog's messages into its state below —
//   its own sends fold in at send time (a SIP message is never
//   echoed), the peer's sends on arrival; media (useCallMedia) and the
//   live indicators (the answer popup, the sidebar pills, the
//   conversation strip) read this state.
//
// - The session's UI STATE — the dependent variable. The INVITE's
//   X-Call-Status header is what the history's log entry displays; it
//   only follows the protocol state, never leads it. When the protocol
//   state of a session we OWN (we are the caller — the INVITE's author)
//   has moved on from its logged status, this hook reports the new UI
//   state with a chat-control amend, exactly like a file transfer's
//   sender amends its status message as acknowledgements arrive. Chat
//   control is a separate layer with its own echo discipline (the
//   receiver applies the amend on arrival, we apply our own via its
//   echo) and its own-messages-only rule — the SIP dialog itself never
//   echoes and never depends on chat control.
//
// Both layers are decoupled from the peer connection's own signalling
// state: the connection is usePeerSessions', shared with messaging and
// file transfer, and no dedicated connection is created for a call (the
// actual SDP offer/answer rides the SS's client-to-client relay between
// the two ends' PerfectNegotiators — hence the stripped SDP body here).

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import type { AudioGraph } from "@/api/audio/audiograph";
import {
  DC_MSG_MIME_SIP,
  DC_SIP_RESPONSE_DECLINE,
  DC_SIP_RESPONSE_OK,
  newChatControlDCMsg,
  newSipDCMsg,
  type DCCallKind,
  type DCCallStatus,
  type DCMsg,
  type DCMsgs,
  type DCSip,
} from "@/api/ss/datachannel";
import type { PeerSessions } from "@/api/ss/peersessions";
import type { ChatUser } from "@/api/ss/types";
import {
  conversationKey,
  parseConversationKey,
  type ActivePhoneCall,
  type ConversationRef,
} from "./types";

// statusEstablishedBy maps one message of the dialog onto the session
// state it establishes: the INVITE opens "inviting" (the fold's
// floor), the INVITE's final response answers it (200 OK → accepted,
// 603 Decline → rejected), a CANCEL aborts the ring, a BYE hangs an
// established call up.
function statusEstablishedBy(sip: DCSip): DCCallStatus {
  if (sip.response !== undefined) {
    return sip.response.code === DC_SIP_RESPONSE_OK.code
      ? "accepted"
      : "rejected";
  }
  switch (sip.method) {
    case "CANCEL":
      return "cancelled";
    case "BYE":
      return "ended";
    default:
      return "inviting"; // INVITE
  }
}

// statusPrecedence totally orders the session states so that folding a
// dialog's messages yields the same state on both ends regardless of
// arrival order (each end sees its own sends as echoes and the peer's
// sends directly): the fold is the precedence maximum. A no-call
// terminal (rejected/cancelled) therefore always settles a cancel/accept
// race the same way on both ends, and a terminal session is never
// revived.
function statusPrecedence(status: DCCallStatus): number {
  switch (status) {
    case "inviting":
      return 0;
    case "accepted":
      return 1;
    case "ended":
      return 2;
    case "cancelled":
      return 3;
    case "rejected":
      return 4;
  }
}

// PhoneSessionModel is one (channel, peer) pair's current phone session:
// the protocol state its dialog's messages fold into (status — it
// leads), plus the UI state the INVITE currently shows (loggedStatus —
// it follows, via this hook's chat-control amends).
interface PhoneSessionModel {
  ref: ConversationRef;
  // The INVITE message's id — the target the UI-state amends point at.
  messageId: string;
  // The dialog's identifier (the INVITE's Call-ID).
  callId: string;
  // What the session carries — voice only, or voice and video.
  kind: DCCallKind;
  status: DCCallStatus;
  loggedStatus: DCCallStatus;
  // true when the peer called us (we are the callee).
  incoming: boolean;
  // Unix seconds of the INVITE.
  since: number;
}

export interface UsePhoneCallsResult {
  /**
   * The live calls, keyed by conversation key: every conversation whose
   * current phone session is ringing ("inviting") or in call
   * ("accepted"), in the protocol state. Terminal sessions drop out of
   * the map and stay visible in the history as phone-call messages.
   */
  calls: Record<string, ActivePhoneCall>;
  /**
   * Rings the conversation's peer with a call of the given kind: sends
   * the INVITE. No-op without an own subscription or while a call
   * with the peer is live. Wakes the audio graph inside the click's
   * user gesture, so the caller's side can play and capture audio when
   * the callee later accepts without a gesture on this end.
   */
  startCall: (ref: ConversationRef, kind: DCCallKind) => void;
  /** Picks up an incoming call: answers the INVITE 200 OK. */
  acceptCall: (call: ActivePhoneCall) => void;
  /** Declines an incoming call: answers the INVITE 603 Decline. */
  rejectCall: (call: ActivePhoneCall) => void;
  /**
   * Hangs up a live call: sends a CANCEL while still ringing (the
   * caller), a BYE once accepted (either party).
   */
  hangupCall: (call: ActivePhoneCall) => void;
}

export function usePhoneCalls(
  me: ChatUser | null,
  dcMsgs: DCMsgs,
  sessions: PeerSessions,
  sendTo: (msg: DCMsg) => void,
  audio: AudioGraph | null,
): UsePhoneCallsResult {
  // Sessions force-ended locally: when a peer's session drops mid-call
  // there is nobody left to exchange the dialog with, so the session's
  // death is kept in this local overlay instead of on the wire. The
  // derivation treats overlaid sessions as terminated; the history's
  // call log entry keeps its last logged status.
  const [deadSessions, setDeadSessions] = useState<ReadonlySet<string>>(
    new Set(),
  );
  // The latest live calls, for the reset handler below.
  const callsRef = useRef<Record<string, ActivePhoneCall>>({});

  // A peer-session teardown force-ends its live calls locally.
  useEffect(() => {
    return sessions.subscribeReset((dropped) => {
      const dying = new Set<string>();
      for (const call of Object.values(callsRef.current)) {
        if (
          dropped !== null &&
          (call.ref.channelId !== dropped.channelId ||
            call.ref.userId !== dropped.peer)
        ) {
          continue;
        }
        dying.add(call.callId);
      }
      if (dying.size > 0) {
        setDeadSessions((prev) => new Set([...prev, ...dying]));
      }
    });
  }, [sessions]);

  // The session models: fold every conversation's SIP messages — both
  // parties' dialogs, invitations and answers alike — into the current
  // session's protocol state.
  const models = useMemo(() => {
    const self = me?.subscriberId;
    if (self === undefined) return {};
    // First collect each conversation's SIP messages. A conversation's
    // frames live in the peer's sender list (their sends) and in our
    // own (our sends, recorded locally at send time — SIP is never
    // echoed).
    const perConversation = new Map<string, DCMsg[]>();
    for (const [channelId, bySender] of Object.entries(dcMsgs)) {
      for (const list of Object.values(bySender)) {
        for (const m of list) {
          if (m.mimeType !== DC_MSG_MIME_SIP || m.sip === undefined) {
            continue;
          }
          // The conversation's peer is the frame's other end: its
          // sender, or its recipient for an echo of our own frame.
          const key = conversationKey({
            kind: "dm",
            channelId,
            userId: m.echo === true ? m.toSubscriberId : m.fromSubscriberId,
          });
          const bucket = perConversation.get(key) ?? [];
          bucket.push(m);
          perConversation.set(key, bucket);
        }
      }
    }
    const out: Record<string, PhoneSessionModel> = {};
    for (const [key, dialog] of perConversation) {
      // The current session is the latest INVITE's dialog; earlier
      // ones are terminated calls' log entries.
      let invite: DCMsg | null = null;
      for (const m of dialog) {
        if (m.sip?.method !== "INVITE") continue;
        if (invite === null || m.creationTimestamp > invite.creationTimestamp) {
          invite = m;
        }
      }
      if (invite === null || invite.sip === undefined) continue;
      const invitation = invite.sip;
      let status: DCCallStatus = "inviting";
      for (const m of dialog) {
        const sip = m.sip;
        if (sip === undefined || sip.callId !== invitation.callId) continue;
        const s = statusEstablishedBy(sip);
        if (statusPrecedence(s) > statusPrecedence(status)) {
          status = s;
        }
      }
      const ref = parseConversationKey(key);
      if (ref === null) continue;
      out[key] = {
        ref,
        messageId: invite.msgId,
        callId: invitation.callId,
        // X-Media stands in for the stripped SDP's m= lines: absent is
        // a voice call.
        kind: invitation["X-Media"] ?? "voice",
        status,
        loggedStatus: invitation["X-Call-Status"] ?? "inviting",
        incoming: invite.fromSubscriberId !== self,
        since: invite.creationTimestamp,
      };
    }
    return out;
  }, [dcMsgs, me?.subscriberId]);

  // The live calls: the models still ringing or in call, minus the
  // locally force-ended ones.
  const calls = useMemo(() => {
    const out: Record<string, ActivePhoneCall> = {};
    for (const [key, model] of Object.entries(models)) {
      if (model.status !== "inviting" && model.status !== "accepted") {
        continue;
      }
      if (deadSessions.has(model.callId)) continue;
      out[key] = {
        ref: model.ref,
        messageId: model.messageId,
        callId: model.callId,
        kind: model.kind,
        status: model.status,
        incoming: model.incoming,
        since: model.since,
      };
    }
    return out;
  }, [models, deadSessions]);
  useEffect(() => {
    callsRef.current = calls;
  }, [calls]);

  // The UI state follows the protocol state: for a session we own (we
  // are the caller — the INVITE's author), report the new status on the
  // INVITE message with a chat-control amend whenever the two have
  // drifted apart. Self-terminating: the amend's echo brings the logged
  // status up to date, ending the drift.
  useEffect(() => {
    const self = me?.subscriberId;
    if (self === undefined) return;
    for (const model of Object.values(models)) {
      if (model.incoming) continue;
      if (model.status === model.loggedStatus) continue;
      if (deadSessions.has(model.callId)) continue;
      sendTo(
        newChatControlDCMsg(model.ref.channelId, self, model.ref.userId, {
          subtype: "amend",
          targetMessageId: model.messageId,
          // The amend rewrites the INVITE's whole sip body, so the
          // X-Media header rides along to keep the log entry's kind.
          sip: {
            callId: model.callId,
            method: "INVITE",
            "X-Media": model.kind,
            "X-Call-Status": model.status,
          },
        }),
      );
    }
  }, [models, me?.subscriberId, sendTo, deadSessions]);

  const startCall = useCallback(
    (ref: ConversationRef, kind: DCCallKind) => {
      const self = me?.subscriberId;
      if (self === undefined) return;
      if (calls[conversationKey(ref)] !== undefined) return;
      // Inside the click's user gesture: wake the audio graph.
      audio?.resume();
      sendTo(
        newSipDCMsg(ref.channelId, self, ref.userId, {
          callId: crypto.randomUUID(),
          method: "INVITE",
          "X-Media": kind,
          "X-Call-Status": "inviting",
        }),
      );
    },
    [me?.subscriberId, calls, sendTo, audio],
  );

  // say sends one message of the call's dialog; the send itself folds
  // it into our own session state (SIP messages are never echoed).
  const say = useCallback(
    (call: ActivePhoneCall, startLine: Omit<DCSip, "callId">) => {
      const self = me?.subscriberId;
      if (self === undefined) return;
      sendTo(
        newSipDCMsg(call.ref.channelId, self, call.ref.userId, {
          callId: call.callId,
          ...startLine,
        }),
      );
    },
    [me?.subscriberId, sendTo],
  );

  const acceptCall = useCallback(
    (call: ActivePhoneCall) => {
      // Inside the click's user gesture: wake the audio graph.
      audio?.resume();
      say(call, { response: DC_SIP_RESPONSE_OK });
    },
    [say, audio],
  );
  const rejectCall = useCallback(
    (call: ActivePhoneCall) => say(call, { response: DC_SIP_RESPONSE_DECLINE }),
    [say],
  );
  const hangupCall = useCallback(
    (call: ActivePhoneCall) =>
      say(call, { method: call.status === "accepted" ? "BYE" : "CANCEL" }),
    [say],
  );

  return { calls, startCall, acceptCall, rejectCall, hangupCall };
}
