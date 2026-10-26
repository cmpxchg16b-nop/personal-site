import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { WELLKNOWN_CH_ID_MAIN } from "./types";
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
    // eslint-disable-next-line react-hooks/rules-of-hooks
    const peers = usePeerSessions(me, channels, revision, complete, failed);
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
  onSend: ((event: SignallingEvent) => void) | null = null;
  private readonly readers = new Set<
    ReadableStreamDefaultController<SignallingEvent>
  >();

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
      write: (event) => {
        this.outgoing.push(event);
        this.onSend?.(event);
      },
    });
  }

  receive(event: SignallingEvent): void {
    for (const reader of this.readers) reader.enqueue(event);
  }

  respond(
    request: SignallingEvent,
    event: NonNullable<SignallingEvent["s2CEv"]>,
  ): void {
    this.receive({
      from: {},
      to: { userId: "user" },
      msgId: crypto.randomUUID(),
      inReplyTo: request.msgId,
      s2CEv: event,
    });
  }

  close(): void {
    for (const reader of this.readers) reader.close();
  }
}

class MockDataChannel {
  readonly readyState = "connecting";
  constructor(readonly label: string) {}
  addEventListener(): void {}
}

class MockPeerConnection {
  static instances: MockPeerConnection[] = [];

  readonly candidates: RTCIceCandidateInit[] = [];
  readonly createdChannels: MockDataChannel[] = [];
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
  channelId,
  subscriberId: "0-bot",
};

function self(subscriberId: string): ChatUser & { subscriberId: string } {
  return {
    id: `user-${subscriberId}`,
    name: subscriberId,
    online: true,
    channelId,
    subscriberId,
  };
}

function listing(members: ChatUser[]): ChatChannel[] {
  return [{ id: channelId, name: "main", members }];
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

async function flushMicrotasks() {
  for (let i = 0; i < 20; i++) await Promise.resolve();
}

type ListingMode = "incomplete-members" | "profile-failure" | "complete";

function prepareProducer(
  mode: { current: ListingMode },
  registrationFails = false,
) {
  vi.useFakeTimers();
  vi.stubGlobal("window", {
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
        channelProfile: { channelId, channelName: "main" },
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
              },
            },
      );
      return;
    }
    // Leave pings unanswered: unrelated pong traffic would continually
    // re-arm the paged-listing timeout this harness needs to reach.
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

  it("still discards a pending offer when the proxy changes before registration", async () => {
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

    const secondProxy = new MockProxy();
    mocks.proxy.current = secondProxy;
    proxy = secondProxy;
    runtime.render(null, []);
    runtime.render(me, listing([peer]));
    await flushMicrotasks();

    expect(MockPeerConnection.instances).toHaveLength(1);
    const pc = MockPeerConnection.instances[0];
    expect(pc.remoteDescription).toBeNull();
    expect(pc.localDescription).toBeNull();

    const freshOffer: RTCSessionDescriptionInit = {
      type: "offer",
      sdp: "fresh-proxy-offer",
    };
    secondProxy.receive(
      c2c(peer.subscriberId, me.subscriberId, {
        sessionDesc: freshOffer,
      }),
    );
    await flushMicrotasks();
    expect(pc.remoteDescription).toEqual(freshOffer);
    expect(pc.localDescription?.type).toBe("answer");
    expect(firstProxy.outgoing).toHaveLength(0);
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
  it.each(faultyListings)(
    "$title does not produce membership-expiry authority",
    async ({ mode }) => {
      prepareProducer({ current: mode });
      runtime.renderWithSignalling(actualUseSignalling);

      const result = await producerAtRevision(1);

      // The UI remains best-effort, but the partial result is not authoritative.
      expect(result.signalling.channels).toEqual(listing([]));
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

      await producerAtRevision(1);
      expect(MockPeerConnection.instances).toHaveLength(0);
      await producerAtRevision(2);
      expect(MockPeerConnection.instances).toHaveLength(0);

      current.current = "complete";
      const complete = await producerAtRevision(3);
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
