import { describe, expect, it, vi } from "vitest";

import { PerfectNegotiator } from "./negotiate";
import type { SignallingEvent } from "./types";

class MockPeerConnection {
  iceConnectionState: RTCIceConnectionState = "new";
  signalingState: RTCSignalingState = "stable";
  localDescription: RTCSessionDescriptionInit | null = null;
  onnegotiationneeded: ((ev: Event) => void) | null = null;
  onicecandidate: ((ev: RTCPeerConnectionIceEvent) => void) | null = null;
  oniceconnectionstatechange: (() => void) | null = null;
  offers = 0;

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

  setState(state: RTCIceConnectionState) {
    this.iceConnectionState = state;
    this.oniceconnectionstatechange?.();
  }
}

function startNegotiation(selfSubscriber: "alice" | "bob") {
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
  });
  const running = negotiator.negotiate(input, output);
  const close = async () => {
    inputController.close();
    await running;
  };
  return { pc, events, inputController, close };
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
});
