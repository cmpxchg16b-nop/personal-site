"use client";

import { useCallback, useEffect, useMemo, useRef, useState } from "react";

import type { AudioGraph } from "@/api/audio/audiograph";
import { useICEServers } from "@/hooks/useICEServers";
import { dialerForSender, type DialerHandle } from "../dtmf";
import { useSSProxy } from "./react";
import type { SSProxy } from "./proxy";
import {
  WELLKNOWN_SVC_ID_SIP,
  type SIPCallAction,
  type SIPCallDirection,
  type SIPCallEvent,
  type SIPCallKind,
  type SIPCallState,
  type SIPMessageEvent,
  type SIPMessageState,
  type SignallingEvent,
} from "./types";

const TERMINAL_CALL_STATES: ReadonlySet<SIPCallState> = new Set([
  "ended",
  "cancelled",
  "rejected",
  "failed",
]);
const CALL_PROGRESS: Record<
  Exclude<SIPCallState, "ended" | "cancelled" | "rejected" | "failed">,
  number
> = {
  dialing: 0,
  ringing: 1,
  answering: 2,
  accepted: 3,
};

export type TerminalSIPCallState =
  "ended" | "cancelled" | "rejected" | "failed";

export function isTerminalSIPCallState(
  state: SIPCallState,
): state is TerminalSIPCallState {
  return TERMINAL_CALL_STATES.has(state);
}

export function isExternalSIPCallActive(call: ExternalSIPCall): boolean {
  return !isTerminalSIPCallState(call.state);
}

export interface ExternalSIPCall {
  callId: string;
  credentialId?: string;
  target?: string;
  kind: SIPCallKind;
  direction: SIPCallDirection;
  state: SIPCallState;
  localUri?: string;
  remoteUri?: string;
  reason?: string;
  /** Local wall-clock milliseconds, used only for display ordering. */
  since: number;
  updatedAt: number;
}

export type ExternalSIPMessageState = SIPMessageState | "sending";

export interface ExternalSIPMessage {
  messageId: string;
  credentialId?: string;
  target?: string;
  text: string;
  direction: SIPCallDirection;
  state: ExternalSIPMessageState;
  localUri?: string;
  remoteUri?: string;
  reason?: string;
  /** Unix seconds. */
  createdAt: number;
}

export interface ExternalSIPMedia {
  localStream: MediaStream | null;
  remoteStream: MediaStream | null;
}

export interface ExternalSIPState {
  calls: Record<string, ExternalSIPCall>;
  messages: ExternalSIPMessage[];
}

export interface ExternalSIPSnapshot extends ExternalSIPState {
  media: Record<string, ExternalSIPMedia>;
}

export interface ExternalSIPDialInput {
  credentialId: string;
  target: string;
  kind: SIPCallKind;
}

export interface ExternalSIPMessageInput {
  credentialId: string;
  target: string;
  text: string;
}

const EMPTY_STATE: ExternalSIPState = { calls: {}, messages: [] };
export const EMPTY_EXTERNAL_SIP_SNAPSHOT: ExternalSIPSnapshot = {
  calls: {},
  messages: [],
  media: {},
};

function progressedCallState(
  previous: SIPCallState | undefined,
  requested: SIPCallState,
): SIPCallState {
  if (
    previous === undefined ||
    isTerminalSIPCallState(previous) ||
    isTerminalSIPCallState(requested)
  ) {
    return requested;
  }
  return CALL_PROGRESS[requested] < CALL_PROGRESS[previous]
    ? previous
    : requested;
}

/** Fold one authoritative server event into the in-page external call history. */
export function reduceSIPCallEvent(
  state: ExternalSIPState,
  event: SIPCallEvent,
  now = Date.now(),
): ExternalSIPState {
  if (event.callId === "") return state;
  const previous = state.calls[event.callId];
  if (previous !== undefined && isTerminalSIPCallState(previous.state)) {
    return state;
  }
  const direction =
    event.direction ?? previous?.direction ?? ("incoming" as const);
  const requestedState = event.state ?? previous?.state ?? ("ringing" as const);
  const call: ExternalSIPCall = {
    callId: event.callId,
    credentialId: event.credentialId ?? previous?.credentialId,
    target: event.target ?? previous?.target,
    kind: event.kind ?? previous?.kind ?? "voice",
    direction,
    state: progressedCallState(previous?.state, requestedState),
    localUri: event.localUri ?? previous?.localUri,
    remoteUri: event.remoteUri ?? previous?.remoteUri,
    reason:
      event.reason !== undefined
        ? conciseSIPReason(event.reason, "")
        : previous?.reason,
    since: previous?.since ?? now,
    updatedAt: now,
  };
  return {
    ...state,
    calls: { ...state.calls, [event.callId]: call },
  };
}

/** Fold a send result or incoming MESSAGE into the persistent page history. */
export function reduceSIPMessageEvent(
  state: ExternalSIPState,
  event: SIPMessageEvent,
  now = Date.now(),
): ExternalSIPState {
  if (event.messageId === "") return state;
  const index = state.messages.findIndex(
    (message) => message.messageId === event.messageId,
  );
  const previous = index < 0 ? undefined : state.messages[index];
  const direction =
    event.direction ??
    previous?.direction ??
    (event.state === "received" ? "incoming" : "outgoing");
  const message: ExternalSIPMessage = {
    messageId: event.messageId,
    credentialId: event.credentialId ?? previous?.credentialId,
    target: event.target ?? previous?.target,
    text: event.text ?? previous?.text ?? "",
    direction,
    state:
      event.state ??
      previous?.state ??
      (direction === "incoming" ? "received" : "sent"),
    localUri: event.localUri ?? previous?.localUri,
    remoteUri: event.remoteUri ?? previous?.remoteUri,
    reason:
      event.reason !== undefined
        ? conciseSIPReason(event.reason, "")
        : previous?.reason,
    createdAt: event.createdAt ?? previous?.createdAt ?? now / 1000,
  };
  const messages = [...state.messages];
  if (index < 0) messages.push(message);
  else messages[index] = message;
  return { ...state, messages };
}

export function conciseSIPReason(error: unknown, fallback: string): string {
  const value =
    typeof error === "string" && error.trim() !== ""
      ? error
      : error instanceof Error && error.message.trim() !== ""
        ? error.message
        : fallback;
  return value.replace(/\s+/g, " ").trim().slice(0, 160);
}

export interface ExternalSIPTransport {
  getReadStream(): ReadableStream<SignallingEvent>;
  getWriteStream(): WritableStream<SignallingEvent>;
}

export interface ExternalSIPClientOptions {
  transport: ExternalSIPTransport;
  audio?: AudioGraph | null;
  iceServers?: RTCIceServer[];
  createPeerConnection?: (configuration: RTCConfiguration) => RTCPeerConnection;
  randomUUID?: () => string;
  now?: () => number;
}

interface CallRuntime {
  readonly callId: string;
  readonly pc: RTCPeerConnection;
  readonly audioSender: RTCRtpSender;
  readonly videoSender: RTCRtpSender | null;
  readonly pendingRemoteCandidates: RTCIceCandidateInit[];
  readonly pendingLocalCandidates: RTCIceCandidateInit[];
  remoteDescriptionKey: string | null;
  offerCancelled: boolean;
  offerCommandInFlight: boolean;
  offerSent: boolean;
  closed: boolean;
  mediaPromise: Promise<void> | null;
  audioInputHeld: boolean;
  readonly ownedLocalTracks: MediaStreamTrack[];
  localStream: MediaStream | null;
  remoteStream: MediaStream | null;
  remoteAudio: HTMLAudioElement | null;
}

function messageForSendFailure(error: unknown): string {
  return conciseSIPReason(error, "Signalling send failed");
}

function failureAbortAction(call: ExternalSIPCall): SIPCallAction {
  if (call.direction === "incoming") {
    return call.state === "accepted" ? "hangup" : "decline";
  }
  return call.state === "dialing" || call.state === "ringing"
    ? "cancel"
    : "hangup";
}

/**
 * Browser adapter for the external SIP service. It emits only c2s SIP-service
 * events; SIP itself remains entirely on the server. One instance belongs to
 * one SSProxy incarnation and owns all of its peer connections and media.
 */
export class ExternalSIPClient {
  private readonly transport: ExternalSIPTransport;
  private readonly audio: AudioGraph | null;
  private readonly createPeerConnection: (
    configuration: RTCConfiguration,
  ) => RTCPeerConnection;
  private readonly randomUUID: () => string;
  private readonly now: () => number;
  private iceServers: RTCIceServer[];
  private state: ExternalSIPState = EMPTY_STATE;
  private snapshot: ExternalSIPSnapshot = EMPTY_EXTERNAL_SIP_SNAPSHOT;
  private readonly runtimes = new Map<string, CallRuntime>();
  private readonly listeners = new Set<
    (snapshot: ExternalSIPSnapshot) => void
  >();
  private reader: ReadableStreamDefaultReader<SignallingEvent> | null = null;
  private writeTail: Promise<void> = Promise.resolve();
  private started = false;
  private closed = false;

  constructor(options: ExternalSIPClientOptions) {
    this.transport = options.transport;
    this.audio = options.audio ?? null;
    this.iceServers = [...(options.iceServers ?? [])];
    this.createPeerConnection =
      options.createPeerConnection ??
      ((configuration) => new RTCPeerConnection(configuration));
    this.randomUUID = options.randomUUID ?? (() => crypto.randomUUID());
    this.now = options.now ?? (() => Date.now());
  }

  setICEServers(iceServers: RTCIceServer[]): void {
    // Existing calls retain the configuration with which they were created.
    this.iceServers = [...iceServers];
  }

  getSnapshot(): ExternalSIPSnapshot {
    return this.snapshot;
  }

  subscribe(listener: (snapshot: ExternalSIPSnapshot) => void): () => void {
    this.listeners.add(listener);
    listener(this.snapshot);
    return () => this.listeners.delete(listener);
  }

  start(): void {
    if (this.started || this.closed) return;
    this.started = true;
    void this.readEvents();
  }

  private async readEvents(): Promise<void> {
    const reader = this.transport.getReadStream().getReader();
    this.reader = reader;
    try {
      for (;;) {
        const { done, value } = await reader.read();
        if (done) break;
        await this.handleEvent(value);
      }
    } catch {
      // A destroyed proxy also closes the stream; both paths converge here.
    } finally {
      if (this.reader === reader) this.reader = null;
      reader.releaseLock();
    }
    if (!this.closed) this.disconnect();
  }

  async handleEvent(event: SignallingEvent): Promise<void> {
    if (this.closed || event.from?.serviceId !== WELLKNOWN_SVC_ID_SIP) return;
    const call = event.s2CEv?.sipCall;
    if (call !== undefined) await this.handleCallEvent(call);
    const message = event.s2CEv?.sipMessage;
    if (message !== undefined) {
      this.state = reduceSIPMessageEvent(this.state, message, this.now());
      this.publish();
    }
  }

  private publish(): void {
    const media: Record<string, ExternalSIPMedia> = {};
    for (const [callId, runtime] of this.runtimes) {
      media[callId] = {
        localStream: runtime.localStream,
        remoteStream: runtime.remoteStream,
      };
    }
    this.snapshot = { ...this.state, media };
    for (const listener of this.listeners) listener(this.snapshot);
  }

  private send(event: SignallingEvent): Promise<void> {
    if (this.closed) {
      return Promise.reject(new Error("External SIP client is closed"));
    }
    const write = this.writeTail.then(async () => {
      if (this.closed) throw new Error("External SIP client is closed");
      const writer = this.transport.getWriteStream().getWriter();
      try {
        await writer.write(event);
      } finally {
        writer.releaseLock();
      }
    });
    // A failed write rejects its own caller without poisoning later writes.
    this.writeTail = write.catch(() => {});
    return write;
  }

  private sendCall(call: SIPCallEvent): Promise<void> {
    return this.send({
      from: {},
      to: { serviceId: WELLKNOWN_SVC_ID_SIP },
      msgId: this.randomUUID(),
      c2SEv: { sipCall: call },
    });
  }

  private sendMessageEvent(message: SIPMessageEvent): Promise<void> {
    return this.send({
      from: {},
      to: { serviceId: WELLKNOWN_SVC_ID_SIP },
      msgId: this.randomUUID(),
      c2SEv: { sipMessage: message },
    });
  }

  private patchCall(
    callId: string,
    patch: Partial<ExternalSIPCall>,
  ): ExternalSIPCall | null {
    const previous = this.state.calls[callId];
    if (previous === undefined) return null;
    const call = { ...previous, ...patch, updatedAt: this.now() };
    this.state = {
      ...this.state,
      calls: { ...this.state.calls, [callId]: call },
    };
    this.publish();
    return call;
  }

  private createRuntime(call: ExternalSIPCall): CallRuntime {
    const existing = this.runtimes.get(call.callId);
    if (existing !== undefined) return existing;
    const pc = this.createPeerConnection({ iceServers: this.iceServers });
    const audioTransceiver = pc.addTransceiver("audio", {
      direction: "sendrecv",
    });
    const videoTransceiver =
      call.kind === "video"
        ? pc.addTransceiver("video", { direction: "sendrecv" })
        : null;
    const runtime: CallRuntime = {
      callId: call.callId,
      pc,
      audioSender: audioTransceiver.sender,
      videoSender: videoTransceiver?.sender ?? null,
      pendingRemoteCandidates: [],
      pendingLocalCandidates: [],
      remoteDescriptionKey: null,
      offerCancelled: false,
      offerCommandInFlight: false,
      offerSent: false,
      closed: false,
      mediaPromise: null,
      audioInputHeld: false,
      ownedLocalTracks: [],
      localStream: null,
      remoteStream: null,
      remoteAudio: null,
    };
    this.runtimes.set(call.callId, runtime);

    pc.onicecandidate = ({ candidate }) => {
      const currentCall = this.state.calls[call.callId];
      if (
        runtime.closed ||
        currentCall === undefined ||
        isTerminalSIPCallState(currentCall.state)
      ) {
        return;
      }
      const wireCandidate: RTCIceCandidateInit = candidate ?? {
        candidate: "",
      };
      if (!runtime.offerSent) {
        runtime.pendingLocalCandidates.push(wireCandidate);
        return;
      }
      void this.sendCandidate(runtime, wireCandidate).catch((error) => {
        if (!runtime.closed) {
          void this.failCall(call.callId, messageForSendFailure(error)).catch(
            () => {},
          );
        }
      });
    };
    pc.ontrack = (event) => {
      if (runtime.closed) return;
      let stream = event.streams[0] ?? runtime.remoteStream;
      if (stream === null || stream === undefined) {
        stream = new MediaStream();
      }
      if (
        event.streams.length === 0 &&
        !stream.getTracks().some((track) => track === event.track)
      ) {
        stream.addTrack(event.track);
      }
      runtime.remoteStream = stream;
      if (this.state.calls[call.callId]?.state === "accepted") {
        this.connectRemoteAudio(runtime);
      }
      this.publish();
    };
    pc.onconnectionstatechange = () => {
      if (!runtime.closed && pc.connectionState === "failed") {
        void this.failCall(call.callId, "Media connection failed").catch(
          () => {},
        );
      }
    };
    return runtime;
  }

  private offerIsBlocked(runtime: CallRuntime): boolean {
    const call = this.state.calls[runtime.callId];
    return (
      runtime.offerCancelled ||
      runtime.closed ||
      call === undefined ||
      isTerminalSIPCallState(call.state)
    );
  }

  private async createAndSendOffer(
    runtime: CallRuntime,
    call: ExternalSIPCall,
    action: "dial" | "accept",
  ): Promise<void> {
    if (this.offerIsBlocked(runtime)) return;
    const offer = await runtime.pc.createOffer();
    if (this.offerIsBlocked(runtime)) return;
    await runtime.pc.setLocalDescription(offer);
    if (this.offerIsBlocked(runtime)) return;
    const local = runtime.pc.localDescription;
    if (local === null) throw new Error("Browser produced no local SDP offer");
    runtime.offerCommandInFlight = true;
    try {
      await this.sendCall({
        callId: call.callId,
        action,
        credentialId: call.credentialId,
        target: call.target,
        kind: call.kind,
        sessionDesc: { type: local.type, sdp: local.sdp },
      });
      runtime.offerSent = true;
      if (this.offerIsBlocked(runtime)) return;
    } finally {
      runtime.offerCommandInFlight = false;
    }
    const pending = runtime.pendingLocalCandidates.splice(0);
    for (const candidate of pending) {
      await this.sendCandidate(runtime, candidate);
    }
  }

  private sendCandidate(
    runtime: CallRuntime,
    candidate: RTCIceCandidateInit,
  ): Promise<void> {
    const call = this.state.calls[runtime.callId];
    if (
      runtime.closed ||
      call === undefined ||
      isTerminalSIPCallState(call.state)
    ) {
      return Promise.resolve();
    }
    return this.sendCall({
      callId: runtime.callId,
      action: "ice",
      rtcICECandidate: candidate,
    });
  }

  async dial(input: ExternalSIPDialInput): Promise<string | null> {
    const credentialId = input.credentialId.trim();
    const target = input.target.trim();
    if (this.closed || credentialId === "" || target === "") return null;
    this.audio?.resume();
    const callId = this.randomUUID();
    const now = this.now();
    const call: ExternalSIPCall = {
      callId,
      credentialId,
      target,
      kind: input.kind,
      direction: "outgoing",
      state: "dialing",
      since: now,
      updatedAt: now,
    };
    this.state = {
      ...this.state,
      calls: { ...this.state.calls, [callId]: call },
    };
    this.publish();
    try {
      const runtime = this.createRuntime(call);
      await this.createAndSendOffer(runtime, call, "dial");
    } catch (error) {
      await this.failCall(
        callId,
        conciseSIPReason(error, "Unable to start call"),
      );
    }
    return callId;
  }

  async accept(callId: string): Promise<void> {
    const call = this.state.calls[callId];
    if (
      this.closed ||
      call === undefined ||
      call.direction !== "incoming" ||
      isTerminalSIPCallState(call.state) ||
      call.state === "answering" ||
      call.state === "accepted"
    ) {
      return;
    }
    this.audio?.resume();
    this.patchCall(callId, { state: "answering" });
    try {
      const runtime = this.createRuntime(call);
      await this.createAndSendOffer(
        runtime,
        { ...call, state: "answering" },
        "accept",
      );
    } catch (error) {
      await this.failCall(
        callId,
        conciseSIPReason(error, "Unable to accept call"),
      );
    }
  }

  /** The accepted call's DTMF dialer, while telephone-event is negotiated. */
  dtmfFor(callId: string): DialerHandle | null {
    return dialerForSender(() => {
      const runtime = this.runtimes.get(callId);
      return runtime !== undefined && this.runtimeIsLive(runtime)
        ? runtime.audioSender
        : null;
    });
  }

  private async control(callId: string, action: SIPCallAction): Promise<void> {
    const call = this.state.calls[callId];
    if (
      this.closed ||
      call === undefined ||
      isTerminalSIPCallState(call.state)
    ) {
      return;
    }
    const runtime = this.runtimes.get(callId);
    if (
      runtime !== undefined &&
      !runtime.offerSent &&
      (action === "cancel" || action === "decline")
    ) {
      runtime.offerCancelled = true;
      if (call.direction === "outgoing" && !runtime.offerCommandInFlight) {
        this.patchCall(callId, { state: "cancelled" });
        this.cleanupRuntime(runtime);
        this.publish();
        return;
      }
    }
    try {
      await this.sendCall({ callId, action });
    } catch (error) {
      // The failed control write was already the best-effort abort attempt.
      await this.failCall(callId, messageForSendFailure(error), false);
    }
  }

  async decline(callId: string): Promise<void> {
    await this.control(callId, "decline");
  }

  async end(callId: string): Promise<void> {
    const call = this.state.calls[callId];
    if (call === undefined) return;
    const action: SIPCallAction =
      call.direction === "incoming" && call.state !== "accepted"
        ? "decline"
        : call.direction === "outgoing" &&
            (call.state === "dialing" || call.state === "ringing")
          ? "cancel"
          : "hangup";
    await this.control(callId, action);
  }

  async sendMessage(input: ExternalSIPMessageInput): Promise<string | null> {
    const credentialId = input.credentialId.trim();
    const target = input.target.trim();
    const text = input.text.trim();
    if (this.closed || credentialId === "" || target === "" || text === "") {
      return null;
    }
    const messageId = this.randomUUID();
    const local: ExternalSIPMessage = {
      messageId,
      credentialId,
      target,
      text,
      direction: "outgoing",
      state: "sending",
      createdAt: this.now() / 1000,
    };
    this.state = { ...this.state, messages: [...this.state.messages, local] };
    this.publish();
    try {
      await this.sendMessageEvent({
        messageId,
        credentialId,
        target,
        text,
        action: "send",
      });
    } catch (error) {
      const index = this.state.messages.findIndex(
        (message) => message.messageId === messageId,
      );
      if (index >= 0) {
        const messages = [...this.state.messages];
        messages[index] = {
          ...messages[index],
          state: "failed",
          reason: messageForSendFailure(error),
        };
        this.state = { ...this.state, messages };
        this.publish();
      }
    }
    return messageId;
  }

  private async handleCallEvent(event: SIPCallEvent): Promise<void> {
    const previous = this.state.calls[event.callId];
    if (previous !== undefined && isTerminalSIPCallState(previous.state)) {
      return;
    }
    this.state = reduceSIPCallEvent(this.state, event, this.now());
    const call = this.state.calls[event.callId];
    if (call === undefined) return;
    this.publish();
    if (isTerminalSIPCallState(call.state)) {
      const runtime = this.runtimes.get(call.callId);
      if (runtime !== undefined) this.cleanupRuntime(runtime);
      this.publish();
      return;
    }

    const runtime = this.runtimes.get(call.callId);
    if (runtime !== undefined) {
      try {
        if (event.sessionDesc !== undefined) {
          await this.applyRemoteDescription(runtime, event.sessionDesc);
          // Negotiated sender parameters now include the answered codecs.
          this.publish();
        }
        if (event.rtcICECandidate !== undefined) {
          await this.applyRemoteCandidate(runtime, event.rtcICECandidate);
        }
      } catch (error) {
        await this.failCall(
          call.callId,
          conciseSIPReason(error, "WebRTC negotiation failed"),
        );
        return;
      }
      if (call.state === "accepted") {
        this.connectRemoteAudio(runtime);
        this.startMedia(runtime);
      }
    }
  }

  private async applyRemoteDescription(
    runtime: CallRuntime,
    description: RTCSessionDescriptionInit,
  ): Promise<void> {
    if (runtime.closed) return;
    const key = JSON.stringify(description);
    if (runtime.remoteDescriptionKey === key) return;
    await runtime.pc.setRemoteDescription(description);
    runtime.remoteDescriptionKey = key;
    const candidates = runtime.pendingRemoteCandidates.splice(0);
    for (const candidate of candidates) {
      if (runtime.closed) return;
      await runtime.pc.addIceCandidate(candidate);
    }
  }

  private async applyRemoteCandidate(
    runtime: CallRuntime,
    candidate: RTCIceCandidateInit,
  ): Promise<void> {
    if (runtime.closed) return;
    if (runtime.remoteDescriptionKey === null) {
      runtime.pendingRemoteCandidates.push(candidate);
      return;
    }
    await runtime.pc.addIceCandidate(candidate);
  }

  private startMedia(runtime: CallRuntime): void {
    if (runtime.closed || runtime.mediaPromise !== null) return;
    const promise = this.captureAndAttachMedia(runtime);
    runtime.mediaPromise = promise;
    void promise.catch((error: unknown) => {
      const call = this.state.calls[runtime.callId];
      if (
        runtime.closed ||
        call === undefined ||
        isTerminalSIPCallState(call.state)
      ) {
        return;
      }
      void this.failCall(
        runtime.callId,
        conciseSIPReason(error, "Unable to acquire call media"),
      ).catch(() => {});
    });
  }

  private runtimeIsLive(runtime: CallRuntime): boolean {
    return (
      !runtime.closed && this.state.calls[runtime.callId]?.state === "accepted"
    );
  }

  private async captureAndAttachMedia(runtime: CallRuntime): Promise<void> {
    if (this.audio !== null) {
      const input = await this.audio.acquireLocalInput();
      runtime.audioInputHeld = true;
      if (!this.runtimeIsLive(runtime)) {
        this.releaseAudioInput(runtime);
        return;
      }
      await runtime.audioSender.replaceTrack(input.track);
    } else {
      const stream = await navigator.mediaDevices.getUserMedia({ audio: true });
      runtime.ownedLocalTracks.push(...stream.getTracks());
      if (!this.runtimeIsLive(runtime)) {
        this.stopOwnedTracks(runtime);
        return;
      }
      const track = stream.getAudioTracks()[0];
      if (track === undefined) throw new Error("No microphone track available");
      await runtime.audioSender.replaceTrack(track);
    }

    if (runtime.videoSender !== null) {
      const stream = await navigator.mediaDevices.getUserMedia({ video: true });
      runtime.ownedLocalTracks.push(...stream.getTracks());
      if (!this.runtimeIsLive(runtime)) {
        this.stopOwnedTracks(runtime);
        return;
      }
      const track = stream.getVideoTracks()[0];
      if (track === undefined) throw new Error("No camera track available");
      await runtime.videoSender.replaceTrack(track);
      runtime.localStream = stream;
    }
    if (!this.runtimeIsLive(runtime)) return;
    this.connectRemoteAudio(runtime);
    this.publish();
  }

  private connectRemoteAudio(runtime: CallRuntime): void {
    const stream = runtime.remoteStream;
    if (
      runtime.closed ||
      stream === null ||
      stream.getAudioTracks().length === 0
    ) {
      return;
    }
    if (this.audio !== null) {
      this.audio.addRemote(`external-sip:${runtime.callId}`, stream);
      return;
    }
    if (runtime.remoteAudio !== null || typeof document === "undefined") return;
    const element = document.createElement("audio");
    element.srcObject = stream;
    element.autoplay = true;
    runtime.remoteAudio = element;
    void element.play().catch(() => {});
  }

  private releaseAudioInput(runtime: CallRuntime): void {
    if (!runtime.audioInputHeld) return;
    runtime.audioInputHeld = false;
    this.audio?.releaseLocalInput();
  }

  private stopOwnedTracks(runtime: CallRuntime): void {
    for (const track of runtime.ownedLocalTracks.splice(0)) track.stop();
    runtime.localStream = null;
  }

  private cleanupRuntime(runtime: CallRuntime): void {
    if (runtime.closed) return;
    runtime.closed = true;
    this.runtimes.delete(runtime.callId);
    this.audio?.removeRemote(`external-sip:${runtime.callId}`);
    if (runtime.remoteAudio !== null) {
      runtime.remoteAudio.pause();
      runtime.remoteAudio.srcObject = null;
      runtime.remoteAudio = null;
    }
    this.releaseAudioInput(runtime);
    this.stopOwnedTracks(runtime);
    runtime.pendingLocalCandidates.length = 0;
    runtime.pendingRemoteCandidates.length = 0;
    runtime.pc.onicecandidate = null;
    runtime.pc.ontrack = null;
    runtime.pc.onconnectionstatechange = null;
    try {
      runtime.pc.close();
    } catch {
      // Some browser implementations throw when closing an already-closed PC.
    }
  }

  private bestEffortAbort(
    call: ExternalSIPCall,
    runtime: CallRuntime,
  ): Promise<void> {
    if (!runtime.offerSent || runtime.offerCancelled || runtime.closed) {
      return Promise.resolve();
    }
    return this.sendCall({
      callId: call.callId,
      action: failureAbortAction(call),
    }).catch(() => {
      // The transport is already failing; local cleanup must still proceed.
    });
  }

  private async failCall(
    callId: string,
    reason: string,
    notifyServer = true,
  ): Promise<void> {
    const call = this.state.calls[callId];
    if (call === undefined || isTerminalSIPCallState(call.state)) return;
    const runtime = this.runtimes.get(callId);
    this.state = {
      ...this.state,
      calls: {
        ...this.state.calls,
        [callId]: {
          ...call,
          state: "failed",
          reason: conciseSIPReason(reason, "Call failed"),
          updatedAt: this.now(),
        },
      },
    };
    // Mark the terminal state before awaiting so concurrent failures cannot
    // emit duplicate aborts. The abort itself must precede media cleanup.
    this.publish();
    if (notifyServer && runtime !== undefined) {
      await this.bestEffortAbort(call, runtime);
    }
    if (runtime !== undefined) this.cleanupRuntime(runtime);
    this.publish();
  }

  private disconnect(): void {
    if (this.closed) return;
    const calls = { ...this.state.calls };
    for (const [callId, call] of Object.entries(calls)) {
      if (!isTerminalSIPCallState(call.state)) {
        calls[callId] = {
          ...call,
          state: "failed",
          reason: "Signalling disconnected",
          updatedAt: this.now(),
        };
      }
    }
    this.state = { ...this.state, calls };
    this.closed = true;
    for (const runtime of [...this.runtimes.values()]) {
      this.cleanupRuntime(runtime);
    }
    this.publish();
  }

  /** Stop every media path and peer connection without emitting more events. */
  dispose(): void {
    if (this.closed) return;
    this.closed = true;
    if (this.reader !== null) void this.reader.cancel().catch(() => {});
    for (const runtime of [...this.runtimes.values()]) {
      this.cleanupRuntime(runtime);
    }
    this.listeners.clear();
  }
}

const NO_ICE_SERVER_URLS: string[] = [];

export interface UseExternalSIPResult extends ExternalSIPSnapshot {
  dial: (input: ExternalSIPDialInput) => Promise<string | null>;
  accept: (callId: string) => Promise<void>;
  decline: (callId: string) => Promise<void>;
  end: (callId: string) => Promise<void>;
  dtmfFor: (callId: string) => DialerHandle | null;
  sendMessage: (input: ExternalSIPMessageInput) => Promise<string | null>;
}

/** React binding: one client per proxy incarnation, torn down on unmount. */
export function useExternalSIP(
  audio: AudioGraph | null = null,
): UseExternalSIPResult {
  const proxy: SSProxy | null = useSSProxy();
  const iceQuery = useICEServers();
  const iceServerURLs = iceQuery.data ?? NO_ICE_SERVER_URLS;
  const iceServers = useMemo<RTCIceServer[]>(
    () => (iceServerURLs.length === 0 ? [] : [{ urls: iceServerURLs }]),
    [iceServerURLs],
  );
  const iceServersRef = useRef(iceServers);
  const clientRef = useRef<ExternalSIPClient | null>(null);
  const [snapshotState, setSnapshotState] = useState<{
    proxy: SSProxy | null;
    value: ExternalSIPSnapshot;
  }>({ proxy: null, value: EMPTY_EXTERNAL_SIP_SNAPSHOT });

  useEffect(() => {
    iceServersRef.current = iceServers;
    clientRef.current?.setICEServers(iceServers);
  }, [iceServers]);

  useEffect(() => {
    if (proxy === null) {
      clientRef.current = null;
      return;
    }
    const client = new ExternalSIPClient({
      transport: proxy,
      audio,
      iceServers: iceServersRef.current,
    });
    clientRef.current = client;
    const unsubscribe = client.subscribe((value) =>
      setSnapshotState({ proxy, value }),
    );
    client.start();
    return () => {
      unsubscribe();
      if (clientRef.current === client) clientRef.current = null;
      client.dispose();
    };
  }, [proxy, audio]);

  const dial = useCallback(
    (input: ExternalSIPDialInput) =>
      clientRef.current?.dial(input) ?? Promise.resolve(null),
    [],
  );
  const accept = useCallback(
    (callId: string) => clientRef.current?.accept(callId) ?? Promise.resolve(),
    [],
  );
  const decline = useCallback(
    (callId: string) => clientRef.current?.decline(callId) ?? Promise.resolve(),
    [],
  );
  const end = useCallback(
    (callId: string) => clientRef.current?.end(callId) ?? Promise.resolve(),
    [],
  );
  const dtmfFor = useCallback(
    (callId: string) => clientRef.current?.dtmfFor(callId) ?? null,
    [],
  );
  const sendMessage = useCallback(
    (input: ExternalSIPMessageInput) =>
      clientRef.current?.sendMessage(input) ?? Promise.resolve(null),
    [],
  );

  const snapshot =
    snapshotState.proxy === proxy
      ? snapshotState.value
      : EMPTY_EXTERNAL_SIP_SNAPSHOT;
  return { ...snapshot, dial, accept, decline, end, dtmfFor, sendMessage };
}
