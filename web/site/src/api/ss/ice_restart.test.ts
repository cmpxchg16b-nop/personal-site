import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import {
  IceRestartController,
  type IceRestartControllerOptions,
} from "./ice_restart";

function mockPeerConnection() {
  return {
    iceConnectionState: "new" as RTCIceConnectionState,
    signalingState: "stable" as RTCSignalingState,
    restartIce: vi.fn(),
  };
}

function setup(options: IceRestartControllerOptions = {}) {
  const pc = mockPeerConnection();
  const controller = new IceRestartController(
    pc as unknown as RTCPeerConnection,
    {
      disconnectGraceMs: 100,
      retryBaseMs: 50,
      retryMaxMs: 120,
      random: () => 0.5,
      ...options,
    },
  );
  const setState = (state: RTCIceConnectionState) => {
    pc.iceConnectionState = state;
    controller.handleState(state);
  };
  return { pc, controller, setState };
}

describe("IceRestartController", () => {
  beforeEach(() => vi.useFakeTimers());
  afterEach(() => vi.useRealTimers());

  it("does not recover a connection that was never usable", () => {
    const { pc, setState } = setup();
    setState("disconnected");
    setState("failed");
    vi.advanceTimersByTime(10_000);
    expect(pc.restartIce).not.toHaveBeenCalled();
  });

  it("waits for the disconnect grace period", () => {
    const { pc, setState } = setup();
    setState("connected");
    setState("disconnected");
    vi.advanceTimersByTime(99);
    expect(pc.restartIce).not.toHaveBeenCalled();
    vi.advanceTimersByTime(1);
    expect(pc.restartIce).toHaveBeenCalledTimes(1);
  });

  it("restarts immediately on failed and accelerates a pending disconnect", () => {
    const { pc, setState } = setup();
    setState("connected");
    setState("disconnected");
    vi.advanceTimersByTime(50);
    setState("failed");
    vi.advanceTimersByTime(0);
    expect(pc.restartIce).toHaveBeenCalledTimes(1);
  });

  it("does not accelerate a retry when another attempt fails", () => {
    const { pc, setState } = setup();
    setState("connected");
    setState("failed");
    vi.advanceTimersByTime(0);
    expect(pc.restartIce).toHaveBeenCalledTimes(1);

    vi.advanceTimersByTime(25);
    setState("checking");
    setState("failed");
    vi.advanceTimersByTime(24);
    expect(pc.restartIce).toHaveBeenCalledTimes(1);
    vi.advanceTimersByTime(1);
    expect(pc.restartIce).toHaveBeenCalledTimes(2);
  });

  it("retries through checking with exponential backoff capped nominally", () => {
    const { pc, setState } = setup();
    setState("connected");
    setState("failed");
    vi.advanceTimersByTime(0);
    expect(pc.restartIce).toHaveBeenCalledTimes(1);

    setState("checking");
    for (const delay of [50, 100, 120, 120]) {
      vi.advanceTimersByTime(delay - 1);
      const before = pc.restartIce.mock.calls.length;
      vi.advanceTimersByTime(1);
      expect(pc.restartIce).toHaveBeenCalledTimes(before + 1);
    }
  });

  it.each([
    { random: 0, delay: 40 },
    { random: 1, delay: 60 },
  ])("applies ±20%% jitter (random=$random)", ({ random, delay }) => {
    const { pc, setState } = setup({ random: () => random });
    setState("connected");
    setState("failed");
    vi.advanceTimersByTime(0);
    expect(pc.restartIce).toHaveBeenCalledTimes(1);
    vi.advanceTimersByTime(delay - 1);
    expect(pc.restartIce).toHaveBeenCalledTimes(1);
    vi.advanceTimersByTime(1);
    expect(pc.restartIce).toHaveBeenCalledTimes(2);
  });

  it("cancels on spontaneous recovery and resets the backoff", () => {
    const { pc, setState } = setup();
    setState("connected");
    setState("disconnected");
    setState("connected");
    vi.advanceTimersByTime(10_000);
    expect(pc.restartIce).not.toHaveBeenCalled();

    setState("disconnected");
    vi.advanceTimersByTime(100);
    expect(pc.restartIce).toHaveBeenCalledTimes(1);
    setState("connected");
    setState("disconnected");
    vi.advanceTimersByTime(99);
    expect(pc.restartIce).toHaveBeenCalledTimes(1);
    vi.advanceTimersByTime(1);
    expect(pc.restartIce).toHaveBeenCalledTimes(2);
  });

  it("cancels on close and ignores late state events", () => {
    const { pc, controller, setState } = setup();
    setState("connected");
    setState("disconnected");
    controller.close();
    setState("failed");
    vi.advanceTimersByTime(10_000);
    expect(pc.restartIce).not.toHaveBeenCalled();
    expect(vi.getTimerCount()).toBe(0);
  });

  it("reports a restart error and keeps the retry watchdog", () => {
    const onRestartError = vi.fn();
    const { pc, setState } = setup({ onRestartError });
    pc.restartIce.mockImplementationOnce(() => {
      throw new Error("restart rejected");
    });
    setState("connected");
    setState("failed");
    vi.advanceTimersByTime(0);
    expect(onRestartError).toHaveBeenCalledTimes(1);
    vi.advanceTimersByTime(50);
    expect(pc.restartIce).toHaveBeenCalledTimes(2);
  });
});
