import { describe, expect, it, vi } from "vitest";

import type { PeerDiagnostics } from "./diagnostics";
import { PerfectNegotiator } from "./negotiate";
import type { SignallingEvent } from "./types";

class MockPeerConnection {
  iceConnectionState: RTCIceConnectionState = "new";
  connectionState: RTCPeerConnectionState = "new";
  signalingState: RTCSignalingState = "stable";
  localDescription: RTCSessionDescriptionInit | null = null;
  remoteDescription: RTCSessionDescriptionInit | null = null;
  get currentRemoteDescription() {
    return this.remoteDescription;
  }
  onnegotiationneeded: ((ev: Event) => void) | null = null;
  onicecandidate: ((ev: RTCPeerConnectionIceEvent) => void) | null = null;
  oniceconnectionstatechange: (() => void) | null = null;
  offers = 0;
  private connectionStateListeners = new Set<() => void>();

  restartIce = vi.fn(() => {
    queueMicrotask(() =>
      this.onnegotiationneeded?.(new Event("negotiationneeded")),
    );
  });

  setLocalDescription = vi.fn(async () => {
    this.offers += 1;
    this.localDescription = {
      type: "offer",
      sdp: `offer-${this.offers}`,
    };
  });

  setRemoteDescription = vi.fn(
    async (description: RTCSessionDescriptionInit) => {
      if (
        description.type === "answer" &&
        this.signalingState !== "have-local-offer"
      ) {
        throw new Error("invalid answer state");
      }
      this.remoteDescription = description;
      this.signalingState =
        description.type === "answer" ? "stable" : "have-remote-offer";
    },
  );

  addEventListener(type: string, listener: () => void) {
    if (type === "connectionstatechange") {
      this.connectionStateListeners.add(listener);
    }
  }

  removeEventListener(type: string, listener: () => void) {
    if (type === "connectionstatechange") {
      this.connectionStateListeners.delete(listener);
    }
  }

  setState(state: RTCIceConnectionState) {
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
    for (const listener of this.connectionStateListeners) listener();
  }
}

function startNegotiation(
  selfSubscriber: "alice" | "bob",
  diagnostics?: PeerDiagnostics,
) {
  const pc = new MockPeerConnection();
  const events: SignallingEvent[] = [];
  let inputController!: ReadableStreamDefaultController<SignallingEvent>;
  const input = new ReadableStream<SignallingEvent>({
    start(controller) {
      inputController = controller;
    },
  });
  const output = new WritableStream<SignallingEvent>({
    write(event) {
      events.push(event);
    },
  });
  const negotiator = new PerfectNegotiator(pc as unknown as RTCPeerConnection, {
    channelId: "main",
    selfSubscriber,
    peerSubscriber: selfSubscriber === "alice" ? "bob" : "alice",
    diagnostics,
  });
  const running = negotiator.negotiate(input, output);
  const close = async () => {
    inputController.close();
    await running;
  };
  return { pc, events, inputController, close, negotiator };
}

async function flushMicrotasks() {
  for (let i = 0; i < 10; i++) await Promise.resolve();
}

describe("PerfectNegotiator ICE recovery", () => {
  it("polite peer restarts ICE after a disconnect and sends another offer", async () => {
    vi.useFakeTimers();
    try {
      const { pc, events, close } = startNegotiation("alice");
      try {
        pc.onnegotiationneeded?.(new Event("negotiationneeded"));
        await flushMicrotasks();
        expect(events).toHaveLength(1);

        pc.setState("connected");
        pc.setState("disconnected");
        await vi.advanceTimersByTimeAsync(2_999);
        expect(pc.restartIce).not.toHaveBeenCalled();
        await vi.advanceTimersByTimeAsync(1);
        await flushMicrotasks();

        expect(pc.restartIce).toHaveBeenCalledTimes(1);
        expect(events).toHaveLength(2);
        expect(events[1].c2CEv?.sessionDesc?.sdp).toBe("offer-2");
      } finally {
        await close();
      }
      expect(vi.getTimerCount()).toBe(0);
    } finally {
      vi.useRealTimers();
    }
  });

  it("impolite peer requests a restart instead of making a competing offer", async () => {
    vi.useFakeTimers();
    try {
      const { pc, events, close } = startNegotiation("bob");
      try {
        pc.setState("connected");
        pc.setState("failed");
        await vi.advanceTimersByTimeAsync(0);
        await flushMicrotasks();

        expect(pc.restartIce).not.toHaveBeenCalled();
        expect(events).toHaveLength(1);
        expect(events[0].c2CEv?.iceRestartRequest).toBe(true);
        expect(events[0].c2CEv?.sessionDesc).toBeUndefined();
      } finally {
        await close();
      }
    } finally {
      vi.useRealTimers();
    }
  });

  it("polite peer honors a restart request from the impolite peer", async () => {
    const { pc, events, inputController, close } = startNegotiation("alice");
    try {
      inputController.enqueue({
        from: {},
        to: {},
        msgId: "restart-request",
        c2CEv: {
          fromSubscriber: "bob",
          toSubscriber: "alice",
          channelId: "main",
          iceRestartRequest: true,
        },
      });
      await flushMicrotasks();

      expect(pc.restartIce).toHaveBeenCalledTimes(1);
      expect(events).toHaveLength(1);
      expect(events[0].c2CEv?.sessionDesc?.sdp).toBe("offer-1");
    } finally {
      await close();
    }
  });

  it("retransmits an unanswered offer on every retry without another negotiationneeded", async () => {
    vi.useFakeTimers();
    try {
      const { pc, events, close } = startNegotiation("alice");
      try {
        pc.onnegotiationneeded?.(new Event("negotiationneeded"));
        await flushMicrotasks();
        expect(events).toHaveLength(1);
        pc.signalingState = "have-local-offer";
        // A real browser need not emit negotiationneeded again while this
        // offer is unanswered. The old mock hid the stalled recovery path.
        pc.restartIce.mockImplementation(() => {});

        pc.setState("connected");
        pc.setState("disconnected");
        await vi.advanceTimersByTimeAsync(3_000);
        await flushMicrotasks();
        expect(events).toHaveLength(2);
        expect(events[1].c2CEv?.sessionDesc?.sdp).toBe("offer-1");

        await vi.advanceTimersByTimeAsync(2_500);
        await flushMicrotasks();
        expect(events).toHaveLength(3);
        expect(events[2].c2CEv?.sessionDesc?.sdp).toBe("offer-1");
      } finally {
        await close();
      }
    } finally {
      vi.useRealTimers();
    }
  });

  it("retransmits a pending offer when the impolite peer requests recovery", async () => {
    const { pc, events, inputController, close } = startNegotiation("alice");
    try {
      pc.onnegotiationneeded?.(new Event("negotiationneeded"));
      await flushMicrotasks();
      expect(events).toHaveLength(1);
      pc.signalingState = "have-local-offer";
      pc.restartIce.mockImplementation(() => {});

      inputController.enqueue({
        from: {},
        to: {},
        msgId: "restart-after-lost-answer",
        c2CEv: {
          fromSubscriber: "bob",
          toSubscriber: "alice",
          channelId: "main",
          iceRestartRequest: true,
        },
      });
      await flushMicrotasks();

      expect(events).toHaveLength(2);
      expect(events[1].c2CEv?.sessionDesc?.sdp).toBe("offer-1");
    } finally {
      await close();
    }
  });

  it("retries an unresolved impolite glare without an immediate duplicate", async () => {
    vi.useFakeTimers();
    try {
      const { pc, events, inputController, close } = startNegotiation("bob");
      try {
        pc.onnegotiationneeded?.(new Event("negotiationneeded"));
        await flushMicrotasks();
        expect(events).toHaveLength(1);
        pc.signalingState = "have-local-offer";

        inputController.enqueue({
          from: {},
          to: {},
          msgId: "colliding-restart-offer",
          c2CEv: {
            fromSubscriber: "alice",
            toSubscriber: "bob",
            channelId: "main",
            sessionDesc: { type: "offer", sdp: "remote-restart-offer" },
          },
        });
        await flushMicrotasks();
        expect(events).toHaveLength(1);

        await vi.advanceTimersByTimeAsync(1_999);
        expect(events).toHaveLength(1);
        await vi.advanceTimersByTimeAsync(1);
        await flushMicrotasks();
        // The polite peer must eventually receive this offer so it can roll
        // back, answer, and let the ICE watchdog finish after the glare.
        expect(events).toHaveLength(2);
        expect(events[1].c2CEv?.sessionDesc?.sdp).toBe("offer-1");

        await vi.advanceTimersByTimeAsync(2_000);
        await flushMicrotasks();
        expect(events).toHaveLength(3);
      } finally {
        await close();
      }
      expect(vi.getTimerCount()).toBe(0);
    } finally {
      vi.useRealTimers();
    }
  });

  it("ignores delayed SDP from a retired remote DTLS identity", async () => {
    const { pc, events, inputController, close, negotiator } =
      startNegotiation("alice");
    const fresh = new MockPeerConnection();
    const oldSdp = "v=0\r\na=fingerprint:sha-256 AA:BB\r\n";
    const freshSdp = "v=0\r\na=fingerprint:sha-256 CC:DD\r\n";
    try {
      pc.remoteDescription = { type: "answer", sdp: oldSdp };
      pc.signalingState = "stable";
      expect(
        negotiator.replacePeerConnection(fresh as unknown as RTCPeerConnection),
      ).toBe(true);
      await flushMicrotasks();
      expect(events).toHaveLength(1);
      fresh.signalingState = "have-local-offer";

      const receive = (msgId: string, sessionDesc: RTCSessionDescriptionInit) =>
        inputController.enqueue({
          from: {},
          to: {},
          msgId,
          c2CEv: {
            fromSubscriber: "bob",
            toSubscriber: "alice",
            channelId: "main",
            sessionDesc,
          },
        });
      receive("old-answer", { type: "answer", sdp: oldSdp });
      receive("old-offer", { type: "offer", sdp: oldSdp });
      await flushMicrotasks();
      // Neither an old answer nor a colliding old offer may complete/roll back
      // the fresh offer, even when the browser is the polite peer.
      expect(fresh.setRemoteDescription).not.toHaveBeenCalled();
      expect(fresh.signalingState).toBe("have-local-offer");
      expect(events).toHaveLength(1);

      receive("fresh-answer", { type: "answer", sdp: freshSdp });
      await flushMicrotasks();
      expect(fresh.remoteDescription?.sdp).toBe(freshSdp);
      expect(fresh.signalingState).toBe("stable");
    } finally {
      await close();
    }
  });

  it("correlates the outbound SDP diagnostic with its signalling msgId", async () => {
    const diagnostics: PeerDiagnostics = {
      log: vi.fn(),
      captureStats: vi.fn(),
      close: vi.fn(),
    };
    const { pc, events, close } = startNegotiation("alice", diagnostics);
    try {
      pc.onnegotiationneeded?.(new Event("negotiationneeded"));
      await flushMicrotasks();
      expect(events).toHaveLength(1);
      expect(diagnostics.log).toHaveBeenCalledWith(
        "sdp:offer:sent",
        expect.objectContaining({
          msgId: events[0].msgId,
          description: { type: "offer", sections: [] },
        }),
        "info",
      );
    } finally {
      await close();
    }
  });
});
