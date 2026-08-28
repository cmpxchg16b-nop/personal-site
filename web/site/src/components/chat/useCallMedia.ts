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
// indicators light exactly while a call is sending. The hook owns no
// session state: it is purely an effect of the wire-carried
// invitations, decoupled from the connections' signalling state.

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
}

export function useCallMedia(
  sessions: PeerSessions,
  audio: AudioGraph | null,
  calls: Record<string, ActivePhoneCall>,
): UseCallMediaResult {
  // The RTCRtpSenders of our tracks per attached pair: the mic's, and
  // the camera's for video calls.
  const sendersRef = useRef(new Map<string, RTCRtpSender>());
  const cameraSendersRef = useRef(new Map<string, RTCRtpSender>());
  // The one shared camera capture: cameraPromise makes the acquisition
  // idempotent (concurrent video calls share it), cameraHolds counts the
  // acquisitions — the capture stops when the last hold is released.
  // localCamera is the render-facing half (the "me" view); reading the
  // lifecycle ref during render is off-limits.
  const cameraPromiseRef = useRef<Promise<MediaStream> | null>(null);
  const cameraStreamRef = useRef<MediaStream | null>(null);
  const cameraHoldsRef = useRef(0);
  const [localCamera, setLocalCamera] = useState<MediaStream | null>(null);
  // The peers' camera streams per pair (video tracks off the wire).
  const remoteVideosRef = useRef(new Map<string, MediaStream>());
  // Bumped whenever an attachment changes, so render-time reads
  // (localAnalyser, localCamera, remoteAnalyserFor, remoteVideoFor) see
  // them the moment they exist.
  const [, setMediaVersion] = useState(0);

  // acquireCamera opens the camera, shared between concurrent video
  // calls — reference-counted like the audio graph's mic capture: every
  // acquisition takes a hold, balanced by releaseCamera, and the capture
  // stops with the last hold, so the camera indicator lights exactly
  // while a video call is sending. A rejected acquisition (permission
  // denied) takes no hold and may be retried.
  const acquireCamera = useCallback(async (): Promise<MediaStream> => {
    let promise = cameraPromiseRef.current;
    if (promise === null) {
      promise = navigator.mediaDevices.getUserMedia({ video: true });
      cameraPromiseRef.current = promise;
    }
    // The hold is taken before the await: acquisitions sharing one
    // pending getUserMedia are then all counted before any releaseCamera
    // can run.
    cameraHoldsRef.current += 1;
    try {
      const stream = await promise;
      if (cameraPromiseRef.current !== promise) {
        // The hook was released while the permission prompt sat open.
        for (const track of stream.getTracks()) track.stop();
        throw new Error("callmedia: released while opening the camera");
      }
      cameraStreamRef.current = stream;
      setLocalCamera(stream);
      return stream;
    } catch (err) {
      // Allow a retry (e.g. the user grants the permission later) —
      // unless a newer acquisition already replaced the promise — and
      // drop this acquisition's hold with the failure.
      if (cameraPromiseRef.current === promise) cameraPromiseRef.current = null;
      cameraHoldsRef.current -= 1;
      throw err;
    }
  }, []);

  // releaseCamera balances one acquireCamera hold; the last release
  // stops the capture. Acquisitions settle before their releases run
  // (the caller awaits before attaching), so the capture is never
  // pending here.
  const releaseCamera = useCallback(() => {
    if (cameraHoldsRef.current > 0) cameraHoldsRef.current -= 1;
    if (cameraHoldsRef.current > 0) return;
    const stream = cameraStreamRef.current;
    if (stream !== null) {
      for (const track of stream.getTracks()) track.stop();
    }
    cameraStreamRef.current = null;
    cameraPromiseRef.current = null;
    setLocalCamera(null);
  }, []);

  // Leaving the chat page drops the camera even mid-call (the mic goes
  // with the audio graph's own release).
  useEffect(() => {
    return () => {
      const stream = cameraStreamRef.current;
      if (stream !== null) {
        for (const track of stream.getTracks()) track.stop();
      }
      cameraStreamRef.current = null;
      cameraPromiseRef.current = null;
      cameraHoldsRef.current = 0;
    };
  }, []);

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
          const stream = await acquireCamera();
          // The effect re-ran (or the call ended) while the camera
          // opened: this hold has no attachment to back, so it goes
          // back.
          if (!live || cameraSendersRef.current.has(key)) {
            releaseCamera();
            return;
          }
          const sender = sessions.addTrack(
            call.ref.channelId,
            call.ref.userId,
            stream.getVideoTracks()[0],
            stream,
          );
          if (sender === null) {
            releaseCamera();
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
      audio.removeRemote(key);
      audio.releaseLocalInput();
      setMediaVersion((v) => v + 1);
    }
    for (const [key, sender] of [...cameraSendersRef.current]) {
      if (wantedVideo.has(key)) continue;
      const sep = key.indexOf(":");
      sessions.removeTrack(key.slice(0, sep), key.slice(sep + 1), sender);
      cameraSendersRef.current.delete(key);
      releaseCamera();
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
  }, [sessions, audio, calls, acquireCamera, releaseCamera]);

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

  return {
    localAnalyser: audio?.localAnalyser ?? null,
    remoteAnalyserFor,
    localCamera,
    remoteVideoFor,
  };
}
