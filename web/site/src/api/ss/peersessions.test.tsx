import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { ErrorCode, WELLKNOWN_CH_ID_MAIN } from "./types";
import type { EndpointIdentity } from "./identity";
import { SS_VISIBILITY_STORAGE_KEY } from "./visibility";
import type { SignallingState } from "./react";
import type {
  ChannelId,
  ChatChannel,
  ChatUser,
  SignallingEvent,
} from "./types";

const mocks = vi.hoisted(() => ({
  proxy: { current: null as unknown },
  runtime: { current: null as unknown },
}));

vi.mock("react", () => ({
  createContext: () => ({}),
  startTransition: (fn: () => void) => fn(),
  useContext: () => mocks.proxy.current,
  useCallback: (fn: unknown, deps: readonly unknown[]) =>
    (mocks.runtime.current as HookRuntime).memo(() => fn, deps),
  useEffect: (effect: Effect, deps: readonly unknown[]) =>
    (mocks.runtime.current as HookRuntime).effect(effect, deps),
  useMemo: <T,>(factory: () => T, deps: readonly unknown[]) =>
    (mocks.runtime.current as HookRuntime).memo(factory, deps),
  useRef: <T,>(initial: T) =>
    (mocks.runtime.current as HookRuntime).ref(initial),
  useState: <T,>(initial: T) =>
    (mocks.runtime.current as HookRuntime).state(initial),
}));

vi.mock("./react", () => ({
  useSSProxy: () => mocks.proxy.current,
}));

vi.mock("@/hooks/useICEServers", () => ({
  useICEServers: () => ({ data: [], isError: false, error: null }),
}));

vi.mock("@/api/profile", () => ({
  fetchProfile: async () => ({ username: "browser", subject_id: "user" }),
  ProfileApiError: class ProfileApiError extends Error {},
}));

import { usePeerSessions } from "./peersessions";

type Effect = () => void | (() => void);
type Cleanup = void | (() => void);
type PeerSessionsResult = ReturnType<typeof usePeerSessions>;

interface ProducerOverrides {
  me?: ChatUser | null;
  channels?: ChatChannel[];
  listingRevision?: number;
  completeMembershipChannelIds?: readonly ChannelId[];
  registrationFailed?: boolean;
  registrationReady?: boolean;
}

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

// Minimal deterministic hook runtime: enough React semantics to exercise the
// real hook's renders, memoized objects, effect setups, and ordered cleanups in
// Vitest's node environment without adding a DOM test dependency.
class HookRuntime {
  private readonly slots: HookSlot[] = [];
  private index = 0;
  private changedEffects: number[] = [];

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
    this.slots[index] = { value, deps };
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

  private finishRender(): void {
    const changed = this.changedEffects;
    for (const index of changed) {
      const cleanup = this.slots[index].cleanup;
      if (typeof cleanup === "function") cleanup();
    }
    for (const index of changed) {
      const slot = this.slots[index];
      slot.cleanup = slot.effect?.();
    }
  }

  render(
    me: ChatUser | null,
    channels: ChatChannel[],
    listingRevision = 0,
    completeMembershipChannelIds: readonly ChannelId[] = [],
    registrationFailed = false,
    registrationReady = true,
  ): PeerSessionsResult {
    this.index = 0;
    this.changedEffects = [];
    // Deliberate direct invocation: this class is the test's hook runtime.
    // eslint-disable-next-line react-hooks/rules-of-hooks
    const result = usePeerSessions(
      me,
      channels,
      listingRevision,
      completeMembershipChannelIds,
      registrationFailed,
      registrationReady,
    );
    this.finishRender();
    return result;
  }

  renderWithSignalling(
    useSignalling: () => SignallingState,
    overrides: ProducerOverrides = {},
  ): { signalling: SignallingState; peers: PeerSessionsResult } {
    this.index = 0;
    this.changedEffects = [];
    // Deliberate direct invocation: this class is the test's hook runtime.
    // eslint-disable-next-line react-hooks/rules-of-hooks
    const signalling = useSignalling();
    const producer = signalling as SignallingState & {
      completeMembershipChannelIds?: ChannelId[];
      registrationFailed?: boolean;
    };
    const me = overrides.me === undefined ? signalling.me : overrides.me;
    const channels = overrides.channels ?? signalling.channels;
    const revision = overrides.listingRevision ?? signalling.listingRevision;
    const complete =
      overrides.completeMembershipChannelIds ??
      producer.completeMembershipChannelIds ??
      [];
    const failed =
      overrides.registrationFailed ?? producer.registrationFailed ?? false;
    const ready = overrides.registrationReady ?? signalling.registrationReady;
    // eslint-disable-next-line react-hooks/rules-of-hooks
    const peers = usePeerSessions(
      me,
      channels,
      revision,
      complete,
      failed,
      ready,
    );
    this.finishRender();
    return { signalling, peers };
  }

  unmount(): void {
    for (const slot of this.slots) {
      if (typeof slot.cleanup === "function") slot.cleanup();
      slot.cleanup = undefined;
    }
  }
}

class MockProxy {
  readonly outgoing: SignallingEvent[] = [];
  readonly identity: EndpointIdentity;
  destroyed = false;
  onSend: ((event: SignallingEvent) => void) | null = null;
  onWrite: ((event: SignallingEvent) => void | Promise<void>) | null = null;
  private readonly readers = new Set<
    ReadableStreamDefaultController<SignallingEvent>
  >();

  constructor(
    identity: EndpointIdentity = { userId: "user", userSessionId: "session" },
  ) {
    this.identity = identity;
  }

  getReadStream(): ReadableStream<SignallingEvent> {
    let controller!: ReadableStreamDefaultController<SignallingEvent>;
    return new ReadableStream<SignallingEvent>({
      start: (c) => {
        controller = c;
        this.readers.add(c);
      },
      cancel: () => {
        this.readers.delete(controller);
      },
    });
  }

  getWriteStream(): WritableStream<SignallingEvent> {
    return new WritableStream<SignallingEvent>({
      write: async (event) => {
        if (this.destroyed) throw new Error("mock proxy is closed");
        await this.onWrite?.(event);
        this.outgoing.push(event);
        this.onSend?.(event);
      },
    });
  }

  receive(event: SignallingEvent): void {
    for (const reader of this.readers) reader.enqueue(event);
  }

  acknowledgeEarlyC2C(): void {}

  respond(
    request: SignallingEvent,
    event: NonNullable<SignallingEvent["s2CEv"]>,
  ): void {
    const reply: SignallingEvent = {
      from: {},
      to: { ...this.identity },
      msgId: crypto.randomUUID(),
      inReplyTo: request.msgId,
      s2CEv: event,
    };
    // Real socket replies arrive after the send continuation has recorded its
    // pending request; synchronous delivery would make pongs look unanswered.
    queueMicrotask(() => queueMicrotask(() => this.receive(reply)));
  }

  close(): void {
    this.destroyed = true;
    for (const reader of this.readers) {
      try {
        reader.close();
      } catch {
        // Already closed by an earlier disconnect.
      }
    }
    this.readers.clear();
  }
}

class MockDataChannel {
  readyState = "connecting";
  private readonly listeners = new Map<string, Set<() => void>>();
  constructor(readonly label: string) {}
  addEventListener(type: string, listener: () => void): void {
    const listeners = this.listeners.get(type) ?? new Set();
    listeners.add(listener);
    this.listeners.set(type, listeners);
  }
  emit(type: "open" | "close" | "error"): void {
    if (type === "open") this.readyState = "open";
    if (type === "close") this.readyState = "closed";
    for (const listener of this.listeners.get(type) ?? []) listener();
  }
}

class MockPeerConnection {
  static instances: MockPeerConnection[] = [];

  readonly candidates: RTCIceCandidateInit[] = [];
  readonly createdChannels: MockDataChannel[] = [];
  readonly addedTracks: {
    track: MediaStreamTrack;
    streams: MediaStream[];
    sender: RTCRtpSender;
  }[] = [];
  readonly removedSenders: RTCRtpSender[] = [];
  dtlsState: RTCDtlsTransportState = "new";
  localDescription: RTCSessionDescriptionInit | null = null;
  remoteDescription: RTCSessionDescriptionInit | null = null;
  signalingState: RTCSignalingState = "stable";
  connectionState: RTCPeerConnectionState = "new";
  iceConnectionState: RTCIceConnectionState = "new";
  onnegotiationneeded: ((event: Event) => void) | null = null;
  onicecandidate:
    ((event: { candidate: RTCIceCandidate | null }) => void) | null = null;
  oniceconnectionstatechange: (() => void) | null = null;
  ondatachannel: ((event: { channel: RTCDataChannel }) => void) | null = null;
  ontrack: ((event: RTCTrackEvent) => void) | null = null;
  onconnectionstatechange: (() => void) | null = null;
  private stateListeners = new Map<string, Set<() => void>>();

  get sctp() {
    return { transport: { state: this.dtlsState } };
  }

  constructor() {
    MockPeerConnection.instances.push(this);
  }

  createDataChannel(label: string): RTCDataChannel {
    const channel = new MockDataChannel(label);
    this.createdChannels.push(channel);
    queueMicrotask(() =>
      this.onnegotiationneeded?.(new Event("negotiationneeded")),
    );
    return channel as unknown as RTCDataChannel;
  }

  addTrack(track: MediaStreamTrack, ...streams: MediaStream[]): RTCRtpSender {
    const sender = { track } as RTCRtpSender;
    this.addedTracks.push({ track, streams, sender });
    queueMicrotask(() =>
      this.onnegotiationneeded?.(new Event("negotiationneeded")),
    );
    return sender;
  }

  removeTrack(sender: RTCRtpSender): void {
    this.removedSenders.push(sender);
  }

  async setLocalDescription(description?: RTCSessionDescriptionInit) {
    const type =
      description?.type ??
      (this.remoteDescription?.type === "offer" ? "answer" : "offer");
    this.localDescription = { type, sdp: `${type}-sdp` };
    this.signalingState = type === "offer" ? "have-local-offer" : "stable";
  }

  async setRemoteDescription(description: RTCSessionDescriptionInit) {
    this.remoteDescription = description;
    this.signalingState =
      description.type === "offer" ? "have-remote-offer" : "stable";
  }

  async addIceCandidate(candidate: RTCIceCandidateInit) {
    this.candidates.push(candidate);
  }

  restartIce(): void {}

  async getStats(): Promise<RTCStatsReport> {
    return new Map() as RTCStatsReport;
  }

  addEventListener(type: string, listener: () => void): void {
    const listeners = this.stateListeners.get(type) ?? new Set();
    listeners.add(listener);
    this.stateListeners.set(type, listeners);
  }

  removeEventListener(type: string, listener: () => void): void {
    this.stateListeners.get(type)?.delete(listener);
  }

  emitStateEvent(type: "connectionstatechange" | "iceconnectionstatechange") {
    for (const listener of this.stateListeners.get(type) ?? []) listener();
  }

  setState(state: RTCIceConnectionState): void {
    this.iceConnectionState = state;
    this.connectionState =
      state === "connected" || state === "completed"
        ? "connected"
        : state === "disconnected"
          ? "disconnected"
          : state === "failed"
            ? "failed"
            : state === "closed"
              ? "closed"
              : state === "checking"
                ? "connecting"
                : "new";
    this.oniceconnectionstatechange?.();
    this.emitStateEvent("iceconnectionstatechange");
    this.onconnectionstatechange?.();
    this.emitStateEvent("connectionstatechange");
  }

  close(): void {
    this.connectionState = "closed";
    this.iceConnectionState = "closed";
    this.signalingState = "closed";
  }
}

const channelId = WELLKNOWN_CH_ID_MAIN;
const peer: ChatUser & { subscriberId: string } = {
  id: "0-bot",
  name: "bot",
  online: true,
  visible: true,
  channelId,
  subscriberId: "0-bot",
};

function self(subscriberId: string): ChatUser & { subscriberId: string } {
  return {
    id: `user-${subscriberId}`,
    name: subscriberId,
    online: true,
    visible: true,
    channelId,
    subscriberId,
  };
}

function listing(members: ChatUser[]): ChatChannel[] {
  return [{ id: channelId, name: "main", visible: true, members }];
}

function c2c(
  fromSubscriber: string,
  toSubscriber: string,
  payload: Pick<
    NonNullable<SignallingEvent["c2CEv"]>,
    "sessionDesc" | "rtcICECandidate"
  >,
): SignallingEvent {
  return {
    from: {},
    to: {},
    msgId: crypto.randomUUID(),
    c2CEv: { channelId, fromSubscriber, toSubscriber, ...payload },
  };
}

function callPayload(
  fromSubscriberId: string,
  toSubscriberId: string,
): NonNullable<NonNullable<SignallingEvent["c2CEv"]>["call"]> {
  return {
    mimeVersion: "1.0",
    channelId,
    fromSubscriberId,
    toSubscriberId,
    creationTimestamp: 1234.5,
    msgId: crypto.randomUUID(),
    mimeType: "application/x-sip",
    plaintext: "",
    sip: {
      callId: "call-id",
      method: "INVITE",
      "X-Media": "voice",
      "X-Call-Status": "inviting",
    },
  };
}

async function flushMicrotasks() {
  for (let i = 0; i < 20; i++) await Promise.resolve();
}

type ListingMode = "incomplete-members" | "profile-failure" | "complete";

function prepareProducer(
  mode: { current: ListingMode },
  registrationFails = false,
  storedVisibility?: "true" | "false",
) {
  vi.useFakeTimers();
  const stored = new Map<string, string>();
  if (storedVisibility !== undefined)
    stored.set(SS_VISIBILITY_STORAGE_KEY, storedVisibility);
  vi.stubGlobal("window", {
    localStorage: {
      getItem: (key: string) => stored.get(key) ?? null,
      setItem: (key: string, value: string) => stored.set(key, value),
      removeItem: (key: string) => stored.delete(key),
    },
    setTimeout: globalThis.setTimeout.bind(globalThis),
    clearTimeout: globalThis.clearTimeout.bind(globalThis),
    setInterval: globalThis.setInterval.bind(globalThis),
    clearInterval: globalThis.clearInterval.bind(globalThis),
    location: {
      pathname: "/chat",
      search: "",
      hash: "",
      assign: () => {},
    },
  });
  proxy.onSend = (request) => {
    const event = request.c2SEv;
    if (event?.register) {
      proxy.respond(
        request,
        registrationFails
          ? {}
          : {
              registerResult: {
                channelId,
                subscriberId: "1000",
                visible: event.register.visible ?? true,
              },
            },
      );
      return;
    }
    if (event?.listChannels) {
      proxy.respond(request, {
        channelListResult: { channels: [channelId], hasMore: false },
      });
      return;
    }
    if (event?.channelProfileQuery) {
      proxy.respond(request, {
        channelProfile: { channelId, channelName: "main", visible: true },
      });
      return;
    }
    if (event?.listChannelMembers) {
      if (mode.current === "incomplete-members") {
        proxy.respond(request, {
          channelMbsListResult: {
            channelId,
            members: [],
            hasMore: true,
          },
        });
      } else {
        proxy.respond(request, {
          channelMbsListResult: {
            channelId,
            members: registrationFails ? [] : [peer.subscriberId],
            hasMore: false,
          },
        });
      }
      return;
    }
    if (event?.userProfileQuery) {
      proxy.respond(
        request,
        mode.current === "profile-failure"
          ? {}
          : {
              profile: {
                subscriberId: peer.subscriberId,
                channelId,
                username: peer.name,
                visible: true,
              },
            },
      );
      return;
    }
    if (event?.ping) {
      proxy.respond(request, {
        pong: {
          pingId: event.ping.pingId,
          sequenceNumber: 0,
          ackSequenceNumber: event.ping.sequenceNumber + 1,
        },
      });
    }
  };
}

async function producerAtRevision(target: number) {
  for (let attempt = 0; attempt < 12; attempt++) {
    await flushMicrotasks();
    const result = runtime.renderWithSignalling(actualUseSignalling);
    if (result.signalling.listingRevision >= target) return result;
    await vi.advanceTimersByTimeAsync(5000);
  }
  throw new Error(`listing revision ${target} was not produced`);
}

let runtime: HookRuntime;
let proxy: MockProxy;
let actualUseSignalling: typeof import("./react").useSignalling;

beforeEach(async () => {
  MockPeerConnection.instances = [];
  proxy = new MockProxy();
  runtime = new HookRuntime();
  mocks.proxy.current = proxy;
  mocks.runtime.current = runtime;
  vi.stubGlobal("RTCPeerConnection", MockPeerConnection);
  actualUseSignalling = (
    await vi.importActual<typeof import("./react")>("./react")
  ).useSignalling;
});

afterEach(async () => {
  // Let negotiators observe transport EOF and release their reader locks
  // before hook teardown cancels the now-unlocked streams.
  proxy.close();
  await flushMicrotasks();
  runtime.unmount();
  await flushMicrotasks();
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

describe("usePeerSessions startup lifecycle", () => {
  it("synchronizes and logs the current PC state on an ICE-only event", async () => {
    const info = vi.spyOn(console, "info").mockImplementation(() => {});
    try {
      const me = self("1000");
      runtime.render(me, listing([peer]));
      await flushMicrotasks();
      const pc = MockPeerConnection.instances[0];
      expect(pc).toBeDefined();

      pc.iceConnectionState = "disconnected";
      pc.connectionState = "disconnected";
      pc.oniceconnectionstatechange?.();
      pc.emitStateEvent("iceconnectionstatechange");
      pc.emitStateEvent("connectionstatechange");
      expect(
        runtime.render(me, listing([peer])).connectionStates[channelId][
          peer.subscriberId
        ],
      ).toBe("disconnected");

      // The browser updated both properties but delivered only the ICE event.
      // The session must re-read the aggregate instead of waiting forever for
      // a missing connectionstatechange notification.
      pc.iceConnectionState = "connected";
      pc.connectionState = "connected";
      pc.oniceconnectionstatechange?.();
      pc.emitStateEvent("iceconnectionstatechange");
      expect(
        runtime.render(me, listing([peer])).connectionStates[channelId][
          peer.subscriberId
        ],
      ).toBe("connected");
      expect(info).toHaveBeenCalledWith(
        `[rtc ${me.subscriberId}→${peer.subscriberId}]`,
        expect.objectContaining({
          event: "iceconnectionstatechange",
          sessionId: expect.any(String),
          peer: peer.subscriberId,
          previousConnectionState: "disconnected",
          previousIceConnectionState: "disconnected",
          connectionState: "connected",
          iceConnectionState: "connected",
        }),
      );
    } finally {
      info.mockRestore();
    }
  });

  it("replaces only a failed DTLS transport while preserving the logical session", async () => {
    const me = self("1000");
    const sessions = runtime.render(me, listing([peer])).sessions;
    await flushMicrotasks();
    const original = MockPeerConnection.instances[0];
    const onChannel = vi.fn();
    const onReset = vi.fn();
    sessions.subscribeReset(onReset);
    const originalChannel = new MockDataChannel("dcmsg");
    original.ondatachannel?.({
      channel: originalChannel as unknown as RTCDataChannel,
    });
    // The subscription can arrive after OnDataChannel; the received label must
    // still be part of a later transport replacement.
    sessions.subscribeChannel("dcmsg", onChannel);
    let openedChannel: RTCDataChannel | null | undefined;
    const opened = sessions
      .whenOpenChannel("dcmsg", channelId, peer.subscriberId)
      .then((channel) => {
        openedChannel = channel;
      });
    const track = { id: "mic", kind: "audio" } as MediaStreamTrack;
    const originalSender = sessions.addTrack(
      channelId,
      peer.subscriberId,
      track,
    );
    expect(originalSender).not.toBeNull();
    await flushMicrotasks();

    original.dtlsState = "connected";
    original.setState("connected");
    original.signalingState = "stable";
    original.iceConnectionState = "failed";
    original.connectionState = "failed";
    original.emitStateEvent("connectionstatechange");
    // Aggregate failure with a live DTLS association remains ICE-restartable.
    expect(MockPeerConnection.instances).toHaveLength(1);

    original.iceConnectionState = "connected";
    original.dtlsState = "failed";
    original.emitStateEvent("connectionstatechange");
    expect(MockPeerConnection.instances).toHaveLength(2);
    const fresh = MockPeerConnection.instances[1];
    expect(original.signalingState).toBe("closed");
    expect(onReset).not.toHaveBeenCalled();
    expect(fresh.createdChannels.map((channel) => channel.label)).toEqual([
      "dcmsg",
    ]);
    expect(fresh.addedTracks.map((added) => added.track)).toEqual([track]);
    expect(onChannel).toHaveBeenLastCalledWith(
      channelId,
      peer.subscriberId,
      fresh.createdChannels[0],
    );

    await flushMicrotasks();
    expect(fresh.localDescription?.type).toBe("offer");
    expect(proxy.outgoing.at(-1)?.c2CEv?.sessionDesc?.type).toBe("offer");
    originalChannel.emit("open");
    await flushMicrotasks();
    expect(openedChannel).toBeUndefined();
    fresh.createdChannels[0].emit("open");
    await opened;
    expect(openedChannel).toBe(fresh.createdChannels[0]);
    expect(
      sessions.resolveSender(
        channelId,
        peer.subscriberId,
        originalSender as RTCRtpSender,
      ),
    ).toBe(fresh.addedTracks[0].sender);
    sessions.removeTrack(
      channelId,
      peer.subscriberId,
      originalSender as RTCRtpSender,
    );
    expect(fresh.removedSenders).toEqual([fresh.addedTracks[0].sender]);

    // Late events from the replaced PC cannot trigger another replacement.
    original.emitStateEvent("connectionstatechange");
    expect(MockPeerConnection.instances).toHaveLength(2);
    fresh.signalingState = "stable";
    fresh.dtlsState = "connected";
    fresh.setState("connected");
    expect(
      runtime.render(me, listing([peer])).connectionStates[channelId][
        peer.subscriberId
      ],
    ).toBe("connected");
    expect(onReset).not.toHaveBeenCalled();
  });

  it("replays an offer and candidate received before the peer appears in membership", async () => {
    const me = self("1000");
    const sessions = runtime.render(me, []).sessions;
    sessions.subscribeChannel("dcmsg", () => {});

    const offer: RTCSessionDescriptionInit = {
      type: "offer",
      sdp: "early-offer-sdp",
    };
    const candidate: RTCIceCandidateInit = {
      candidate: "candidate:early",
      sdpMid: "0",
    };
    proxy.receive(
      c2c(peer.subscriberId, me.subscriberId, { sessionDesc: offer }),
    );
    proxy.receive(
      c2c(peer.subscriberId, me.subscriberId, {
        rtcICECandidate: candidate,
      }),
    );
    expect(MockPeerConnection.instances).toHaveLength(0);

    runtime.render(me, listing([peer]));
    await flushMicrotasks();

    expect(MockPeerConnection.instances).toHaveLength(1);
    const pc = MockPeerConnection.instances[0];
    expect(pc.remoteDescription).toEqual(offer);
    expect(pc.localDescription?.type).toBe("answer");
    expect(pc.candidates).toEqual([candidate]);
    expect(
      proxy.outgoing.filter(
        (event) => event.c2CEv?.sessionDesc?.type === "answer",
      ),
    ).toHaveLength(1);
  });

  it("preserves an early offer through initial registration", async () => {
    const sessions = runtime.render(null, []).sessions;
    sessions.subscribeChannel("dcmsg", () => {});
    const me = self("1000");
    const offer: RTCSessionDescriptionInit = {
      type: "offer",
      sdp: "pre-registration-offer",
    };
    const candidate: RTCIceCandidateInit = {
      candidate: "candidate:pre-registration",
      sdpMid: "0",
    };
    proxy.receive(
      c2c(peer.subscriberId, me.subscriberId, { sessionDesc: offer }),
    );
    proxy.receive(
      c2c(peer.subscriberId, me.subscriberId, {
        rtcICECandidate: candidate,
      }),
    );
    await flushMicrotasks();
    expect(MockPeerConnection.instances).toHaveLength(0);

    runtime.render(me, []);
    runtime.render(me, listing([peer]));
    await flushMicrotasks();

    expect(MockPeerConnection.instances).toHaveLength(1);
    const pc = MockPeerConnection.instances[0];
    expect(pc.remoteDescription).toEqual(offer);
    expect(pc.localDescription?.type).toBe("answer");
    expect(pc.candidates).toEqual([candidate]);
  });

  it("carries a pending offer across a proxy change into the same registration", async () => {
    const firstProxy = proxy;
    const sessions = runtime.render(null, []).sessions;
    sessions.subscribeChannel("dcmsg", () => {});
    const me = self("1000");
    proxy.receive(
      c2c(peer.subscriberId, me.subscriberId, {
        sessionDesc: { type: "offer", sdp: "old-proxy-offer" },
      }),
    );
    await flushMicrotasks();

    mocks.proxy.current = null;
    runtime.render(null, [], 0, [], false, false);
    const secondProxy = new MockProxy();
    mocks.proxy.current = secondProxy;
    proxy = secondProxy;
    runtime.render(null, [], 0, [], false, false);
    runtime.render(me, listing([peer]));
    await flushMicrotasks();

    expect(MockPeerConnection.instances).toHaveLength(1);
    const pc = MockPeerConnection.instances[0];
    expect(pc.remoteDescription?.sdp).toBe("old-proxy-offer");
    expect(pc.localDescription?.type).toBe("answer");
    expect(firstProxy.outgoing).toHaveLength(0);
    expect(
      secondProxy.outgoing.filter(
        (event) => event.c2CEv?.sessionDesc?.type === "answer",
      ),
    ).toHaveLength(1);
  });

  it("drops A's pre-registration offer and call after B gets a new endpoint", async () => {
    const firstProxy = proxy;
    const { sessions } = runtime.render(null, [], 0, [], false, false);
    const received: SignallingEvent[] = [];
    sessions.subscribeChannel("dcmsg", () => {});
    sessions.subscribeCall((event) => received.push(event));
    const staleOffer: RTCSessionDescriptionInit = {
      type: "offer",
      sdp: "stale-a-offer",
    };
    firstProxy.receive(
      c2c(peer.subscriberId, "1000", { sessionDesc: staleOffer }),
    );
    firstProxy.receive({
      from: {},
      to: {},
      msgId: "stale-a-invite",
      c2CEv: {
        channelId,
        fromSubscriber: peer.subscriberId,
        toSubscriber: "1000",
        call: callPayload(peer.subscriberId, "1000"),
      },
    });
    await flushMicrotasks();
    expect(MockPeerConnection.instances).toHaveLength(0);

    firstProxy.close();
    await flushMicrotasks();
    mocks.proxy.current = null;
    runtime.render(null, [], 0, [], false, false);
    const secondProxy = new MockProxy({
      userId: "user",
      userSessionId: "b-session",
    });
    mocks.proxy.current = secondProxy;
    proxy = secondProxy;
    runtime.render(null, [], 0, [], false, false);
    await flushMicrotasks();

    // B deliberately registers the same channel and subscriber id. Nothing
    // accepted by A's unregistered demux may be adopted by this incarnation.
    runtime.render(self("1000"), listing([peer]), 1, [channelId]);
    await flushMicrotasks();
    expect(MockPeerConnection.instances).toHaveLength(1);
    const pc = MockPeerConnection.instances[0];
    expect(pc.remoteDescription).toBeNull();
    expect(pc.localDescription).toBeNull();
    expect(received).toHaveLength(0);

    const freshOffer: RTCSessionDescriptionInit = {
      type: "offer",
      sdp: "fresh-b-offer",
    };
    secondProxy.receive(
      c2c(peer.subscriberId, "1000", { sessionDesc: freshOffer }),
    );
    secondProxy.receive({
      from: {},
      to: {},
      msgId: "fresh-b-invite",
      c2CEv: {
        channelId,
        fromSubscriber: peer.subscriberId,
        toSubscriber: "1000",
        call: callPayload(peer.subscriberId, "1000"),
      },
    });
    await flushMicrotasks();
    expect(pc.remoteDescription).toEqual(freshOffer);
    expect(pc.localDescription?.type).toBe("answer");
    expect(received.map((event) => event.msgId)).toEqual(["fresh-b-invite"]);
  });

  it("retires an unadopted offer after two membership listings omit its sender", async () => {
    const me = self("1000");
    const sessions = runtime.render(me, []).sessions;
    sessions.subscribeChannel("dcmsg", () => {});
    proxy.receive(
      c2c(peer.subscriberId, me.subscriberId, {
        sessionDesc: { type: "offer", sdp: "abandoned-offer" },
      }),
    );
    await flushMicrotasks();

    // Identical rounds keep the same channel array in useSignalling; only
    // their successful-listing revisions distinguish the membership evidence.
    const emptyListing = listing([]);
    runtime.render(me, emptyListing, 1, [channelId]);
    await flushMicrotasks();
    runtime.render(me, emptyListing, 2, [channelId]);
    await flushMicrotasks();
    runtime.render(me, listing([peer]), 3, [channelId]);
    await flushMicrotasks();

    expect(MockPeerConnection.instances).toHaveLength(1);
    const pc = MockPeerConnection.instances[0];
    expect(pc.remoteDescription).toBeNull();
    expect(pc.localDescription).toBeNull();

    const freshOffer: RTCSessionDescriptionInit = {
      type: "offer",
      sdp: "fresh-offer-after-expiry",
    };
    proxy.receive(
      c2c(peer.subscriberId, me.subscriberId, {
        sessionDesc: freshOffer,
      }),
    );
    await flushMicrotasks();
    expect(pc.remoteDescription).toEqual(freshOffer);
    expect(pc.localDescription?.type).toBe("answer");
  });

  it("retains an early offer through one possibly stale membership omission", async () => {
    const me = self("1000");
    const sessions = runtime.render(me, []).sessions;
    sessions.subscribeChannel("dcmsg", () => {});
    const offer: RTCSessionDescriptionInit = {
      type: "offer",
      sdp: "offer-before-stale-listing",
    };
    proxy.receive(
      c2c(peer.subscriberId, me.subscriberId, { sessionDesc: offer }),
    );
    await flushMicrotasks();

    runtime.render(me, listing([]), 1, [channelId]);
    await flushMicrotasks();
    runtime.render(me, listing([peer]), 2, [channelId]);
    await flushMicrotasks();

    expect(MockPeerConnection.instances).toHaveLength(1);
    const pc = MockPeerConnection.instances[0];
    expect(pc.remoteDescription).toEqual(offer);
    expect(pc.localDescription?.type).toBe("answer");
  });

  it("does not count a proxy change as another membership omission", async () => {
    const me = self("1000");
    const sessions = runtime.render(me, []).sessions;
    sessions.subscribeChannel("dcmsg", () => {});
    const offer: RTCSessionDescriptionInit = {
      type: "offer",
      sdp: "offer-across-proxy-omission-check",
    };
    proxy.receive(
      c2c(peer.subscriberId, me.subscriberId, { sessionDesc: offer }),
    );
    await flushMicrotasks();
    const emptyListing = listing([]);
    runtime.render(me, emptyListing, 1, [channelId]);
    await flushMicrotasks();

    const secondProxy = new MockProxy();
    mocks.proxy.current = secondProxy;
    proxy = secondProxy;
    runtime.render(me, emptyListing, 1, [channelId]);
    await flushMicrotasks();
    runtime.render(me, listing([peer]), 2, [channelId]);
    await flushMicrotasks();

    expect(MockPeerConnection.instances).toHaveLength(1);
    const pc = MockPeerConnection.instances[0];
    expect(pc.remoteDescription).toEqual(offer);
    expect(pc.localDescription?.type).toBe("answer");
  });

  it("does not carry a pending offer into a new subscriber incarnation", async () => {
    const first = self("1000");
    const sessions = runtime.render(first, []).sessions;
    sessions.subscribeChannel("dcmsg", () => {});
    proxy.receive(
      c2c(peer.subscriberId, first.subscriberId, {
        sessionDesc: { type: "offer", sdp: "old-incarnation-offer" },
      }),
    );

    const second = self("1001");
    runtime.render(second, listing([peer]));
    await flushMicrotasks();

    expect(MockPeerConnection.instances).toHaveLength(1);
    const pc = MockPeerConnection.instances[0];
    expect(pc.remoteDescription).toBeNull();
    expect(pc.localDescription).toBeNull();

    const freshOffer: RTCSessionDescriptionInit = {
      type: "offer",
      sdp: "fresh-incarnation-offer",
    };
    proxy.receive(
      c2c(peer.subscriberId, second.subscriberId, {
        sessionDesc: freshOffer,
      }),
    );
    await flushMicrotasks();

    expect(pc.remoteDescription).toEqual(freshOffer);
    expect(pc.localDescription?.type).toBe("answer");
  });

  it("restarts omission evidence after a subscriber identity reset", async () => {
    const first = self("1000");
    const emptyListing = listing([]);
    runtime.render(first, emptyListing, 1, [channelId]);

    const second = self("1001");
    const sessions = runtime.render(second, []).sessions;
    sessions.subscribeChannel("dcmsg", () => {});
    const staleOffer: RTCSessionDescriptionInit = {
      type: "offer",
      sdp: "offer-expired-after-identity-reset",
    };
    proxy.receive(
      c2c(peer.subscriberId, second.subscriberId, {
        sessionDesc: staleOffer,
      }),
    );
    await flushMicrotasks();
    runtime.render(second, emptyListing, 1, [channelId]);
    await flushMicrotasks();
    runtime.render(second, emptyListing, 2, [channelId]);
    await flushMicrotasks();
    runtime.render(second, listing([peer]), 3, [channelId]);
    await flushMicrotasks();

    expect(MockPeerConnection.instances).toHaveLength(1);
    const pc = MockPeerConnection.instances[0];
    expect(pc.remoteDescription).toBeNull();
    const freshOffer: RTCSessionDescriptionInit = {
      type: "offer",
      sdp: "fresh-offer-after-identity-reset",
    };
    proxy.receive(
      c2c(peer.subscriberId, second.subscriberId, {
        sessionDesc: freshOffer,
      }),
    );
    await flushMicrotasks();
    expect(pc.remoteDescription).toEqual(freshOffer);
  });

  it("holds an early INVITE until its matching peer session can answer", async () => {
    const me = self("1000");
    const { sessions } = runtime.render(me, []);
    const received: SignallingEvent[] = [];
    sessions.subscribeCall((event) => received.push(event));
    proxy.receive({
      from: {},
      to: {},
      msgId: crypto.randomUUID(),
      c2CEv: {
        channelId,
        fromSubscriber: peer.subscriberId,
        toSubscriber: me.subscriberId,
        call: callPayload(peer.subscriberId, me.subscriberId),
      },
    });
    await flushMicrotasks();
    expect(received).toHaveLength(0);
    expect(MockPeerConnection.instances).toHaveLength(0);

    runtime.render(me, listing([peer]));
    await flushMicrotasks();
    expect(received).toHaveLength(1);
    expect(MockPeerConnection.instances).toHaveLength(1);
    await sessions.sendCall(
      channelId,
      peer.subscriberId,
      callPayload(me.subscriberId, peer.subscriberId),
    );
    expect(proxy.outgoing.filter((event) => event.c2CEv?.call)).toHaveLength(1);
  });

  it("drops a pre-registration call addressed to another incarnation", async () => {
    const { sessions } = runtime.render(null, []);
    const received: SignallingEvent[] = [];
    sessions.subscribeCall((event) => received.push(event));
    proxy.receive({
      from: {},
      to: {},
      msgId: crypto.randomUUID(),
      c2CEv: {
        channelId,
        fromSubscriber: peer.subscriberId,
        toSubscriber: "1000",
        call: callPayload(peer.subscriberId, "1000"),
      },
    });
    await flushMicrotasks();
    runtime.render(self("1001"), listing([peer]));
    await flushMicrotasks();
    expect(received).toHaveLength(0);
  });

  it("routes call signalling outside the negotiator and over the peer WebSocket output", async () => {
    const me = self("1000");
    const { sessions } = runtime.render(me, listing([peer]), 1, [channelId]);
    const received: SignallingEvent[] = [];
    sessions.subscribeCall((event) => received.push(event));
    await flushMicrotasks();
    expect(MockPeerConnection.instances).toHaveLength(1);
    const pc = MockPeerConnection.instances[0];
    const call: NonNullable<NonNullable<SignallingEvent["c2CEv"]>["call"]> = {
      mimeVersion: "1.0",
      channelId,
      fromSubscriberId: peer.subscriberId,
      toSubscriberId: me.subscriberId,
      creationTimestamp: 1234.5,
      msgId: "invite-message",
      mimeType: "application/x-sip",
      plaintext: "",
      sip: {
        callId: "call-id",
        method: "INVITE",
        "X-Media": "voice",
        "X-Call-Status": "inviting",
      },
    };
    proxy.receive({
      from: {},
      to: {},
      msgId: "outer-call-event",
      c2CEv: {
        channelId,
        fromSubscriber: peer.subscriberId,
        toSubscriber: me.subscriberId,
        call,
      },
    });
    await flushMicrotasks();
    expect(received).toHaveLength(1);
    expect(received[0].c2CEv?.call).toEqual(call);
    expect(pc.remoteDescription).toBeNull();
    expect(pc.localDescription).toBeNull();

    await sessions.sendCall(channelId, peer.subscriberId, {
      ...call,
      fromSubscriberId: "forged-from",
      toSubscriberId: "forged-to",
    });
    const sent = proxy.outgoing.filter((event) => event.c2CEv?.call);
    expect(sent).toHaveLength(1);
    expect(sent[0].c2CEv).toMatchObject({
      channelId,
      fromSubscriber: me.subscriberId,
      toSubscriber: peer.subscriberId,
    });
    expect(sent[0].c2CEv?.call).toMatchObject({
      channelId,
      fromSubscriberId: me.subscriberId,
      toSubscriberId: peer.subscriberId,
      msgId: "invite-message",
    });
    expect(pc.remoteDescription).toBeNull();
  });

  it("preserves a peer session and queues signalling across a proxy reconnect", async () => {
    const me = self("1000");
    const firstProxy = proxy;
    const { sessions } = runtime.render(me, listing([peer]), 1, [channelId]);
    const onReset = vi.fn();
    sessions.subscribeReset(onReset);
    await flushMicrotasks();
    expect(MockPeerConnection.instances).toHaveLength(1);
    const pc = MockPeerConnection.instances[0];

    firstProxy.close();
    await flushMicrotasks();
    mocks.proxy.current = null;
    runtime.render(me, listing([peer]), 1, [channelId]);
    await flushMicrotasks();

    expect(pc.signalingState).not.toBe("closed");
    expect(onReset).not.toHaveBeenCalled();
    const candidate = { candidate: "candidate:during-reconnect", sdpMid: "0" };
    pc.onicecandidate?.({ candidate: candidate as RTCIceCandidate });
    await flushMicrotasks();
    expect(firstProxy.outgoing).toHaveLength(0);

    const secondProxy = new MockProxy();
    mocks.proxy.current = secondProxy;
    proxy = secondProxy;
    runtime.render(me, listing([peer]), 1, [channelId]);
    await flushMicrotasks();

    expect(MockPeerConnection.instances).toHaveLength(1);
    expect(pc.signalingState).not.toBe("closed");
    expect(onReset).not.toHaveBeenCalled();
    expect(
      secondProxy.outgoing.filter(
        (event) => event.c2CEv?.rtcICECandidate === candidate,
      ),
    ).toHaveLength(1);
  });

  it("retries a queued write when the proxy changes during a failed send", async () => {
    const me = self("1000");
    const firstProxy = proxy;
    runtime.render(me, listing([peer]), 1, [channelId]);
    await flushMicrotasks();
    const pc = MockPeerConnection.instances[0];
    firstProxy.onWrite = () => new Promise<void>(() => {});
    const candidate = { candidate: "candidate:write-race", sdpMid: "0" };
    pc.onicecandidate?.({ candidate: candidate as RTCIceCandidate });
    await flushMicrotasks();

    const secondProxy = new MockProxy();
    mocks.proxy.current = secondProxy;
    proxy = secondProxy;
    runtime.render(me, listing([peer]), 1, [channelId]);
    await flushMicrotasks();
    // The old attempt never settles. A proxy swap must cancel it rather than
    // letting every later ICE message remain blocked behind it.
    expect(
      secondProxy.outgoing.filter(
        (event) => event.c2CEv?.rtcICECandidate === candidate,
      ),
    ).toHaveLength(1);

    const secondCandidate = {
      candidate: "candidate:second-write-race",
      sdpMid: "0",
    };
    secondProxy.onWrite = () => new Promise<void>(() => {});
    pc.onicecandidate?.({
      candidate: secondCandidate as RTCIceCandidate,
    });
    await flushMicrotasks();
    const thirdProxy = new MockProxy();
    mocks.proxy.current = thirdProxy;
    proxy = thirdProxy;
    runtime.render(me, listing([peer]), 1, [channelId]);
    await flushMicrotasks();
    expect(
      thirdProxy.outgoing.filter(
        (event) => event.c2CEv?.rtcICECandidate === secondCandidate,
      ),
    ).toHaveLength(1);
  });

  it("keeps the polite membership-driven initial offer", async () => {
    const me = self("0-browser");
    const sessions = runtime.render(me, []).sessions;
    sessions.subscribeChannel("dcmsg", () => {});

    runtime.render(me, listing([{ ...peer, subscriberId: "1000" }]));
    await flushMicrotasks();

    expect(MockPeerConnection.instances).toHaveLength(1);
    const pc = MockPeerConnection.instances[0];
    expect(pc.createdChannels.map((channel) => channel.label)).toEqual([
      "dcmsg",
    ]);
    expect(pc.localDescription?.type).toBe("offer");
    expect(
      proxy.outgoing.filter(
        (event) => event.c2CEv?.sessionDesc?.type === "offer",
      ),
    ).toHaveLength(1);
  });
});

const faultyListings = [
  { mode: "incomplete-members" as const, title: "truncated member pages" },
  { mode: "profile-failure" as const, title: "failed member profiles" },
];

describe("useSignalling membership and registration outcomes", () => {
  it("keeps a healthy socket open while pongs are answered every 500ms", async () => {
    prepareProducer({ current: "complete" });
    runtime.renderWithSignalling(actualUseSignalling);
    await flushMicrotasks();
    // The preference-loading effect gates registration; this deterministic
    // hook runtime needs the state-driven second render explicitly.
    runtime.renderWithSignalling(actualUseSignalling);
    await flushMicrotasks();
    const pings = () =>
      proxy.outgoing.filter((event) => event.c2SEv?.ping !== undefined);
    expect(pings()).toHaveLength(1);
    await vi.advanceTimersByTimeAsync(499);
    expect(pings()).toHaveLength(1);
    await vi.advanceTimersByTimeAsync(1);
    await flushMicrotasks();
    expect(pings()).toHaveLength(2);
    const result = runtime.renderWithSignalling(actualUseSignalling);
    expect(result.signalling.lastPing).not.toBeNull();
    expect(proxy.destroyed).toBe(false);
  });

  it("persists online/invisible switches and re-registers the same incarnation", async () => {
    prepareProducer({ current: "complete" });
    const initial = await producerAtRevision(1);
    const pc = MockPeerConnection.instances[0];
    const registrations = () =>
      proxy.outgoing.filter((event) => event.c2SEv?.register !== undefined);
    expect(registrations()).toHaveLength(1);
    expect(registrations()[0].c2SEv?.register).toMatchObject({
      subscriberId: "",
      visible: true,
    });

    initial.signalling.setVisible(false);
    expect(window.localStorage.getItem(SS_VISIBILITY_STORAGE_KEY)).toBe(
      "false",
    );
    const pending = runtime.renderWithSignalling(actualUseSignalling);
    expect(pending.signalling.visible).toBe(false);
    expect(pending.signalling.visibilityPending).toBe(true);
    expect(pending.signalling.registrationReady).toBe(true);
    expect(pending.signalling.me).toBe(initial.signalling.me);
    expect(pending.signalling.channels).toBe(initial.signalling.channels);

    const invisible = await producerAtRevision(2);
    expect(invisible.signalling.visible).toBe(false);
    expect(invisible.signalling.visibilityPending).toBe(false);
    expect(invisible.signalling.me?.visible).toBe(false);
    expect(registrations()).toHaveLength(2);
    expect(registrations()[1].c2SEv?.register).toMatchObject({
      subscriberId: "1000",
      visible: false,
    });
    expect(MockPeerConnection.instances).toHaveLength(1);
    expect(pc.signalingState).not.toBe("closed");

    invisible.signalling.setVisible(true);
    const online = await producerAtRevision(3);
    expect(window.localStorage.getItem(SS_VISIBILITY_STORAGE_KEY)).toBe("true");
    expect(online.signalling.visible).toBe(true);
    expect(online.signalling.visibilityPending).toBe(false);
    expect(online.signalling.me?.visible).toBe(true);
    expect(registrations()).toHaveLength(3);
    expect(registrations()[2].c2SEv?.register).toMatchObject({
      subscriberId: "1000",
      visible: true,
    });
    expect(MockPeerConnection.instances).toHaveLength(1);
    expect(pc.signalingState).not.toBe("closed");
  });

  it("loads an invisible preference before registration and preserves it while roaming", async () => {
    prepareProducer({ current: "complete" }, false, "false");
    const initial = await producerAtRevision(1);
    expect(initial.signalling.visible).toBe(false);
    expect(initial.signalling.me?.visible).toBe(false);
    const firstProxy = proxy;
    const firstRegistrations = firstProxy.outgoing.filter(
      (event) => event.c2SEv?.register !== undefined,
    );
    expect(firstRegistrations).toHaveLength(1);
    expect(firstRegistrations[0].c2SEv?.register).toMatchObject({
      subscriberId: "",
      visible: false,
    });

    firstProxy.close();
    await flushMicrotasks();
    mocks.proxy.current = null;
    runtime.renderWithSignalling(actualUseSignalling);
    await flushMicrotasks();
    const secondProxy = new MockProxy();
    secondProxy.onSend = firstProxy.onSend;
    mocks.proxy.current = secondProxy;
    proxy = secondProxy;
    runtime.renderWithSignalling(actualUseSignalling);
    await flushMicrotasks();
    const reconnected = runtime.renderWithSignalling(actualUseSignalling);
    await flushMicrotasks();
    const secondRegistrations = secondProxy.outgoing.filter(
      (event) => event.c2SEv?.register !== undefined,
    );
    expect(secondRegistrations).toHaveLength(1);
    expect(secondRegistrations[0].c2SEv?.register).toMatchObject({
      subscriberId: "1000",
      visible: false,
    });
    expect(reconnected.signalling.visible).toBe(false);
    expect(reconnected.signalling.registrationReady).toBe(true);
  });

  it("does not claim invisibility when a legacy SS omits confirmation", async () => {
    prepareProducer({ current: "complete" }, false, "false");
    const respond = proxy.onSend;
    proxy.onSend = (request) => {
      if (request.c2SEv?.register) {
        proxy.respond(request, {
          registerResult: { channelId, subscriberId: "1000" },
        });
        return;
      }
      respond?.(request);
    };
    await producerAtRevision(1);
    // The legacy reply restores visible=true, which schedules one confirming
    // refresh on the next render in this deterministic hook runtime.
    runtime.renderWithSignalling(actualUseSignalling);
    await flushMicrotasks();
    const result = runtime.renderWithSignalling(actualUseSignalling);
    const registrations = proxy.outgoing.filter(
      (event) => event.c2SEv?.register !== undefined,
    );
    expect(registrations).toHaveLength(2);
    expect(registrations[0].c2SEv?.register?.visible).toBe(false);
    expect(registrations[1].c2SEv?.register?.visible).toBe(true);
    expect(result.signalling.visible).toBe(true);
    expect(result.signalling.me?.visible).toBe(true);
    expect(result.signalling.visibilityPending).toBe(false);
    expect(window.localStorage.getItem(SS_VISIBILITY_STORAGE_KEY)).toBe("true");
  });

  it("retains the incarnation and queues output until roaming registration succeeds", async () => {
    prepareProducer({ current: "complete" });
    const initial = await producerAtRevision(1);
    const firstProxy = proxy;
    const onReset = vi.fn();
    initial.peers.sessions.subscribeReset(onReset);
    expect(MockPeerConnection.instances).toHaveLength(1);
    const pc = MockPeerConnection.instances[0];

    firstProxy.close();
    await flushMicrotasks();
    mocks.proxy.current = null;
    const disconnected = runtime.renderWithSignalling(actualUseSignalling);
    await flushMicrotasks();
    expect(disconnected.signalling.me).toEqual(initial.signalling.me);
    expect(disconnected.signalling.channels).toEqual(listing([peer]));
    expect(pc.signalingState).not.toBe("closed");
    expect(onReset).not.toHaveBeenCalled();
    const candidate = { candidate: "candidate:roaming", sdpMid: "0" };
    pc.onicecandidate?.({ candidate: candidate as RTCIceCandidate });
    await flushMicrotasks();

    // Hold the replacement's registration: an open socket alone is not
    // permission to flush c2c traffic after the old registration may have aged.
    const secondProxy = new MockProxy();
    secondProxy.onSend = (request) => {
      if (request.c2SEv?.register) return;
      firstProxy.onSend?.(request);
    };
    mocks.proxy.current = secondProxy;
    proxy = secondProxy;
    runtime.renderWithSignalling(actualUseSignalling);
    await flushMicrotasks();
    const pending = runtime.renderWithSignalling(actualUseSignalling);
    await flushMicrotasks();

    const registrations = secondProxy.outgoing.filter(
      (event) => event.c2SEv?.register,
    );
    expect(registrations).toHaveLength(1);
    expect(registrations[0].c2SEv?.register?.subscriberId).toBe("1000");
    expect(pending.signalling.registrationReady).toBe(false);
    expect(
      secondProxy.outgoing.filter((event) => event.c2CEv?.rtcICECandidate),
    ).toHaveLength(0);

    // EOF while that registration is pending is another transport outage, not
    // a definitive registration failure that may discard the incarnation.
    secondProxy.close();
    await flushMicrotasks();
    mocks.proxy.current = null;
    const interrupted = runtime.renderWithSignalling(actualUseSignalling);
    await flushMicrotasks();
    expect(interrupted.signalling.me).toEqual(initial.signalling.me);
    expect(interrupted.signalling.channels).toEqual(listing([peer]));
    expect(pc.signalingState).not.toBe("closed");
    expect(onReset).not.toHaveBeenCalled();

    const thirdProxy = new MockProxy();
    thirdProxy.onSend = firstProxy.onSend;
    mocks.proxy.current = thirdProxy;
    proxy = thirdProxy;
    runtime.renderWithSignalling(actualUseSignalling);
    await flushMicrotasks();
    const reconnected = runtime.renderWithSignalling(actualUseSignalling);
    await flushMicrotasks();

    const thirdRegistrations = thirdProxy.outgoing.filter(
      (event) => event.c2SEv?.register,
    );
    expect(thirdRegistrations).toHaveLength(1);
    expect(thirdRegistrations[0].c2SEv?.register?.subscriberId).toBe("1000");
    expect(reconnected.signalling.registrationReady).toBe(true);
    expect(reconnected.signalling.me).toEqual(initial.signalling.me);
    expect(reconnected.signalling.channels).toEqual(listing([peer]));
    expect(
      thirdProxy.outgoing.filter(
        (event) => event.c2CEv?.rtcICECandidate === candidate,
      ),
    ).toHaveLength(1);
    expect(MockPeerConnection.instances).toHaveLength(1);
    expect(pc.signalingState).not.toBe("closed");
    expect(onReset).not.toHaveBeenCalled();
  });

  it("starts fresh when the endpoint changes even if the subscriber id is reused", async () => {
    prepareProducer({ current: "complete" });
    const initial = await producerAtRevision(1);
    const firstProxy = proxy;
    const firstPC = MockPeerConnection.instances[0];
    const onReset = vi.fn();
    initial.peers.sessions.subscribeReset(onReset);
    firstProxy.close();
    await flushMicrotasks();

    const secondProxy = new MockProxy({
      userId: "user",
      userSessionId: "other-session",
    });
    secondProxy.onSend = firstProxy.onSend;
    mocks.proxy.current = secondProxy;
    proxy = secondProxy;
    const changed = runtime.renderWithSignalling(actualUseSignalling);

    // The new hello invalidates old output before any registration reply. Peer
    // state cannot survive merely because the server will assign "1000" again.
    expect(changed.signalling.me).toBeNull();
    expect(changed.signalling.channels).toEqual([]);
    expect(changed.signalling.registrationReady).toBe(false);
    expect(firstPC.signalingState).toBe("closed");
    expect(
      onReset.mock.calls.filter(([dropped]) => dropped === null),
    ).toHaveLength(1);

    const recovered = await producerAtRevision(1);
    await flushMicrotasks();
    const registrations = secondProxy.outgoing.filter(
      (event) => event.c2SEv?.register,
    );
    expect(registrations).toHaveLength(1);
    expect(registrations[0].c2SEv?.register?.subscriberId).toBe("");
    expect(recovered.signalling.me?.id).toBe("user");
    expect(recovered.signalling.me?.subscriberId).toBe("1000");
    expect(recovered.signalling.registrationReady).toBe(true);
    expect(MockPeerConnection.instances).toHaveLength(2);
    expect(MockPeerConnection.instances[1].signalingState).not.toBe("closed");
    expect(
      onReset.mock.calls.filter(([dropped]) => dropped === null),
    ).toHaveLength(1);
  });

  it("retransmits a polite peer's pending restart offer on the replacement socket", async () => {
    prepareProducer({ current: "complete" });
    const respond = proxy.onSend;
    const respondAsPoliteSelf = (request: SignallingEvent) => {
      if (request.c2SEv?.register) {
        proxy.respond(request, {
          registerResult: { channelId, subscriberId: "0" },
        });
        return;
      }
      respond?.(request);
    };
    proxy.onSend = respondAsPoliteSelf;
    const initial = await producerAtRevision(1);
    initial.peers.sessions.subscribeChannel("dcmsg", () => {});
    await flushMicrotasks();
    const firstProxy = proxy;
    const pc = MockPeerConnection.instances[0];
    firstProxy.receive(
      c2c(peer.subscriberId, "0", {
        sessionDesc: { type: "answer", sdp: "initial-answer" },
      }),
    );
    await flushMicrotasks();
    expect(pc.signalingState).toBe("stable");
    pc.restartIce = vi.fn(() => {
      queueMicrotask(() =>
        pc.onnegotiationneeded?.(new Event("negotiationneeded")),
      );
    });
    const respondWithPings = firstProxy.onSend;
    firstProxy.onSend = (request) => {
      if (request.c2SEv?.ping) return;
      respondWithPings?.(request);
    };
    pc.setState("connected");
    pc.setState("disconnected");

    await vi.advanceTimersByTimeAsync(3000);
    await flushMicrotasks();
    expect(pc.signalingState).toBe("have-local-offer");
    await vi.advanceTimersByTimeAsync(1000);
    await flushMicrotasks();
    expect(firstProxy.destroyed).toBe(true);
    mocks.proxy.current = null;
    runtime.renderWithSignalling(actualUseSignalling);
    await flushMicrotasks();

    const secondProxy = new MockProxy();
    secondProxy.onSend = respondAsPoliteSelf;
    mocks.proxy.current = secondProxy;
    proxy = secondProxy;
    runtime.renderWithSignalling(actualUseSignalling);
    await flushMicrotasks();
    runtime.renderWithSignalling(actualUseSignalling);
    await flushMicrotasks();

    expect(
      secondProxy.outgoing.filter(
        (event) => event.c2CEv?.sessionDesc?.type === "offer",
      ),
    ).toHaveLength(1);
  });

  it("does not duplicate a pending offer already queued for the replacement", async () => {
    vi.useFakeTimers();
    const me = self("0");
    const sessions = runtime.render(me, listing([peer])).sessions;
    sessions.subscribeChannel("dcmsg", () => {});
    await flushMicrotasks();
    const pc = MockPeerConnection.instances[0];
    proxy.receive(
      c2c(peer.subscriberId, me.subscriberId, {
        sessionDesc: { type: "answer", sdp: "initial-answer" },
      }),
    );
    await flushMicrotasks();
    expect(pc.signalingState).toBe("stable");
    pc.restartIce = vi.fn(() => {
      queueMicrotask(() =>
        pc.onnegotiationneeded?.(new Event("negotiationneeded")),
      );
    });
    mocks.proxy.current = null;
    runtime.render(me, listing([peer]));
    pc.setState("connected");
    pc.setState("disconnected");
    await vi.advanceTimersByTimeAsync(3000);
    await flushMicrotasks();
    expect(pc.signalingState).toBe("have-local-offer");
    pc.onicecandidate?.({
      candidate: { candidate: "candidate:gathered-offline" } as RTCIceCandidate,
    });
    pc.localDescription = {
      ...pc.localDescription!,
      sdp: "offer-with-new-candidates",
    };

    const secondProxy = new MockProxy();
    mocks.proxy.current = secondProxy;
    proxy = secondProxy;
    runtime.render(me, listing([peer]));
    await flushMicrotasks();

    expect(
      secondProxy.outgoing.filter(
        (event) => event.c2CEv?.sessionDesc?.type === "offer",
      ),
    ).toHaveLength(1);
    expect(
      secondProxy.outgoing.filter((event) => event.c2CEv?.rtcICECandidate),
    ).toHaveLength(1);
  });

  it("replaces an unresponsive open socket and resumes ICE restart signalling", async () => {
    prepareProducer({ current: "complete" });
    const initial = await producerAtRevision(1);
    const firstProxy = proxy;
    const onReset = vi.fn();
    initial.peers.sessions.subscribeReset(onReset);
    const pc = MockPeerConnection.instances[0];
    const respond = firstProxy.onSend;
    firstProxy.onSend = (request) => {
      if (request.c2SEv?.ping) return; // A stale OPEN socket never answers.
      respond?.(request);
    };
    pc.setState("connected");
    pc.setState("disconnected");

    await vi.advanceTimersByTimeAsync(4000);
    await flushMicrotasks();

    expect(firstProxy.destroyed).toBe(true);
    expect(pc.signalingState).not.toBe("closed");
    expect(onReset).not.toHaveBeenCalled();
    mocks.proxy.current = null;
    runtime.renderWithSignalling(actualUseSignalling);
    await flushMicrotasks();

    const secondProxy = new MockProxy();
    secondProxy.onSend = respond;
    mocks.proxy.current = secondProxy;
    proxy = secondProxy;
    runtime.renderWithSignalling(actualUseSignalling);
    await flushMicrotasks();
    runtime.renderWithSignalling(actualUseSignalling);
    await vi.advanceTimersByTimeAsync(3000);
    await flushMicrotasks();

    expect(
      secondProxy.outgoing.filter(
        (event) => event.c2CEv?.iceRestartRequest === true,
      ),
    ).toHaveLength(1);
    expect(pc.signalingState).not.toBe("closed");
    expect(onReset).not.toHaveBeenCalled();
  });

  it("retains the UI and retries roaming registration after a transient failure", async () => {
    prepareProducer({ current: "complete" });
    const initial = await producerAtRevision(1);
    const firstProxy = proxy;
    const onReset = vi.fn();
    initial.peers.sessions.subscribeReset(onReset);
    const pc = MockPeerConnection.instances[0];
    firstProxy.close();
    await flushMicrotasks();
    mocks.proxy.current = null;
    runtime.renderWithSignalling(actualUseSignalling);
    await flushMicrotasks();

    const secondProxy = new MockProxy();
    let registrationAttempts = 0;
    secondProxy.onSend = (request) => {
      if (request.c2SEv?.register) {
        registrationAttempts += 1;
        if (registrationAttempts > 2) {
          secondProxy.respond(request, {
            registerResult: { channelId, subscriberId: "1000" },
          });
        }
        return; // The first two attempts time out on an open socket.
      }
      if (request.c2SEv?.ping) {
        secondProxy.respond(request, {
          pong: {
            pingId: request.c2SEv.ping.pingId,
            sequenceNumber: 0,
            ackSequenceNumber: request.c2SEv.ping.sequenceNumber + 1,
          },
        });
        return;
      }
      firstProxy.onSend?.(request);
    };
    mocks.proxy.current = secondProxy;
    proxy = secondProxy;
    runtime.renderWithSignalling(actualUseSignalling);
    await vi.advanceTimersByTimeAsync(5000);
    await flushMicrotasks();
    const failed = runtime.renderWithSignalling(actualUseSignalling);
    await flushMicrotasks();

    expect(failed.signalling.registrationReady).toBe(false);
    expect(failed.signalling.me).toEqual(initial.signalling.me);
    expect(failed.signalling.channels).toEqual(initial.signalling.channels);
    expect(pc.signalingState).not.toBe("closed");
    expect(onReset).not.toHaveBeenCalled();

    await vi.advanceTimersByTimeAsync(2000);
    await vi.advanceTimersByTimeAsync(5000);
    await flushMicrotasks();
    const stillFailed = runtime.renderWithSignalling(actualUseSignalling);
    expect(stillFailed.signalling.registrationReady).toBe(false);
    expect(stillFailed.signalling.me).toEqual(initial.signalling.me);
    expect(stillFailed.signalling.channels).toEqual(
      initial.signalling.channels,
    );
    expect(pc.signalingState).not.toBe("closed");
    expect(onReset).not.toHaveBeenCalled();

    await vi.advanceTimersByTimeAsync(2000);
    await flushMicrotasks();
    const recovered = runtime.renderWithSignalling(actualUseSignalling);
    await flushMicrotasks();

    const registrations = secondProxy.outgoing.filter(
      (event) => event.c2SEv?.register,
    );
    expect(registrations).toHaveLength(3);
    expect(
      registrations.map((event) => event.c2SEv?.register?.subscriberId),
    ).toEqual(["1000", "1000", "1000"]);
    expect(recovered.signalling.registrationReady).toBe(true);
    expect(recovered.signalling.me).toEqual(initial.signalling.me);
    expect(recovered.signalling.channels).toEqual(initial.signalling.channels);
    expect(pc.signalingState).not.toBe("closed");
    expect(onReset).not.toHaveBeenCalled();
  });

  it("falls back to a fresh incarnation only after an explicit id conflict", async () => {
    prepareProducer({ current: "complete" });
    const initial = await producerAtRevision(1);
    const firstProxy = proxy;
    const onReset = vi.fn();
    initial.peers.sessions.subscribeReset(onReset);
    const firstPC = MockPeerConnection.instances[0];
    firstProxy.close();
    await flushMicrotasks();
    mocks.proxy.current = null;
    runtime.renderWithSignalling(actualUseSignalling);
    await flushMicrotasks();

    const secondProxy = new MockProxy();
    secondProxy.onSend = (request) => {
      if (request.c2SEv?.register) {
        if (request.c2SEv.register.subscriberId === "1000") {
          secondProxy.respond(request, {
            err: {
              errorCode: ErrorCode.SubscriberIdIsRegistered,
              errorMsg: "claimed by another session",
            },
          });
        } else {
          secondProxy.respond(request, {
            registerResult: { channelId, subscriberId: "1001" },
          });
        }
        return;
      }
      firstProxy.onSend?.(request);
    };
    mocks.proxy.current = secondProxy;
    proxy = secondProxy;
    runtime.renderWithSignalling(actualUseSignalling);
    await flushMicrotasks();
    const conflicted = runtime.renderWithSignalling(actualUseSignalling);
    await flushMicrotasks();

    expect(conflicted.signalling.registrationReady).toBe(false);
    expect(conflicted.signalling.me).toEqual(initial.signalling.me);
    expect(conflicted.signalling.channels).toEqual(initial.signalling.channels);
    expect(firstPC.signalingState).not.toBe("closed");
    expect(onReset).not.toHaveBeenCalled();

    await vi.advanceTimersByTimeAsync(2000);
    await flushMicrotasks();
    const recovered = runtime.renderWithSignalling(actualUseSignalling);
    await flushMicrotasks();

    const registrations = secondProxy.outgoing.filter(
      (event) => event.c2SEv?.register,
    );
    expect(
      registrations.map((event) => event.c2SEv?.register?.subscriberId),
    ).toEqual(["1000", ""]);
    expect(recovered.signalling.registrationReady).toBe(true);
    expect(recovered.signalling.me?.subscriberId).toBe("1001");
    expect(firstPC.signalingState).toBe("closed");
    expect(onReset).toHaveBeenCalled();
    expect(MockPeerConnection.instances).toHaveLength(2);
  });

  it("retains a confirmed listing through an open-socket listing timeout", async () => {
    prepareProducer({ current: "complete" });
    const initial = await producerAtRevision(1);
    const firstProxy = proxy;
    const onReset = vi.fn();
    initial.peers.sessions.subscribeReset(onReset);
    const pc = MockPeerConnection.instances[0];
    firstProxy.close();
    await flushMicrotasks();
    mocks.proxy.current = null;
    runtime.renderWithSignalling(actualUseSignalling);
    await flushMicrotasks();

    const secondProxy = new MockProxy();
    secondProxy.onSend = (request) => {
      if (request.c2SEv?.register) {
        secondProxy.respond(request, {
          registerResult: { channelId, subscriberId: "1000" },
        });
        return;
      }
      if (request.c2SEv?.ping) {
        secondProxy.respond(request, {
          pong: {
            pingId: request.c2SEv.ping.pingId,
            sequenceNumber: 0,
            ackSequenceNumber: request.c2SEv.ping.sequenceNumber + 1,
          },
        });
        return;
      }
      if (request.c2SEv?.listChannels) return;
      firstProxy.onSend?.(request);
    };
    mocks.proxy.current = secondProxy;
    proxy = secondProxy;
    runtime.renderWithSignalling(actualUseSignalling);
    await flushMicrotasks();
    const registered = runtime.renderWithSignalling(actualUseSignalling);
    expect(registered.signalling.registrationReady).toBe(true);

    await vi.advanceTimersByTimeAsync(5000);
    await flushMicrotasks();
    const timedOut = runtime.renderWithSignalling(actualUseSignalling);

    expect(timedOut.signalling.channels).toEqual(initial.signalling.channels);
    expect(timedOut.signalling.listingRevision).toBe(
      initial.signalling.listingRevision,
    );
    expect(pc.signalingState).not.toBe("closed");
    expect(onReset).not.toHaveBeenCalled();
  });

  it("does not replace a good listing with an EOF-derived empty listing", async () => {
    prepareProducer({ current: "complete" });
    const initial = await producerAtRevision(1);
    const firstProxy = proxy;
    const pc = MockPeerConnection.instances[0];
    const respond = firstProxy.onSend;
    firstProxy.onSend = (request) => {
      if (request.c2SEv?.listChannels) return;
      respond?.(request);
    };
    await vi.advanceTimersByTimeAsync(5000);
    expect(
      firstProxy.outgoing.filter((event) => event.c2SEv?.listChannels).length,
    ).toBeGreaterThan(1);

    firstProxy.close();
    await flushMicrotasks();
    mocks.proxy.current = null;
    const disconnected = runtime.renderWithSignalling(actualUseSignalling);
    await flushMicrotasks();

    expect(disconnected.signalling.channels).toEqual(
      initial.signalling.channels,
    );
    expect(disconnected.signalling.listingRevision).toBe(
      initial.signalling.listingRevision,
    );
    expect(pc.signalingState).not.toBe("closed");
  });

  it.each(faultyListings)(
    "$title does not produce membership-expiry authority",
    async ({ mode }) => {
      prepareProducer({ current: mode });
      runtime.renderWithSignalling(actualUseSignalling);
      await vi.advanceTimersByTimeAsync(5000);
      await flushMicrotasks();
      const result = runtime.renderWithSignalling(actualUseSignalling);

      // A partial round is not published as a new listing at all.
      expect(result.signalling.listingRevision).toBe(0);
      expect(result.signalling.channels).toEqual([]);
      expect(result.signalling.completeMembershipChannelIds).toEqual([]);
    },
  );

  it.each(faultyListings)(
    "$title cannot expire an early offer before a complete listing",
    async ({ mode }) => {
      const current: { current: ListingMode } = { current: mode };
      prepareProducer(current);
      const initial = runtime.renderWithSignalling(actualUseSignalling);
      initial.peers.sessions.subscribeChannel("dcmsg", () => {});
      const offer: RTCSessionDescriptionInit = {
        type: "offer",
        sdp: `early-offer-${mode}`,
      };
      proxy.receive(c2c(peer.subscriberId, "1000", { sessionDesc: offer }));

      // Two failed rounds produce no successful revision and no membership
      // evidence against the pending offer.
      await vi.advanceTimersByTimeAsync(10_000);
      await flushMicrotasks();
      runtime.renderWithSignalling(actualUseSignalling);
      expect(MockPeerConnection.instances).toHaveLength(0);

      current.current = "complete";
      const complete = await producerAtRevision(1);
      await flushMicrotasks();

      expect(MockPeerConnection.instances).toHaveLength(1);
      const pc = MockPeerConnection.instances[0];
      expect(pc.remoteDescription).toEqual(offer);
      expect(pc.localDescription?.type).toBe("answer");
      expect(complete.signalling.completeMembershipChannelIds).toEqual([
        channelId,
      ]);
    },
  );

  it("clears a pre-registration offer after definitive registration failure", async () => {
    const current = { current: "complete" as ListingMode };
    prepareProducer(current, true);
    const initial = runtime.renderWithSignalling(actualUseSignalling);
    initial.peers.sessions.subscribeChannel("dcmsg", () => {});
    proxy.receive(
      c2c(peer.subscriberId, "1000", {
        sessionDesc: { type: "offer", sdp: "offer-before-failed-registration" },
      }),
    );

    const failed = await producerAtRevision(1);
    const registrationFailed = failed.signalling.registrationFailed;
    runtime.renderWithSignalling(actualUseSignalling, {
      me: self("1000"),
      channels: listing([peer]),
      listingRevision: failed.signalling.listingRevision + 1,
      completeMembershipChannelIds: [channelId],
      registrationFailed: false,
      registrationReady: true,
    });
    await flushMicrotasks();

    expect(MockPeerConnection.instances).toHaveLength(1);
    const pc = MockPeerConnection.instances[0];
    expect(pc.remoteDescription).toBeNull();
    expect(registrationFailed).toBe(true);

    const freshOffer: RTCSessionDescriptionInit = {
      type: "offer",
      sdp: "offer-after-failed-registration-cleanup",
    };
    proxy.receive(c2c(peer.subscriberId, "1000", { sessionDesc: freshOffer }));
    await flushMicrotasks();
    expect(pc.remoteDescription).toEqual(freshOffer);
    expect(pc.localDescription?.type).toBe("answer");
  });
});
