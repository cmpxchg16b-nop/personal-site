import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const mocks = vi.hoisted(() => ({
  runtime: { current: null as unknown },
  proxy: null as unknown,
  iceServerURLs: [] as string[],
}));

vi.mock("react", () => ({
  useCallback: <T>(callback: T, deps: readonly unknown[]) =>
    (mocks.runtime.current as HookRuntime).memo(() => callback, deps),
  useMemo: <T>(factory: () => T, deps: readonly unknown[]) =>
    (mocks.runtime.current as HookRuntime).memo(factory, deps),
  useEffect: (effect: Effect, deps: readonly unknown[]) =>
    (mocks.runtime.current as HookRuntime).effect(effect, deps),
  useRef: <T>(initial: T) =>
    (mocks.runtime.current as HookRuntime).ref(initial),
  useState: <T>(initial: T) =>
    (mocks.runtime.current as HookRuntime).state(initial),
}));

vi.mock("./react", () => ({ useSSProxy: () => mocks.proxy }));
vi.mock("@/hooks/useICEServers", () => ({
  useICEServers: () => ({ data: mocks.iceServerURLs }),
}));

import type { AudioGraph } from "@/api/audio/audiograph";
import {
  ExternalSIPClient,
  reduceSIPCallEvent,
  reduceSIPMessageEvent,
  useExternalSIP,
  type ExternalSIPState,
  type UseExternalSIPResult,
} from "./sip";
import {
  WELLKNOWN_SVC_ID_SIP,
  type SIPCallEvent,
  type SIPMessageEvent,
  type SignallingEvent,
  type SSToClientEv,
} from "./types";

class MockTransport {
  readonly sent: SignallingEvent[] = [];
  readonly attempted: SignallingEvent[] = [];
  failAllWrites = false;
  private readonly writeFailures: Error[] = [];
  readonly write = vi.fn(async (event: SignallingEvent) => {
    this.attempted.push(event);
    const failure = this.writeFailures.shift();
    if (this.failAllWrites || failure !== undefined) {
      throw failure ?? new Error("signalling write failed");
    }
    this.sent.push(event);
  });
  private controller!: ReadableStreamDefaultController<SignallingEvent>;
  private readonly stream = new ReadableStream<SignallingEvent>({
    start: (controller) => {
      this.controller = controller;
    },
  });
  private closed = false;

  getReadStream(): ReadableStream<SignallingEvent> {
    return this.stream;
  }

  getWriteStream(): WritableStream<SignallingEvent> {
    return new WritableStream<SignallingEvent>({
      write: (event) => this.write(event),
    });
  }

  failNextWrites(count = 1): void {
    for (let index = 0; index < count; index++) {
      this.writeFailures.push(new Error("signalling write failed"));
    }
  }

  receive(event: SignallingEvent): void {
    if (!this.closed) this.controller.enqueue(event);
  }

  close(): void {
    if (this.closed) return;
    this.closed = true;
    this.controller.close();
  }
}

class MockSender {
  track: MediaStreamTrack | null = null;
  dtmf: {
    canInsertDTMF: boolean;
    insertDTMF: ReturnType<typeof vi.fn>;
  } | null = null;
  codecs: RTCRtpCodecParameters[] = [];
  readonly getParameters = vi.fn(
    () => ({ codecs: this.codecs }) as RTCRtpParameters,
  );
  readonly replaceTrack = vi.fn(async (track: MediaStreamTrack | null) => {
    this.track = track;
  });
}

class MockPeerConnection {
  static instances: MockPeerConnection[] = [];

  readonly transceivers: { kind: string; sender: MockSender }[] = [];
  localDescription: RTCSessionDescription | null = null;
  connectionState: RTCPeerConnectionState = "new";
  onicecandidate: ((event: RTCPeerConnectionIceEvent) => void) | null = null;
  ontrack: ((event: RTCTrackEvent) => void) | null = null;
  onconnectionstatechange: ((event: Event) => void) | null = null;
  readonly createOffer = vi.fn(
    async (): Promise<RTCSessionDescriptionInit> => ({
      type: "offer",
      sdp: "browser-offer",
    }),
  );
  readonly setLocalDescription = vi.fn(
    async (description: RTCSessionDescriptionInit) => {
      this.localDescription = description as unknown as RTCSessionDescription;
    },
  );
  readonly setRemoteDescription = vi.fn(async () => {});
  readonly addIceCandidate = vi.fn(async () => {});
  readonly close = vi.fn(() => {
    this.connectionState = "closed";
  });

  constructor(readonly configuration: RTCConfiguration = {}) {
    MockPeerConnection.instances.push(this);
  }

  addTransceiver(kind: string): { sender: MockSender } {
    const sender = new MockSender();
    this.transceivers.push({ kind, sender });
    return { sender };
  }

  sender(kind: string): MockSender {
    const sender = this.transceivers.find((item) => item.kind === kind)?.sender;
    if (sender === undefined) throw new Error(`missing ${kind} sender`);
    return sender;
  }

  emitCandidate(candidate: RTCIceCandidateInit | null): void {
    this.onicecandidate?.({ candidate } as RTCPeerConnectionIceEvent);
  }

  emitTrack(track: MediaStreamTrack, streams: MediaStream[]): void {
    this.ontrack?.({ track, streams } as unknown as RTCTrackEvent);
  }
}

class MockMediaStream {
  constructor(private readonly tracks: MediaStreamTrack[] = []) {}

  getTracks(): MediaStreamTrack[] {
    return [...this.tracks];
  }

  getAudioTracks(): MediaStreamTrack[] {
    return this.tracks.filter((track) => track.kind === "audio");
  }

  getVideoTracks(): MediaStreamTrack[] {
    return this.tracks.filter((track) => track.kind === "video");
  }

  addTrack(track: MediaStreamTrack): void {
    if (!this.tracks.includes(track)) this.tracks.push(track);
  }
}

function mediaTrack(kind: "audio" | "video"): MediaStreamTrack {
  return {
    kind,
    stop: vi.fn(),
    addEventListener: vi.fn(),
  } as unknown as MediaStreamTrack;
}

function mediaStream(...tracks: MediaStreamTrack[]): MediaStream {
  return new MockMediaStream(tracks) as unknown as MediaStream;
}

function serverEvent(s2CEv: SSToClientEv): SignallingEvent {
  return {
    from: { serviceId: WELLKNOWN_SVC_ID_SIP },
    to: { userId: "user", userSessionId: "session" },
    msgId: "server-message",
    s2CEv,
  };
}

function serverCall(call: SIPCallEvent): SignallingEvent {
  return serverEvent({ sipCall: call });
}

function serverMessage(message: SIPMessageEvent): SignallingEvent {
  return serverEvent({ sipMessage: message });
}

async function flushMicrotasks(): Promise<void> {
  for (let index = 0; index < 50; index++) await Promise.resolve();
}

function deferred<T>() {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((resolvePromise) => {
    resolve = resolvePromise;
  });
  return { promise, resolve };
}

interface Setup {
  client: ExternalSIPClient;
  transport: MockTransport;
  audio: {
    resume: ReturnType<typeof vi.fn>;
    acquireLocalInput: ReturnType<typeof vi.fn>;
    releaseLocalInput: ReturnType<typeof vi.fn>;
    addRemote: ReturnType<typeof vi.fn>;
    removeRemote: ReturnType<typeof vi.fn>;
  };
  microphone: MediaStreamTrack;
}

interface SetupOptions {
  configurePeer?: (peer: MockPeerConnection) => void;
}

const clients: ExternalSIPClient[] = [];

function setup(options: SetupOptions = {}): Setup {
  const transport = new MockTransport();
  const microphone = mediaTrack("audio");
  const audio = {
    resume: vi.fn(),
    acquireLocalInput: vi.fn(async () => ({
      track: microphone,
      stream: mediaStream(microphone),
    })),
    releaseLocalInput: vi.fn(),
    addRemote: vi.fn(),
    removeRemote: vi.fn(),
  };
  let sequence = 0;
  const client = new ExternalSIPClient({
    transport,
    audio: audio as unknown as AudioGraph,
    createPeerConnection: () => {
      const peer = new MockPeerConnection();
      options.configurePeer?.(peer);
      return peer as unknown as RTCPeerConnection;
    },
    randomUUID: () => `uuid-${++sequence}`,
    now: () => 10_000,
  });
  clients.push(client);
  return { client, transport, audio, microphone };
}

function sentCall(transport: MockTransport, index: number): SIPCallEvent {
  const event = transport.sent[index]?.c2SEv?.sipCall;
  if (event === undefined) throw new Error(`missing sent call ${index}`);
  return event;
}

function attemptedCallActions(transport: MockTransport): string[] {
  return transport.attempted.map((event) => {
    const action = event.c2SEv?.sipCall?.action;
    if (action === undefined) throw new Error("missing attempted call action");
    return action;
  });
}

type Effect = () => void | (() => void);
type Cleanup = void | (() => void);

interface HookSlot {
  value?: unknown;
  deps?: readonly unknown[];
  effect?: Effect;
  cleanup?: Cleanup;
  setter?: (next: unknown) => void;
}

function sameDeps(a: readonly unknown[] | undefined, b: readonly unknown[]) {
  return (
    a !== undefined &&
    a.length === b.length &&
    a.every((value, index) => Object.is(value, b[index]))
  );
}

class HookRuntime {
  private readonly slots: HookSlot[] = [];
  private index = 0;
  private changedEffects: number[] = [];
  private rendered: UseExternalSIPResult | null = null;

  ref<T>(initial: T): { current: T } {
    const index = this.index++;
    const existing = this.slots[index];
    if (existing?.value !== undefined) {
      return existing.value as { current: T };
    }
    const ref = { current: initial };
    this.slots[index] = { value: ref };
    return ref;
  }

  memo<T>(factory: () => T, deps: readonly unknown[]): T {
    const index = this.index++;
    const existing = this.slots[index];
    if (existing !== undefined && sameDeps(existing.deps, deps)) {
      return existing.value as T;
    }
    const value = factory();
    this.slots[index] = { ...existing, value, deps };
    return value;
  }

  state<T>(initial: T): [T, (next: T | ((prev: T) => T)) => void] {
    const index = this.index++;
    let slot = this.slots[index];
    if (slot === undefined) {
      slot = { value: initial };
      slot.setter = (next) => {
        slot.value =
          typeof next === "function"
            ? (next as (prev: T) => T)(slot.value as T)
            : next;
      };
      this.slots[index] = slot;
    }
    return [
      slot.value as T,
      slot.setter as (next: T | ((prev: T) => T)) => void,
    ];
  }

  effect(effect: Effect, deps: readonly unknown[]): void {
    const index = this.index++;
    const existing = this.slots[index];
    this.slots[index] = { ...existing, deps, effect };
    if (existing === undefined || !sameDeps(existing.deps, deps)) {
      this.changedEffects.push(index);
    }
  }

  render(): void {
    mocks.runtime.current = this;
    this.index = 0;
    this.changedEffects = [];
    // Deliberate direct invocation: this class is the test's hook runtime.
    // eslint-disable-next-line react-hooks/rules-of-hooks
    this.rendered = useExternalSIP();
    for (const index of this.changedEffects) {
      const cleanup = this.slots[index].cleanup;
      if (typeof cleanup === "function") cleanup();
    }
    for (const index of this.changedEffects) {
      const slot = this.slots[index];
      slot.cleanup = slot.effect?.();
    }
  }

  get value(): UseExternalSIPResult {
    if (this.rendered === null) throw new Error("hook has not rendered");
    return this.rendered;
  }

  unmount(): void {
    for (const slot of this.slots) {
      if (typeof slot.cleanup === "function") slot.cleanup();
      slot.cleanup = undefined;
    }
  }
}

const hookRuntimes: HookRuntime[] = [];

beforeEach(() => {
  MockPeerConnection.instances = [];
  mocks.proxy = null;
  mocks.iceServerURLs = [];
  mocks.runtime.current = null;
  vi.stubGlobal("MediaStream", MockMediaStream);
});

afterEach(() => {
  for (const runtime of hookRuntimes.splice(0)) runtime.unmount();
  for (const client of clients.splice(0)) client.dispose();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
});

describe("external SIP protocol and media", () => {
  it("dials with real browser audio/video offers and trickles ICE to the SIP service", async () => {
    const { client, transport, audio } = setup();

    const callId = await client.dial({
      credentialId: "credential-1",
      target: "sip:bob@example.com",
      kind: "video",
    });

    expect(callId).toBe("uuid-1");
    expect(audio.resume).toHaveBeenCalledOnce();
    expect(transport.sent[0]).toMatchObject({
      from: {},
      to: { serviceId: WELLKNOWN_SVC_ID_SIP },
      msgId: "uuid-2",
    });
    expect(sentCall(transport, 0)).toEqual({
      callId: "uuid-1",
      action: "dial",
      credentialId: "credential-1",
      target: "sip:bob@example.com",
      kind: "video",
      sessionDesc: { type: "offer", sdp: "browser-offer" },
    });
    expect(transport.sent[0]?.c2CEv).toBeUndefined();
    const pc = MockPeerConnection.instances[0];
    expect(pc.transceivers.map(({ kind }) => kind)).toEqual(["audio", "video"]);

    const candidate = { candidate: "candidate:1", sdpMid: "0" };
    pc.emitCandidate(candidate);
    pc.emitCandidate(null);
    await flushMicrotasks();
    expect(sentCall(transport, 1)).toEqual({
      callId: "uuid-1",
      action: "ice",
      rtcICECandidate: candidate,
    });
    expect(sentCall(transport, 2)).toEqual({
      callId: "uuid-1",
      action: "ice",
      rtcICECandidate: { candidate: "" },
    });
  });

  it("cancels a pending createOffer locally without a premature cancel or later dial", async () => {
    const offer = deferred<RTCSessionDescriptionInit>();
    const { client, transport } = setup({
      configurePeer: (peer) => {
        peer.createOffer.mockReturnValueOnce(offer.promise);
      },
    });

    const dialing = client.dial({
      credentialId: "credential-1",
      target: "1234",
      kind: "voice",
    });
    const peer = MockPeerConnection.instances[0];
    expect(peer.createOffer).toHaveBeenCalledOnce();

    await client.end("uuid-1");
    expect(transport.attempted).toEqual([]);
    expect(client.getSnapshot().calls["uuid-1"]?.state).toBe("cancelled");
    expect(peer.close).toHaveBeenCalledOnce();

    offer.resolve({ type: "offer", sdp: "late-offer" });
    await dialing;
    expect(peer.setLocalDescription).not.toHaveBeenCalled();
    expect(transport.attempted).toEqual([]);
  });

  it("sends decline for a pending inbound createOffer without a later accept", async () => {
    const offer = deferred<RTCSessionDescriptionInit>();
    const { client, transport } = setup({
      configurePeer: (peer) => {
        peer.createOffer.mockReturnValueOnce(offer.promise);
      },
    });
    await client.handleEvent(
      serverCall({
        callId: "incoming-call",
        credentialId: "credential-1",
        direction: "incoming",
        state: "ringing",
        kind: "voice",
      }),
    );

    const accepting = client.accept("incoming-call");
    const peer = MockPeerConnection.instances[0];
    expect(peer.createOffer).toHaveBeenCalledOnce();
    await client.end("incoming-call");
    expect(attemptedCallActions(transport)).toEqual(["decline"]);

    offer.resolve({ type: "offer", sdp: "late-offer" });
    await accepting;
    expect(peer.setLocalDescription).not.toHaveBeenCalled();
    expect(attemptedCallActions(transport)).toEqual(["decline"]);
  });

  it("does not send dial after cancellation while setLocalDescription is pending", async () => {
    const settingLocal = deferred<void>();
    const { client, transport } = setup({
      configurePeer: (peer) => {
        peer.setLocalDescription.mockImplementationOnce(async (description) => {
          await settingLocal.promise;
          peer.localDescription =
            description as unknown as RTCSessionDescription;
        });
      },
    });

    const dialing = client.dial({
      credentialId: "credential-1",
      target: "1234",
      kind: "voice",
    });
    await flushMicrotasks();
    const peer = MockPeerConnection.instances[0];
    expect(peer.setLocalDescription).toHaveBeenCalledOnce();

    await client.end("uuid-1");
    expect(transport.attempted).toEqual([]);
    expect(client.getSnapshot().calls["uuid-1"]?.state).toBe("cancelled");
    expect(peer.close).toHaveBeenCalledOnce();

    settingLocal.resolve();
    await dialing;
    expect(transport.attempted).toEqual([]);
  });

  it("serializes cancel behind an in-flight dial write", async () => {
    const { client, transport } = setup();
    const dialWrite = deferred<void>();
    transport.write.mockImplementationOnce(async (event) => {
      transport.attempted.push(event);
      await dialWrite.promise;
      transport.sent.push(event);
    });

    const dialing = client.dial({
      credentialId: "credential-1",
      target: "1234",
      kind: "voice",
    });
    await flushMicrotasks();
    expect(attemptedCallActions(transport)).toEqual(["dial"]);

    const ending = client.end("uuid-1");
    await flushMicrotasks();
    expect(attemptedCallActions(transport)).toEqual(["dial"]);

    dialWrite.resolve();
    await Promise.all([dialing, ending]);
    await flushMicrotasks();
    expect(attemptedCallActions(transport)).toEqual(["dial", "cancel"]);
  });

  it("serializes decline behind an in-flight accept write", async () => {
    const { client, transport } = setup();
    await client.handleEvent(
      serverCall({
        callId: "incoming-call",
        credentialId: "credential-1",
        direction: "incoming",
        state: "ringing",
        kind: "voice",
      }),
    );
    const acceptWrite = deferred<void>();
    transport.write.mockImplementationOnce(async (event) => {
      transport.attempted.push(event);
      await acceptWrite.promise;
      transport.sent.push(event);
    });

    const accepting = client.accept("incoming-call");
    await flushMicrotasks();
    expect(attemptedCallActions(transport)).toEqual(["accept"]);

    const declining = client.decline("incoming-call");
    await flushMicrotasks();
    expect(attemptedCallActions(transport)).toEqual(["accept"]);

    acceptWrite.resolve();
    await Promise.all([accepting, declining]);
    await flushMicrotasks();
    expect(attemptedCallActions(transport)).toEqual(["accept", "decline"]);
  });

  it("does not notify the server when dial fails before creating an offer", async () => {
    const { client, transport } = setup({
      configurePeer: (peer) => {
        peer.createOffer.mockRejectedValueOnce(new Error("offer failed"));
      },
    });

    const callId = await client.dial({
      credentialId: "credential-1",
      target: "1234",
      kind: "voice",
    });

    expect(callId).toBe("uuid-1");
    expect(transport.attempted).toEqual([]);
    expect(client.getSnapshot().calls["uuid-1"]).toMatchObject({
      state: "failed",
      reason: "offer failed",
    });
    expect(MockPeerConnection.instances[0]?.close).toHaveBeenCalledOnce();
  });

  it("does not send a cancel after the initial offer write fails", async () => {
    const { client, transport } = setup();
    transport.failAllWrites = true;

    await client.dial({
      credentialId: "credential-1",
      target: "1234",
      kind: "voice",
    });

    expect(attemptedCallActions(transport)).toEqual(["dial"]);
    expect(client.getSnapshot().calls["uuid-1"]?.state).toBe("failed");
    expect(MockPeerConnection.instances[0]?.close).toHaveBeenCalledOnce();
  });

  it("cleans up a failed inbound offer locally before accept is sent", async () => {
    const { client, transport } = setup({
      configurePeer: (peer) => {
        peer.setLocalDescription.mockRejectedValueOnce(
          new Error("local description failed"),
        );
      },
    });
    await client.handleEvent(
      serverCall({
        callId: "incoming-call",
        credentialId: "credential-1",
        direction: "incoming",
        state: "ringing",
        kind: "voice",
      }),
    );

    await client.accept("incoming-call");

    expect(transport.attempted).toEqual([]);
    expect(client.getSnapshot().calls["incoming-call"]).toMatchObject({
      state: "failed",
      reason: "local description failed",
    });
    expect(MockPeerConnection.instances[0]?.close).toHaveBeenCalledOnce();
  });

  it("cancels once after concurrent post-offer ICE send failures", async () => {
    const { client, transport } = setup();
    await client.dial({
      credentialId: "credential-1",
      target: "1234",
      kind: "voice",
    });
    await client.handleEvent(
      serverCall({ callId: "uuid-1", state: "ringing" }),
    );
    const peer = MockPeerConnection.instances[0];
    transport.failNextWrites(2);

    peer.emitCandidate({ candidate: "candidate:1" });
    peer.emitCandidate({ candidate: "candidate:2" });
    await flushMicrotasks();

    expect(attemptedCallActions(transport)).toEqual([
      "dial",
      "ice",
      "ice",
      "cancel",
    ]);
    expect(client.getSnapshot().calls["uuid-1"]?.state).toBe("failed");
    expect(peer.close).toHaveBeenCalledOnce();
    expect(transport.write.mock.invocationCallOrder[3]).toBeLessThan(
      peer.close.mock.invocationCallOrder[0]!,
    );
  });

  it("declines an answering inbound call and cleans up when both ICE and abort writes fail", async () => {
    const { client, transport } = setup();
    await client.handleEvent(
      serverCall({
        callId: "incoming-call",
        credentialId: "credential-1",
        direction: "incoming",
        state: "ringing",
        kind: "voice",
      }),
    );
    await client.accept("incoming-call");
    const peer = MockPeerConnection.instances[0];
    transport.failAllWrites = true;

    peer.emitCandidate({ candidate: "candidate:1" });
    await flushMicrotasks();

    expect(attemptedCallActions(transport)).toEqual([
      "accept",
      "ice",
      "decline",
    ]);
    expect(client.getSnapshot().calls["incoming-call"]?.state).toBe("failed");
    expect(peer.close).toHaveBeenCalledOnce();
  });

  it("cancels an outgoing call when applying a remote description fails", async () => {
    const { client, transport } = setup({
      configurePeer: (peer) => {
        peer.setRemoteDescription.mockRejectedValueOnce(
          new Error("remote description failed"),
        );
      },
    });
    await client.dial({
      credentialId: "credential-1",
      target: "1234",
      kind: "voice",
    });

    await client.handleEvent(
      serverCall({
        callId: "uuid-1",
        state: "ringing",
        sessionDesc: { type: "answer", sdp: "early-answer" },
      }),
    );

    expect(attemptedCallActions(transport)).toEqual(["dial", "cancel"]);
    expect(client.getSnapshot().calls["uuid-1"]).toMatchObject({
      state: "failed",
      reason: "remote description failed",
    });
    expect(MockPeerConnection.instances[0]?.close).toHaveBeenCalledOnce();
  });

  it("hangs up an accepted inbound call when a remote ICE candidate fails", async () => {
    const { client, transport } = setup({
      configurePeer: (peer) => {
        peer.addIceCandidate.mockRejectedValueOnce(
          new Error("remote candidate failed"),
        );
      },
    });
    await client.handleEvent(
      serverCall({
        callId: "incoming-call",
        credentialId: "credential-1",
        direction: "incoming",
        state: "ringing",
        kind: "voice",
      }),
    );
    await client.accept("incoming-call");

    await client.handleEvent(
      serverCall({
        callId: "incoming-call",
        state: "accepted",
        sessionDesc: { type: "answer", sdp: "answer" },
        rtcICECandidate: { candidate: "candidate:remote" },
      }),
    );

    expect(attemptedCallActions(transport)).toEqual(["accept", "hangup"]);
    expect(client.getSnapshot().calls["incoming-call"]).toMatchObject({
      state: "failed",
      reason: "remote candidate failed",
    });
    expect(MockPeerConnection.instances[0]?.close).toHaveBeenCalledOnce();
  });

  it("queues remote ICE until the answer and then applies it in order", async () => {
    const { client, audio, microphone } = setup();
    await client.dial({
      credentialId: "credential-1",
      target: "1234",
      kind: "voice",
    });
    const pc = MockPeerConnection.instances[0];
    const candidate = { candidate: "candidate:remote", sdpMid: "0" };

    await client.handleEvent(
      serverCall({
        callId: "uuid-1",
        state: "ringing",
        rtcICECandidate: candidate,
      }),
    );
    expect(pc.addIceCandidate).not.toHaveBeenCalled();

    const answer = { type: "answer", sdp: "server-answer" } as const;
    await client.handleEvent(
      serverCall({ callId: "uuid-1", state: "accepted", sessionDesc: answer }),
    );
    await flushMicrotasks();
    expect(pc.setRemoteDescription).toHaveBeenCalledWith(answer);
    expect(pc.addIceCandidate).toHaveBeenCalledWith(candidate);
    expect(audio.acquireLocalInput).toHaveBeenCalledOnce();
    expect(pc.sender("audio").replaceTrack).toHaveBeenCalledWith(microphone);
    expect(pc.transceivers.map(({ kind }) => kind)).toEqual(["audio"]);
  });

  it("requires DTMF support and negotiated telephone-event for a dialer", async () => {
    const { client } = setup();
    await client.dial({
      credentialId: "credential-1",
      target: "1234",
      kind: "voice",
    });
    const sender = MockPeerConnection.instances[0]?.sender("audio");
    if (sender === undefined) throw new Error("missing peer connection");

    await client.handleEvent(
      serverCall({
        callId: "uuid-1",
        state: "accepted",
        sessionDesc: { type: "answer", sdp: "answer" },
      }),
    );
    await flushMicrotasks();

    expect(client.dtmfFor("uuid-1")).toBeNull();
    sender.dtmf = { canInsertDTMF: true, insertDTMF: vi.fn() };
    expect(client.dtmfFor("uuid-1")).toBeNull();
    sender.codecs = [{ mimeType: "audio/opus" } as RTCRtpCodecParameters];
    expect(client.dtmfFor("uuid-1")).toBeNull();
    sender.codecs = [
      { mimeType: "audio/telephone-event" } as RTCRtpCodecParameters,
    ];
    expect(client.dtmfFor("uuid-1")).not.toBeNull();
  });

  it("checks canInsertDTMF at press time and invalidates dialers on cleanup", async () => {
    const { client } = setup();
    await client.dial({
      credentialId: "credential-1",
      target: "1234",
      kind: "voice",
    });
    const pc = MockPeerConnection.instances[0];
    const sender = pc.sender("audio");
    const insertDTMF = vi.fn();
    sender.dtmf = { canInsertDTMF: true, insertDTMF };
    sender.codecs = [
      { mimeType: "audio/telephone-event" } as RTCRtpCodecParameters,
    ];
    expect(client.dtmfFor("uuid-1")).toBeNull();

    await client.handleEvent(
      serverCall({
        callId: "uuid-1",
        state: "accepted",
        sessionDesc: { type: "answer", sdp: "answer" },
      }),
    );
    await flushMicrotasks();
    const dialer = client.dtmfFor("uuid-1");
    if (dialer === null) throw new Error("missing negotiated dialer");

    sender.dtmf.canInsertDTMF = false;
    dialer.insert("1");
    expect(insertDTMF).not.toHaveBeenCalled();
    sender.dtmf.canInsertDTMF = true;
    dialer.insert("2");
    expect(insertDTMF).toHaveBeenCalledOnce();
    expect(insertDTMF).toHaveBeenCalledWith("2");

    await client.handleEvent(serverCall({ callId: "uuid-1", state: "ended" }));
    expect(client.dtmfFor("uuid-1")).toBeNull();
    dialer.insert("3");
    expect(insertDTMF).toHaveBeenCalledOnce();
    expect(pc.close).toHaveBeenCalledOnce();
  });

  it("turns a pre-answer media transport failure into a cancel and retained failure", async () => {
    const { client, transport } = setup();
    await client.dial({
      credentialId: "credential-1",
      target: "1234",
      kind: "voice",
    });
    const pc = MockPeerConnection.instances[0];
    pc.connectionState = "failed";
    pc.onconnectionstatechange?.(new Event("connectionstatechange"));
    await flushMicrotasks();

    expect(sentCall(transport, 1)).toEqual({
      callId: "uuid-1",
      action: "cancel",
    });
    expect(client.getSnapshot().calls["uuid-1"]).toMatchObject({
      state: "failed",
      reason: "Media connection failed",
    });
    expect(pc.close).toHaveBeenCalledOnce();
  });

  it("hangs up an accepted call when media acquisition fails", async () => {
    const { client, transport, audio } = setup();
    await client.dial({
      credentialId: "credential-1",
      target: "1234",
      kind: "voice",
    });
    audio.acquireLocalInput.mockRejectedValueOnce(
      new Error("microphone denied"),
    );

    await client.handleEvent(
      serverCall({
        callId: "uuid-1",
        state: "accepted",
        sessionDesc: { type: "answer", sdp: "answer" },
      }),
    );
    await flushMicrotasks();

    expect(attemptedCallActions(transport)).toEqual(["dial", "hangup"]);
    expect(client.getSnapshot().calls["uuid-1"]).toMatchObject({
      state: "failed",
      reason: "microphone denied",
    });
    expect(MockPeerConnection.instances[0]?.close).toHaveBeenCalledOnce();
  });

  it("accepts an inbound call with a browser offer and attaches mic/camera on accepted", async () => {
    const { client, transport, microphone } = setup();
    const camera = mediaTrack("video");
    const cameraStream = mediaStream(camera);
    const getUserMedia = vi.fn(async () => cameraStream);
    vi.stubGlobal("navigator", { mediaDevices: { getUserMedia } });

    await client.handleEvent(
      serverCall({
        callId: "incoming-call",
        credentialId: "credential-1",
        target: "sip:alice@example.com",
        remoteUri: "sip:alice@example.com",
        direction: "incoming",
        state: "ringing",
        kind: "video",
      }),
    );
    expect(MockPeerConnection.instances).toHaveLength(0);
    expect(client.getSnapshot().calls["incoming-call"]).toMatchObject({
      state: "ringing",
      remoteUri: "sip:alice@example.com",
    });

    await client.accept("incoming-call");
    expect(sentCall(transport, 0)).toMatchObject({
      callId: "incoming-call",
      action: "accept",
      kind: "video",
      sessionDesc: { type: "offer", sdp: "browser-offer" },
    });
    expect(client.getSnapshot().calls["incoming-call"]?.state).toBe(
      "answering",
    );

    const answer = { type: "answer", sdp: "answering-sdp" } as const;
    await client.handleEvent(
      serverCall({
        callId: "incoming-call",
        state: "answering",
        sessionDesc: answer,
      }),
    );
    await client.handleEvent(
      serverCall({ callId: "incoming-call", state: "accepted" }),
    );
    await flushMicrotasks();

    const pc = MockPeerConnection.instances[0];
    expect(getUserMedia).toHaveBeenCalledWith({ video: true });
    expect(pc.sender("audio").replaceTrack).toHaveBeenCalledWith(microphone);
    expect(pc.sender("video").replaceTrack).toHaveBeenCalledWith(camera);
    expect(client.getSnapshot().media["incoming-call"]?.localStream).toBe(
      cameraStream,
    );

    client.dispose();
    expect(camera.stop).toHaveBeenCalledOnce();
    expect(pc.close).toHaveBeenCalledOnce();
  });

  it("plays accepted remote audio and retains terminal failure reasons after cleanup", async () => {
    const { client, audio } = setup();
    await client.dial({
      credentialId: "credential-1",
      target: "1234",
      kind: "voice",
    });
    const pc = MockPeerConnection.instances[0];
    await client.handleEvent(
      serverCall({
        callId: "uuid-1",
        state: "accepted",
        sessionDesc: { type: "answer", sdp: "answer" },
      }),
    );
    await flushMicrotasks();
    const remoteTrack = mediaTrack("audio");
    const remoteStream = mediaStream(remoteTrack);
    pc.emitTrack(remoteTrack, [remoteStream]);
    expect(audio.addRemote).toHaveBeenCalledWith(
      "external-sip:uuid-1",
      remoteStream,
    );

    await client.handleEvent(
      serverCall({
        callId: "uuid-1",
        state: "failed",
        reason: "SIP 486 Busy Here",
      }),
    );
    expect(client.getSnapshot().calls["uuid-1"]).toMatchObject({
      state: "failed",
      reason: "SIP 486 Busy Here",
    });
    expect(client.getSnapshot().media["uuid-1"]).toBeUndefined();
    expect(audio.removeRemote).toHaveBeenCalledWith("external-sip:uuid-1");
    expect(audio.releaseLocalInput).toHaveBeenCalledOnce();
    expect(pc.close).toHaveBeenCalledOnce();

    await client.handleEvent(
      serverCall({
        callId: "uuid-1",
        action: "ice",
        rtcICECandidate: { candidate: "late" },
      }),
    );
    expect(MockPeerConnection.instances).toHaveLength(1);
    expect(pc.addIceCandidate).not.toHaveBeenCalled();
  });

  it("correlates MESSAGE results and appends incoming messages without DataChannel fields", async () => {
    const { client, transport } = setup();

    await client.sendMessage({
      credentialId: "credential-1",
      target: "sip:bob@example.com",
      text: "hello",
    });
    expect(transport.sent[0]?.c2SEv?.sipMessage).toEqual({
      messageId: "uuid-1",
      credentialId: "credential-1",
      target: "sip:bob@example.com",
      text: "hello",
      action: "send",
    });
    expect(transport.sent[0]?.c2CEv).toBeUndefined();
    expect(client.getSnapshot().messages[0]?.state).toBe("sending");

    await client.handleEvent(
      serverMessage({
        messageId: "uuid-1",
        state: "sent",
        direction: "outgoing",
        createdAt: 123,
      }),
    );
    await client.handleEvent(
      serverMessage({
        messageId: "server-incoming",
        state: "received",
        direction: "incoming",
        remoteUri: "sip:alice@example.com",
        text: "reply",
        createdAt: 124,
      }),
    );
    expect(client.getSnapshot().messages).toHaveLength(2);
    expect(client.getSnapshot().messages[0]).toMatchObject({
      messageId: "uuid-1",
      text: "hello",
      state: "sent",
      createdAt: 123,
    });
    expect(client.getSnapshot().messages[1]).toMatchObject({
      messageId: "server-incoming",
      text: "reply",
      state: "received",
      direction: "incoming",
    });
  });

  it("marks live calls failed and cleans up when the signalling stream disconnects", async () => {
    const { client, transport } = setup();
    client.start();
    await client.dial({
      credentialId: "credential-1",
      target: "1234",
      kind: "voice",
    });
    const pc = MockPeerConnection.instances[0];

    transport.close();
    await flushMicrotasks();
    expect(client.getSnapshot().calls["uuid-1"]).toMatchObject({
      state: "failed",
      reason: "Signalling disconnected",
    });
    expect(pc.close).toHaveBeenCalledOnce();
  });

  it("ignores events that did not come from the external SIP service", async () => {
    const { client } = setup();
    const event = serverCall({
      callId: "forged",
      direction: "incoming",
      state: "ringing",
    });
    event.from = { serviceId: "another-service" };

    await client.handleEvent(event);
    expect(client.getSnapshot().calls).toEqual({});
  });

  it("passes unchanged ICE servers to each replacement client after reconnect", async () => {
    const firstTransport = new MockTransport();
    const secondTransport = new MockTransport();
    const iceServerURLs = ["stun:stun.example.com", "turn:turn.example.com"];
    mocks.iceServerURLs = iceServerURLs;
    vi.stubGlobal("RTCPeerConnection", MockPeerConnection);
    const runtime = new HookRuntime();
    hookRuntimes.push(runtime);

    mocks.proxy = firstTransport;
    runtime.render();
    await runtime.value.dial({
      credentialId: "credential-1",
      target: "1234",
      kind: "voice",
    });

    mocks.proxy = secondTransport;
    runtime.render();
    await runtime.value.dial({
      credentialId: "credential-1",
      target: "5678",
      kind: "voice",
    });

    expect(MockPeerConnection.instances).toHaveLength(2);
    expect(MockPeerConnection.instances[0]?.configuration).toEqual({
      iceServers: [{ urls: iceServerURLs }],
    });
    expect(MockPeerConnection.instances[1]?.configuration).toEqual({
      iceServers: [{ urls: iceServerURLs }],
    });
    expect(attemptedCallActions(firstTransport)).toEqual(["dial"]);
    expect(attemptedCallActions(secondTransport)).toEqual(["dial"]);
  });
});

describe("external SIP reducers", () => {
  const empty: ExternalSIPState = { calls: {}, messages: [] };

  it("merges sparse progress without regressing or resurrecting terminal calls", () => {
    let state = reduceSIPCallEvent(
      empty,
      {
        callId: "call",
        direction: "outgoing",
        state: "dialing",
        target: "1234",
        kind: "voice",
      },
      100,
    );
    state = reduceSIPCallEvent(
      state,
      { callId: "call", state: "accepted", remoteUri: "sip:bob@example.com" },
      200,
    );
    state = reduceSIPCallEvent(
      state,
      { callId: "call", state: "ringing" },
      300,
    );
    expect(state.calls["call"]).toMatchObject({
      state: "accepted",
      target: "1234",
      remoteUri: "sip:bob@example.com",
      since: 100,
    });
    state = reduceSIPCallEvent(
      state,
      { callId: "call", state: "ended", reason: "normal" },
      400,
    );
    const terminal = reduceSIPCallEvent(
      state,
      { callId: "call", state: "accepted" },
      500,
    );
    expect(terminal).toBe(state);
    expect(terminal.calls["call"]?.reason).toBe("normal");
  });

  it("updates an existing message ID without losing the original text", () => {
    let state = reduceSIPMessageEvent(empty, {
      messageId: "message",
      direction: "outgoing",
      state: "sent",
      text: "original",
      createdAt: 10,
    });
    state = reduceSIPMessageEvent(state, {
      messageId: "message",
      state: "failed",
      reason: "rejected",
    });
    expect(state.messages).toHaveLength(1);
    expect(state.messages[0]).toMatchObject({
      text: "original",
      state: "failed",
      reason: "rejected",
      createdAt: 10,
    });
  });
});
