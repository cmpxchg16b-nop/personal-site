"use client";

/**
 * The chat page's shared audio graph (see the MDN Web Audio API docs).
 *
 * AudioGraph owns the one and only AudioContext of the chat page — a
 * module-level singleton provided to the tree by AudioGraphProvider,
 * mirroring how the SSProxy singleton is provided (see api/ss/react.tsx).
 * Every audio path of the voice-call feature runs through it:
 *
 *   microphone ──► local gain ──┬──► local analyser (the local FFT)
 *              (getUserMedia)   └──► MediaStreamDestination ──► wire
 *                                (its track is what peer connections send)
 *
 *   remote stream ──► remote analyser (the remote FFT) ──► remote gain ──►
 *   (per peer, off the wire)                             (speaker volume)
 *   ctx.destination
 *
 * The local voice therefore passes the graph before it goes on the wire,
 * and every remote stream passes the graph before it hits the speaker.
 * Several remote streams connect to the one remote gain node at once —
 * the graph muxes them into what the user hears. One mic capture is
 * shared by every ongoing call: its processed track can be added to any
 * number of peer connections.
 *
 * Echo cancellation is optional and off by default (headphones are
 * assumed): the call audio menu's toggle switches the capture-side
 * voice processing (echo cancellation, noise suppression, auto gain)
 * on, live via applyConstraints and for future captures. The capture
 * itself is reference-counted (acquireLocalInput/releaseLocalInput): it
 * opens with the first attached call and stops with the last, so the
 * browser's recording indicator lights exactly while a call is sending.
 *
 * Browsers only let an AudioContext run after a user gesture: the context
 * is created/resumed by resume(), which the call UI invokes from its
 * click handlers (call, accept). Paths reached without a gesture (the
 * caller's side when the callee accepts) find the context already running
 * thanks to the earlier gesture.
 */

import { createContext, useContext, useEffect, useMemo } from "react";
import type { ReactNode } from "react";

// The analyser configuration of both FFT taps: 2048-sample FFT (1024
// frequency bins) with heavy temporal smoothing — the spectrum component
// downsamples the bins and smooths spatially on top.
const FFT_SIZE = 2048;
const FFT_SMOOTHING = 0.8;

// One connected remote stream: its graph nodes, kept for teardown.
interface RemoteEntry {
  source: MediaStreamAudioSourceNode;
  analyser: AnalyserNode;
}

export class AudioGraph {
  private ctx: AudioContext | null = null;

  // The local chain, built by acquireLocalInput. localInputPromise makes
  // the build idempotent — concurrent acquisitions share the one mic
  // capture — and localInputHolds counts the acquisitions: the capture
  // stops when the last hold is released (releaseLocalInput).
  private localStream: MediaStream | null = null;
  private localSourceNode: MediaStreamAudioSourceNode | null = null;
  private localGainNode: GainNode | null = null;
  private localAnalyserNode: AnalyserNode | null = null;
  private localDestination: MediaStreamAudioDestinationNode | null = null;
  private localInputPromise: Promise<{
    track: MediaStreamTrack;
    stream: MediaStream;
  }> | null = null;
  private localInputHolds = 0;

  // The remote chain: per-remote sources muxed into one gain node feeding
  // the speaker.
  private remoteGainNode: GainNode | null = null;
  private remotes = new Map<string, RemoteEntry>();

  // The user-facing settings, kept as plain fields so nodes (and
  // captures) built later pick them up; the setters also adjust the
  // live ones. echoCancellation gates the capture-side voice processing
  // (echo cancellation, noise suppression, auto gain) — off by default.
  private localVolume = 1;
  private remoteVolume = 1;
  private echoCancellation = false;

  // The capture constraints of the current echo-cancellation setting.
  // Stated explicitly: browsers turn the voice processing ON when the
  // constraints are merely omitted, so "off" must say false.
  private captureConstraints(): MediaTrackConstraints {
    return {
      echoCancellation: this.echoCancellation,
      noiseSuppression: this.echoCancellation,
      autoGainControl: this.echoCancellation,
    };
  }

  /**
   * Creates the AudioContext on first use and resumes it when suspended.
   * Call from a user gesture (a click handler): without one the browser
   * may keep the context suspended and every path stays silent until the
   * next resume().
   */
  resume(): void {
    if (this.ctx === null) {
      this.ctx = new AudioContext();
      this.remoteGainNode = this.ctx.createGain();
      this.remoteGainNode.gain.value = this.remoteVolume;
      this.remoteGainNode.connect(this.ctx.destination);
    }
    if (this.ctx.state === "suspended") {
      void this.ctx.resume();
    }
  }

  /**
   * Opens the microphone and builds the local chain, returning the
   * processed track (and its stream, for addTrack's stream association)
   * to put on the wire. Idempotent: concurrent and repeated acquisitions
   * share the one capture. Every acquisition takes a hold and must be
   * balanced by releaseLocalInput — the capture stops with the last
   * hold. A rejected acquisition (permission denied) takes no hold and
   * may be retried.
   */
  async acquireLocalInput(): Promise<{
    track: MediaStreamTrack;
    stream: MediaStream;
  }> {
    this.resume();
    let promise = this.localInputPromise;
    if (promise === null) {
      const ctx = this.ctx;
      if (ctx === null) {
        // resume() just created the context; this is unreachable.
        throw new Error("audiograph: no audio context");
      }
      promise = (async () => {
        const stream = await navigator.mediaDevices.getUserMedia({
          audio: this.captureConstraints(),
        });
        if (this.ctx !== ctx) {
          // The graph was released while the permission prompt sat open.
          for (const track of stream.getTracks()) track.stop();
          throw new Error("audiograph: released while opening the mic");
        }
        const source = ctx.createMediaStreamSource(stream);
        const gain = ctx.createGain();
        gain.gain.value = this.localVolume;
        const analyser = ctx.createAnalyser();
        analyser.fftSize = FFT_SIZE;
        analyser.smoothingTimeConstant = FFT_SMOOTHING;
        const destination = ctx.createMediaStreamDestination();
        source.connect(gain);
        gain.connect(analyser);
        gain.connect(destination);
        this.localStream = stream;
        this.localSourceNode = source;
        this.localGainNode = gain;
        this.localAnalyserNode = analyser;
        this.localDestination = destination;
        const track = destination.stream.getAudioTracks()[0];
        return { track, stream: destination.stream };
      })();
      this.localInputPromise = promise;
    }
    // The hold is taken before the await: acquisitions sharing one
    // pending getUserMedia are then all counted before any
    // releaseLocalInput can run.
    this.localInputHolds += 1;
    try {
      return await promise;
    } catch (err) {
      // Allow a retry (e.g. the user grants the permission later) —
      // unless a newer acquisition already replaced the promise — and
      // drop this acquisition's hold with the failure.
      if (this.localInputPromise === promise) this.localInputPromise = null;
      this.localInputHolds -= 1;
      throw err;
    }
  }

  /**
   * Balances one acquireLocalInput hold. When the last hold goes — every
   * call using the mic has ended — the capture is stopped (the browser's
   * recording indicator goes out) and the local chain is torn down; the
   * next acquisition opens a fresh capture. Acquisitions settle before
   * their releases run (the caller awaits before attaching), so the
   * capture is never pending here.
   */
  releaseLocalInput(): void {
    if (this.localInputHolds > 0) this.localInputHolds -= 1;
    if (this.localInputHolds > 0 || this.localInputPromise === null) return;
    if (this.localStream !== null) {
      for (const track of this.localStream.getTracks()) track.stop();
    }
    this.localSourceNode?.disconnect();
    this.localGainNode?.disconnect();
    this.localAnalyserNode?.disconnect();
    this.localStream = null;
    this.localSourceNode = null;
    this.localGainNode = null;
    this.localAnalyserNode = null;
    this.localDestination = null;
    this.localInputPromise = null;
  }

  /** The local FFT tap, or null until the mic chain is built. */
  get localAnalyser(): AnalyserNode | null {
    return this.localAnalyserNode;
  }

  getLocalVolume(): number {
    return this.localVolume;
  }

  /** Sets the mic send volume (0..1) heard by every peer. */
  setLocalVolume(volume: number): void {
    this.localVolume = volume;
    if (this.localGainNode !== null) {
      this.localGainNode.gain.value = volume;
    }
  }

  getRemoteVolume(): number {
    return this.remoteVolume;
  }

  /** Sets the speaker volume (0..1) of all remote voices together. */
  setRemoteVolume(volume: number): void {
    this.remoteVolume = volume;
    if (this.remoteGainNode !== null) {
      this.remoteGainNode.gain.value = volume;
    }
  }

  getEchoCancellation(): boolean {
    return this.echoCancellation;
  }

  /**
   * Toggles the capture-side voice processing (echo cancellation, noise
   * suppression, auto gain). Applies live to the open mic, if any, and
   * to every future capture.
   */
  setEchoCancellation(on: boolean): void {
    this.echoCancellation = on;
    const track = this.localStream?.getAudioTracks()[0];
    if (track !== undefined) {
      // Best-effort: a browser refusing the change keeps the old
      // processing; the setting still stands for the next capture.
      void track
        .applyConstraints(this.captureConstraints())
        .catch((err) =>
          console.error("audiograph: cannot apply echo cancellation", err),
        );
    }
  }

  /**
   * Connects a remote stream (a peer's voice, off the wire) into the
   * graph under `id`: source → analyser → the shared remote gain →
   * speakers. An `id` already connected is left alone. The entry tears
   * itself down when the stream's audio track ends.
   */
  addRemote(id: string, stream: MediaStream): void {
    if (this.remotes.has(id)) return;
    this.resume();
    const ctx = this.ctx;
    if (ctx === null || this.remoteGainNode === null) return;
    const source = ctx.createMediaStreamSource(stream);
    const analyser = ctx.createAnalyser();
    analyser.fftSize = FFT_SIZE;
    analyser.smoothingTimeConstant = FFT_SMOOTHING;
    source.connect(analyser);
    source.connect(this.remoteGainNode);
    this.remotes.set(id, { source, analyser });
    stream
      .getAudioTracks()[0]
      ?.addEventListener("ended", () => this.removeRemote(id), { once: true });
  }

  /** Disconnects the remote stream connected under `id`, if any. */
  removeRemote(id: string): void {
    const entry = this.remotes.get(id);
    if (entry === undefined) return;
    this.remotes.delete(id);
    entry.source.disconnect();
    entry.analyser.disconnect();
  }

  /** The FFT tap of the remote stream connected under `id`, if any. */
  remoteAnalyser(id: string): AnalyserNode | null {
    return this.remotes.get(id)?.analyser ?? null;
  }

  /**
   * Tears the whole graph down: the mic capture is stopped (the browser's
   * recording indicator goes out), every remote is disconnected, and the
   * context is closed. The graph is usable again afterwards — the next
   * resume() builds a fresh context.
   */
  release(): void {
    for (const id of [...this.remotes.keys()]) {
      this.removeRemote(id);
    }
    if (this.localStream !== null) {
      for (const track of this.localStream.getTracks()) track.stop();
    }
    this.localStream = null;
    this.localSourceNode = null;
    this.localGainNode = null;
    this.localAnalyserNode = null;
    this.localDestination = null;
    this.localInputPromise = null;
    this.localInputHolds = 0;
    if (this.ctx !== null) {
      void this.ctx.close();
      this.ctx = null;
      this.remoteGainNode = null;
    }
  }
}

// theGraph is the module-level singleton, created lazily by
// getAudioGraph. Like the SSProxy singleton it survives leaving the chat
// segment's React tree only until the provider unmounts — release() then
// drops the mic and the context, and a return mounts a fresh one.
let theGraph: AudioGraph | null = null;

function getAudioGraph(): AudioGraph {
  theGraph ??= new AudioGraph();
  return theGraph;
}

// AudioGraphContext carries the AudioGraph singleton, or null above the
// provider.
const AudioGraphContext = createContext<AudioGraph | null>(null);

/**
 * Provides the AudioGraph singleton to the tree. Mounting creates no
 * AudioContext yet — the context appears with the first resume(), a user
 * gesture — and unmounting releases the graph, so leaving the chat
 * segment never leaves the mic open.
 */
export function AudioGraphProvider({ children }: { children: ReactNode }) {
  const graph = useMemo(() => getAudioGraph(), []);
  useEffect(() => {
    return () => {
      graph.release();
      if (theGraph === graph) theGraph = null;
    };
  }, [graph]);
  return (
    <AudioGraphContext.Provider value={graph}>
      {children}
    </AudioGraphContext.Provider>
  );
}

// useAudioGraph reads the AudioGraph provided by AudioGraphProvider, or
// null when called above it.
export function useAudioGraph(): AudioGraph | null {
  return useContext(AudioGraphContext);
}
