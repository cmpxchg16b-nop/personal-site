"use client";

// usePhoneCalls manages the phone (voice / video call) sessions, one
// per (channel, peer subscriber) pair. Two deliberately separate layers:
//
// - The session PROTOCOL — the cause. Actions are their own wire
//   frames: the caller's invitation (an application/x-phone-session
//   DCMsg, also stored as the call's log entry) and the session events
//   (application/x-phone-session-event DCMsgs: accept / reject / cancel
//   / end). Each end folds the session's frames into its state below;
//   media (useCallMedia) and the live indicators (the answer popup, the
//   sidebar pills, the conversation strip) read this state.
//
// - The session's UI STATE — the dependent variable. The invitation
//   message's stored status is what the history's log entry displays;
//   it only follows the protocol state, never leads it. When the
//   protocol state of a session we OWN (we are the caller — the
//   invitation's author) has moved on from its logged status, this hook
//   reports the new UI state with a chat-control amend, exactly like a
//   file transfer's sender amends its status message as
//   acknowledgements arrive. The receiver applies the amend on arrival,
//   we apply our own via its echo, and chat control keeps its
//   own-messages-only rule.
//
// Both layers are decoupled from the peer connection's own signalling
// state: the connection is usePeerSessions', shared with messaging and
// file transfer, and no dedicated connection is created for a call.

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import type { AudioGraph } from "@/api/audio/audiograph";
import {
  DC_MSG_MIME_PHONE_SESSION,
  DC_MSG_MIME_PHONE_SESSION_EVENT,
  newChatControlDCMsg,
  newPhoneSessionDCMsg,
  newPhoneSessionEventDCMsg,
  type DCMsg,
  type DCMsgs,
  type DCPhoneSessionAction,
  type DCPhoneSessionKind,
  type DCPhoneSessionStatus,
} from "@/api/ss/datachannel";
import type { PeerSessions } from "@/api/ss/peersessions";
import type { ChatUser } from "@/api/ss/types";
import {
  conversationKey,
  parseConversationKey,
  type ActivePhoneCall,
  type ConversationRef,
} from "./types";

// statusOfAction maps a session protocol action onto the session state
// it establishes.
function statusOfAction(action: DCPhoneSessionAction): DCPhoneSessionStatus {
  switch (action) {
    case "accept":
      return "accepted";
    case "reject":
      return "rejected";
    case "cancel":
      return "cancelled";
    case "end":
      return "ended";
  }
}

// statusPrecedence totally orders the session states so that folding a
// session's frames yields the same state on both ends regardless of
// arrival order (each end sees its own sends as echoes and the peer's
// sends directly): the fold is the precedence maximum. A no-call
// terminal (rejected/cancelled) therefore always settles a cancel/accept
// race the same way on both ends, and a terminal session is never
// revived.
function statusPrecedence(status: DCPhoneSessionStatus): number {
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
// the protocol state its frames fold into (status — it leads), plus the
// UI state the invitation message currently shows (loggedStatus — it
// follows, via this hook's chat-control amends).
interface PhoneSessionModel {
  ref: ConversationRef;
  // The invitation message's id — the target the UI-state amends point
  // at.
  messageId: string;
  sessionId: string;
  // What the session carries — voice only, or voice and video.
  kind: DCPhoneSessionKind;
  status: DCPhoneSessionStatus;
  loggedStatus: DCPhoneSessionStatus;
  // true when the peer called us (we are the callee).
  incoming: boolean;
  // Unix seconds of the invitation.
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
   * the invitation. No-op without an own subscription or while a call
   * with the peer is live. Wakes the audio graph inside the click's
   * user gesture, so the caller's side can play and capture audio when
   * the callee later accepts without a gesture on this end.
   */
  startCall: (ref: ConversationRef, kind: DCPhoneSessionKind) => void;
  /** Picks up an incoming call: sends the accept event. */
  acceptCall: (call: ActivePhoneCall) => void;
  /** Declines an incoming call: sends the reject event. */
  rejectCall: (call: ActivePhoneCall) => void;
  /**
   * Hangs up a live call: sends the cancel event while still ringing
   * (the caller), the end event once accepted (either party).
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
  // there is nobody left to exchange the protocol with, so the
  // session's death is kept in this local overlay instead of on the
  // wire. The derivation treats overlaid sessions as terminated; the
  // history's call log entry keeps its last logged status.
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
        dying.add(call.sessionId);
      }
      if (dying.size > 0) {
        setDeadSessions((prev) => new Set([...prev, ...dying]));
      }
    });
  }, [sessions]);

  // The session models: fold every conversation's phone frames — the
  // invitations and the session events of both parties — into the
  // current session's protocol state.
  const models = useMemo(() => {
    const self = me?.subscriberId;
    if (self === undefined) return {};
    // First collect each conversation's invitations and events. A
    // conversation's frames live in the peer's sender list (their
    // sends) and in our own (the echoes of our sends).
    const perConversation = new Map<
      string,
      { invitations: DCMsg[]; events: DCMsg[] }
    >();
    for (const [channelId, bySender] of Object.entries(dcMsgs)) {
      for (const list of Object.values(bySender)) {
        for (const m of list) {
          const isInvitation =
            m.mimeType === DC_MSG_MIME_PHONE_SESSION &&
            m.phoneSession !== undefined;
          const isEvent =
            m.mimeType === DC_MSG_MIME_PHONE_SESSION_EVENT &&
            m.phoneSessionEvent !== undefined;
          if (!isInvitation && !isEvent) continue;
          // The conversation's peer is the frame's other end: its
          // sender, or its recipient for an echo of our own frame.
          const key = conversationKey({
            kind: "dm",
            channelId,
            userId: m.echo === true ? m.toSubscriberId : m.fromSubscriberId,
          });
          const bucket = perConversation.get(key) ?? {
            invitations: [],
            events: [],
          };
          (isInvitation ? bucket.invitations : bucket.events).push(m);
          perConversation.set(key, bucket);
        }
      }
    }
    const out: Record<string, PhoneSessionModel> = {};
    for (const [key, { invitations, events }] of perConversation) {
      // The current session is the latest invitation's; earlier ones
      // are terminated calls' log entries.
      let invitation: DCMsg | null = null;
      for (const m of invitations) {
        if (
          invitation === null ||
          m.creationTimestamp > invitation.creationTimestamp
        ) {
          invitation = m;
        }
      }
      if (invitation === null) continue;
      const session = invitation.phoneSession;
      if (session === undefined) continue;
      let status: DCPhoneSessionStatus = "inviting";
      for (const m of events) {
        const ev = m.phoneSessionEvent;
        if (ev === undefined || ev.sessionId !== session.sessionId) continue;
        const s = statusOfAction(ev.action);
        if (statusPrecedence(s) > statusPrecedence(status)) {
          status = s;
        }
      }
      const ref = parseConversationKey(key);
      if (ref === null) continue;
      out[key] = {
        ref,
        messageId: invitation.msgId,
        sessionId: session.sessionId,
        // The kind field postdates the invitation: absent is a voice
        // call.
        kind: session.kind ?? "voice",
        status,
        loggedStatus: session.status,
        incoming: invitation.fromSubscriberId !== self,
        since: invitation.creationTimestamp,
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
      if (deadSessions.has(model.sessionId)) continue;
      out[key] = {
        ref: model.ref,
        messageId: model.messageId,
        sessionId: model.sessionId,
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
  // are the caller — the invitation's author), report the new status on
  // the invitation message with a chat-control amend whenever the two
  // have drifted apart. Self-terminating: the amend's echo brings the
  // logged status up to date, ending the drift.
  useEffect(() => {
    const self = me?.subscriberId;
    if (self === undefined) return;
    for (const model of Object.values(models)) {
      if (model.incoming) continue;
      if (model.status === model.loggedStatus) continue;
      if (deadSessions.has(model.sessionId)) continue;
      sendTo(
        newChatControlDCMsg(model.ref.channelId, self, model.ref.userId, {
          subtype: "amend",
          targetMessageId: model.messageId,
          // The amend rewrites the whole phoneSession body, so the kind
          // rides along to keep the log entry's kind.
          phoneSession: {
            sessionId: model.sessionId,
            status: model.status,
            kind: model.kind,
          },
        }),
      );
    }
  }, [models, me?.subscriberId, sendTo, deadSessions]);

  const startCall = useCallback(
    (ref: ConversationRef, kind: DCPhoneSessionKind) => {
      const self = me?.subscriberId;
      if (self === undefined) return;
      if (calls[conversationKey(ref)] !== undefined) return;
      // Inside the click's user gesture: wake the audio graph.
      audio?.resume();
      sendTo(
        newPhoneSessionDCMsg(
          ref.channelId,
          self,
          ref.userId,
          crypto.randomUUID(),
          kind,
        ),
      );
    },
    [me?.subscriberId, calls, sendTo, audio],
  );

  // act sends one session protocol event; its echo folds it into our
  // own session state.
  const act = useCallback(
    (call: ActivePhoneCall, action: DCPhoneSessionAction) => {
      const self = me?.subscriberId;
      if (self === undefined) return;
      sendTo(
        newPhoneSessionEventDCMsg(call.ref.channelId, self, call.ref.userId, {
          sessionId: call.sessionId,
          action,
        }),
      );
    },
    [me?.subscriberId, sendTo],
  );

  const acceptCall = useCallback(
    (call: ActivePhoneCall) => {
      // Inside the click's user gesture: wake the audio graph.
      audio?.resume();
      act(call, "accept");
    },
    [act, audio],
  );
  const rejectCall = useCallback(
    (call: ActivePhoneCall) => act(call, "reject"),
    [act],
  );
  const hangupCall = useCallback(
    (call: ActivePhoneCall) =>
      act(call, call.status === "accepted" ? "end" : "cancel"),
    [act],
  );

  return { calls, startCall, acceptCall, rejectCall, hangupCall };
}
