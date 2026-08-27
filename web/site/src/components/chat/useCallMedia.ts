"use client";

// useCallMedia wires the voice-call media to the peer sessions, reacting
// to the derived call state (usePhoneCalls): while a call says
// "accepted", the local mic track rides that pair's peer connection and
// the peer's incoming stream plays through the audio graph; any other
// state detaches both. Several calls can be accepted at once — the
// graph muxes their remote streams, and the one mic capture feeds every
// connection. The hook owns no session state: it is purely an effect of
// the wire-carried invitations, decoupled from the connections'
// signalling state.

import { useCallback, useEffect, useRef, useState } from "react";
import type { AudioGraph } from "@/api/audio/audiograph";
import type { PeerSessions } from "@/api/ss/peersessions";
import type { ChannelId, SubscriberId } from "@/api/ss/types";
import type { ActivePhoneCall } from "./types";

// callKey indexes one call's media attachments by its pair.
function callKey(channelId: ChannelId, peer: SubscriberId): string {
  return `${channelId}:${peer}`;
}

export interface UseCallMediaResult {
  /**
   * The local mic's FFT tap, or null until the first accepted call opens
   * the microphone. Render-safe: the hook re-renders its caller whenever
   * an attachment changes, so reads during render see analysers the
   * moment they exist.
   */
  localAnalyser: AnalyserNode | null;
  /** The FFT tap of one peer's incoming voice, while connected. */
  remoteAnalyserFor: (
    channelId: ChannelId,
    peer: SubscriberId,
  ) => AnalyserNode | null;
}

export function useCallMedia(
  sessions: PeerSessions,
  audio: AudioGraph | null,
  calls: Record<string, ActivePhoneCall>,
): UseCallMediaResult {
  // The RTCRtpSender of our mic track per attached pair.
  const sendersRef = useRef(new Map<string, RTCRtpSender>());
  // Bumped whenever an attachment changes, so render-time analyser reads
  // (localAnalyser, remoteAnalyserFor) see them the moment they exist.
  const [, setMediaVersion] = useState(0);

  // Remote streams: every inbound audio track of any session connects
  // into the audio graph under its pair key.
  useEffect(() => {
    if (audio === null) return;
    return sessions.subscribeTracks((channelId, peer, ev) => {
      if (ev.track.kind !== "audio") return;
      const stream = ev.streams[0] ?? new MediaStream([ev.track]);
      audio.addRemote(callKey(channelId, peer), stream);
      setMediaVersion((v) => v + 1);
    });
  }, [sessions, audio]);

  // The send-side attachments mirror the accepted calls: attach the mic
  // track to every newly accepted call's connection, detach from every
  // connection whose call left "accepted" (and drop its remote stream).
  useEffect(() => {
    if (audio === null) return;
    let live = true;
    const wanted = new Set(
      Object.values(calls)
        .filter((call) => call.status === "accepted")
        .map((call) => callKey(call.ref.channelId, call.ref.userId)),
    );
    for (const call of Object.values(calls)) {
      if (call.status !== "accepted") continue;
      const key = callKey(call.ref.channelId, call.ref.userId);
      if (sendersRef.current.has(key)) continue;
      void (async () => {
        try {
          const { track, stream } = await audio.ensureLocalInput();
          // The effect re-ran (or the call ended) while the mic opened.
          if (!live || sendersRef.current.has(key)) return;
          const sender = sessions.addTrack(
            call.ref.channelId,
            call.ref.userId,
            track,
            stream,
          );
          if (sender === null) return;
          sendersRef.current.set(key, sender);
          setMediaVersion((v) => v + 1);
        } catch (err) {
          // Permission denied, or no capture device: the call stays
          // accepted, we just send silence.
          console.error("callmedia: cannot open the microphone", err);
        }
      })();
    }
    for (const [key, sender] of [...sendersRef.current]) {
      if (wanted.has(key)) continue;
      const sep = key.indexOf(":");
      sessions.removeTrack(key.slice(0, sep), key.slice(sep + 1), sender);
      sendersRef.current.delete(key);
      audio.removeRemote(key);
      setMediaVersion((v) => v + 1);
    }
    return () => {
      live = false;
    };
  }, [sessions, audio, calls]);

  const remoteAnalyserFor = useCallback(
    (channelId: ChannelId, peer: SubscriberId) =>
      audio?.remoteAnalyser(callKey(channelId, peer)) ?? null,
    [audio],
  );

  return { localAnalyser: audio?.localAnalyser ?? null, remoteAnalyserFor };
}
