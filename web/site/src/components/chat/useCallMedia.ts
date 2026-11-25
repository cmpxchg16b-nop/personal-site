"use client";

// useCallMedia wires the call media to the peer sessions, reacting to
// the derived call state (usePhoneCalls): while a call says "accepted",
// the local mic track rides that pair's peer connection and the peer's
// incoming stream plays through the audio graph — video calls
// additionally attach the local camera and collect the peer's camera
// track for the floating video views. Any other state detaches both.
// Several calls can be accepted at once — the graph muxes their remote
// streams, and the one mic capture feeds every connection; the camera
// capture is shared the same way between video calls. Every attachment
// holds its capture (acquireLocalInput / the camera hold below) and its
// detach releases it, so the captures open with the first accepted call
// needing them and stop with the last — the browser's recording
// indicators light exactly while a call is sending. The mic senders the
// attachments create also answer the pairs' DTMF dialers (dtmfFor): the
// dial pad's existence is the negotiated telephone-event capability,
// read off the live senders. The hook owns no session state: it is
// purely an effect of the wire-carried invitations, decoupled from the
// connections' signalling state.

import { useCallback, useEffect, useRef, useState } from "react";
import type { AudioGraph } from "@/api/audio/audiograph";
import type { CameraController } from "@/api/media/camera";
import { dialerForSender, type DialerHandle } from "@/api/dtmf";
import type { PeerSessions } from "@/api/ss/peersessions";
import type { ChannelId, SubscriberId } from "@/api/ss/types";
import type { ActivePhoneCall } from "./types";

// callKey indexes one call's media attachments by its pair.
function callKey(channelId: ChannelId, peer: SubscriberId): string {
  return `${channelId}:${peer}`;
}

export interface UseCallMediaResult {
  /**
   * The local mic's FFT tap, or null while no accepted call holds the
   * microphone open. Render-safe: the hook re-renders its caller
   * whenever an attachment changes, so reads during render see analysers
   * the moment they exist.
   */
  localAnalyser: AnalyserNode | null;
  /** The FFT tap of one peer's incoming voice, while connected. */
  remoteAnalyserFor: (
    channelId: ChannelId,
    peer: SubscriberId,
  ) => AnalyserNode | null;
  /**
   * The local camera's stream while an accepted video call holds it —
   * the "me" view. Render-safe like localAnalyser.
   */
  localCamera: MediaStream | null;
  /**
   * The peer's camera stream of one pair while an accepted video call
   * carries it — the peer view. The stream holds the video track only;
   * the peer's audio flows through the audio graph.
   */
  remoteVideoFor: (
    channelId: ChannelId,
    peer: SubscriberId,
  ) => MediaStream | null;
  /**
   * The pair's DTMF dialer while its call negotiated telephone-event
   * (RFC 4733) — null otherwise (no attached mic, or the peer answered
   * the capability away). The dial pad's existence test; the pad's
   * visibility is the user's own toggle (the linphone model: capability
   * decides existence, the user decides showing). Render-safe like
   * localAnalyser.
   */
  dtmfFor: (channelId: ChannelId, peer: SubscriberId) => DialerHandle | null;
}

export function useCallMedia(
  sessions: PeerSessions,
  audio: AudioGraph | null,
  camera: CameraController | null,
  calls: Record<string, ActivePhoneCall>,
): UseCallMediaResult {
  // The RTCRtpSenders of our tracks per attached pair: the mic's, and
  // the camera's for video calls.
  const sendersRef = useRef(new Map<string, RTCRtpSender>());
  const cameraSendersRef = useRef(new Map<string, RTCRtpSender>());
  // CameraController owns the shared capture and reference counts. Its
  // subscription updates this render-facing stream and live video senders.
  const [localCamera, setLocalCamera] = useState<MediaStream | null>(
    () => camera?.localStream ?? null,
  );
  // The peers' camera streams per pair (video tracks off the wire).
  const remoteVideosRef = useRef(new Map<string, MediaStream>());
  // The pairs whose mic senders' first negotiation is being watched:
  // getParameters().codecs stays empty until the offer/answer completes,
  // and the DTMF capability read needs that moment (dtmfFor below).
  const dtmfWatchedRef = useRef(new Set<string>());
  // Bumped whenever an attachment changes, so render-time reads
  // (localAnalyser, localCamera, remoteAnalyserFor, remoteVideoFor) see
  // them the moment they exist.
  const [, setMediaVersion] = useState(0);

  useEffect(() => {
    if (camera === null) return;
    return camera.subscribe((stream) => {
      setLocalCamera(stream);
      const track = stream?.getVideoTracks()[0];
      if (track === undefined) return;
      for (const [key, sender] of [...cameraSendersRef.current]) {
        void sender.replaceTrack(track).catch((error: unknown) => {
          console.error("callmedia: cannot switch the camera", error);
          if (cameraSendersRef.current.get(key) !== sender) return;
          // Do not leave an accepted sender on the old, stopped track. Drop
          // this attachment and its hold; a later call-state change may retry.
          const separator = key.indexOf(":");
          sessions.removeTrack(
            key.slice(0, separator),
            key.slice(separator + 1),
            sender,
          );
          cameraSendersRef.current.delete(key);
          camera.release();
          setMediaVersion((version) => version + 1);
        });
      }
    });
  }, [camera, sessions]);

  // Remote streams: every inbound track of any session is routed by
  // kind — audio connects into the audio graph (the volume menu and the
  // FFT taps keep working during video calls), video is kept for the
  // peer view.
  useEffect(() => {
    return sessions.subscribeTracks((channelId, peer, ev) => {
      const key = callKey(channelId, peer);
      if (ev.track.kind === "video") {
        // Bind the bare track in a fresh stream, never ev.streams[0]:
        // stream-association semantics differ across browsers, and a
        // shared association stream can accumulate ended tracks (the
        // MDN "track ordering" note on MediaStreamAudioSourceNode).
        remoteVideosRef.current.set(key, new MediaStream([ev.track]));
        ev.track.addEventListener(
          "ended",
          () => {
            if (remoteVideosRef.current.delete(key)) {
              setMediaVersion((v) => v + 1);
            }
          },
          { once: true },
        );
        setMediaVersion((v) => v + 1);
        return;
      }
      if (ev.track.kind !== "audio" || audio === null) return;
      // Bind the bare track in a fresh stream, never ev.streams[0]:
      // stream-association semantics differ across browsers, and a
      // shared association stream can accumulate ended tracks whose
      // ordering decides which track the source node binds (the MDN
      // "track ordering" note on MediaStreamAudioSourceNode).
      audio.addRemote(key, new MediaStream([ev.track]));
      setMediaVersion((v) => v + 1);
    });
  }, [sessions, audio]);

  // The send-side attachments mirror the accepted calls: attach the mic
  // track to every newly accepted call's connection — and the camera
  // track for a video call — detach from every connection whose call
  // left "accepted" (dropping its remote streams and releasing its
  // capture holds — the mic stops with the last call, the camera with
  // the last video call).
  useEffect(() => {
    let live = true;
    const accepted = Object.values(calls).filter(
      (call) => call.status === "accepted",
    );
    const wanted = new Set(
      accepted.map((call) => callKey(call.ref.channelId, call.ref.userId)),
    );
    const wantedVideo = new Set(
      accepted
        .filter((call) => call.kind === "video")
        .map((call) => callKey(call.ref.channelId, call.ref.userId)),
    );
    if (audio !== null) {
      for (const call of accepted) {
        const key = callKey(call.ref.channelId, call.ref.userId);
        if (sendersRef.current.has(key)) continue;
        void (async () => {
          try {
            const { track, stream } = await audio.acquireLocalInput();
            // The effect re-ran (or the call ended) while the mic opened:
            // this hold has no attachment to back, so it goes back.
            if (!live || sendersRef.current.has(key)) {
              audio.releaseLocalInput();
              return;
            }
            const sender = sessions.addTrack(
              call.ref.channelId,
              call.ref.userId,
              track,
              stream,
            );
            if (sender === null) {
              audio.releaseLocalInput();
              return;
            }
            sendersRef.current.set(key, sender);
            setMediaVersion((v) => v + 1);
            // Watch the sender's first negotiation out: the DTMF
            // capability read (dtmfFor) needs the negotiated codec
            // list, which populates only once the offer/answer
            // completes — bounded, and abandoned if the attachment
            // went first.
            if (!dtmfWatchedRef.current.has(key)) {
              dtmfWatchedRef.current.add(key);
              void (async () => {
                for (
                  let i = 0;
                  i < 50 && sendersRef.current.get(key) === sender;
                  i++
                ) {
                  if (sender.getParameters().codecs.length > 0) break;
                  await new Promise((resolve) => setTimeout(resolve, 100));
                }
                if (sendersRef.current.get(key) === sender) {
                  setMediaVersion((v) => v + 1);
                }
              })();
            }
          } catch (err) {
            // Permission denied, or no capture device: the call stays
            // accepted, we just send silence.
            console.error("callmedia: cannot open the microphone", err);
          }
        })();
      }
    }
    for (const call of accepted) {
      if (call.kind !== "video") continue;
      const key = callKey(call.ref.channelId, call.ref.userId);
      if (cameraSendersRef.current.has(key)) continue;
      void (async () => {
        try {
          const stream = await camera?.acquire();
          if (stream === undefined) {
            throw new Error("callmedia: camera controller unavailable");
          }
          // The effect re-ran (or the call ended) while the camera
          // opened: this hold has no attachment to back, so it goes
          // back.
          if (!live || cameraSendersRef.current.has(key)) {
            camera?.release();
            return;
          }
          const sender = sessions.addTrack(
            call.ref.channelId,
            call.ref.userId,
            stream.getVideoTracks()[0],
            stream,
          );
          if (sender === null) {
            camera?.release();
            return;
          }
          cameraSendersRef.current.set(key, sender);
          setMediaVersion((v) => v + 1);
        } catch (err) {
          // Permission denied, or no camera: the call stays accepted,
          // we just send no video.
          console.error("callmedia: cannot open the camera", err);
        }
      })();
    }
    for (const [key, sender] of [...sendersRef.current]) {
      if (wanted.has(key) || audio === null) continue;
      const sep = key.indexOf(":");
      sessions.removeTrack(key.slice(0, sep), key.slice(sep + 1), sender);
      sendersRef.current.delete(key);
      dtmfWatchedRef.current.delete(key);
      audio.removeRemote(key);
      audio.releaseLocalInput();
      setMediaVersion((v) => v + 1);
    }
    for (const [key, sender] of [...cameraSendersRef.current]) {
      if (wantedVideo.has(key)) continue;
      const sep = key.indexOf(":");
      sessions.removeTrack(key.slice(0, sep), key.slice(sep + 1), sender);
      cameraSendersRef.current.delete(key);
      camera?.release();
      setMediaVersion((v) => v + 1);
    }
    // A peer view whose call left "accepted" closes; a live video track
    // would otherwise freeze on its last frame.
    for (const key of [...remoteVideosRef.current.keys()]) {
      if (wantedVideo.has(key)) continue;
      remoteVideosRef.current.delete(key);
      setMediaVersion((v) => v + 1);
    }
    return () => {
      live = false;
    };
  }, [sessions, audio, camera, calls]);

  const remoteAnalyserFor = useCallback(
    (channelId: ChannelId, peer: SubscriberId) =>
      audio?.remoteAnalyser(callKey(channelId, peer)) ?? null,
    [audio],
  );
  const remoteVideoFor = useCallback(
    (channelId: ChannelId, peer: SubscriberId) =>
      remoteVideosRef.current.get(callKey(channelId, peer)) ?? null,
    [],
  );

  // dtmfFor answers the pair's dialer while its mic sender can speak
  // RFC 4733: a dtmf object on the audio sender AND telephone-event
  // among the negotiated send codecs — the WebRTC API's own capability
  // verdict (docs/webrtc-sip-dtmf-consideration.md).
  const dtmfFor = useCallback(
    (channelId: ChannelId, peer: SubscriberId): DialerHandle | null => {
      const key = callKey(channelId, peer);
      return dialerForSender(() => {
        const retained = sendersRef.current.get(key);
        return retained === undefined
          ? null
          : sessions.resolveSender(channelId, peer, retained);
      });
    },
    [sessions],
  );

  return {
    localAnalyser: audio?.localAnalyser ?? null,
    remoteAnalyserFor,
    localCamera,
    remoteVideoFor,
    dtmfFor,
  };
}
