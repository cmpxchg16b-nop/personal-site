import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import type { EndpointIdentity } from "./identity";
import { SS_IDENTITY_STORAGE_KEY } from "./identity";
import { closeSSProxy, getSSProxy, type SSProxy } from "./proxy";
import {
  WELLKNOWN_SVC_ID_SS,
  type EPAddr,
  type SignallingEvent,
} from "./types";

const identity: EndpointIdentity = {
  userId: "user",
  userSessionId: "session",
};

class MockWebSocket {
  static readonly OPEN = 1;
  static instances: MockWebSocket[] = [];

  readonly url: string;
  readonly sent: string[] = [];
  readyState = 0;
  onopen: (() => void) | null = null;
  onmessage: ((event: { data: unknown }) => void) | null = null;
  onerror: (() => void) | null = null;
  onclose: ((event: { code: number }) => void) | null = null;

  constructor(url: string | URL) {
    this.url = String(url);
    MockWebSocket.instances.push(this);
  }

  open(): void {
    this.readyState = MockWebSocket.OPEN;
    this.onopen?.();
  }

  receive(data: unknown): void {
    this.onmessage?.({ data });
  }

  receiveJSON(value: unknown): void {
    this.receive(JSON.stringify(value));
  }

  send(data: string): void {
    this.sent.push(data);
  }

  close = vi.fn((): void => {
    if (this.readyState === 3) return;
    this.readyState = 3;
    this.onclose?.({ code: 1000 });
  });
}

function stubStorage(initial?: EndpointIdentity) {
  const values = new Map<string, string>();
  if (initial !== undefined) {
    values.set(SS_IDENTITY_STORAGE_KEY, JSON.stringify(initial));
  }
  const getItem = vi.fn((key: string) => values.get(key) ?? null);
  const setItem = vi.fn((key: string, value: string) => values.set(key, value));
  const removeItem = vi.fn((key: string) => values.delete(key));
  vi.stubGlobal("window", {
    localStorage: { getItem, setItem, removeItem },
  });
  return { values, getItem, setItem, removeItem };
}

function hello(
  endpoint: EndpointIdentity = identity,
  to: EPAddr = endpoint,
): SignallingEvent {
  return {
    from: { serviceId: WELLKNOWN_SVC_ID_SS },
    to,
    msgId: "hello-message",
    s2CEv: { endpoint },
  };
}

function c2c(to: EPAddr | undefined, msgId: string): unknown {
  return {
    from: { userId: "peer", userSessionId: "peer-session" },
    to,
    msgId,
    c2CEv: {
      channelId: "channel",
      fromSubscriber: "1001",
      toSubscriber: "1000",
      ping: { pingId: "peer-ping", sequenceNumber: 1, ackSequenceNumber: 0 },
    },
  };
}

function s2c(to: EPAddr | undefined, msgId: string): unknown {
  return {
    from: { serviceId: WELLKNOWN_SVC_ID_SS },
    to,
    msgId,
    s2CEv: {
      pong: { pingId: "ping", sequenceNumber: 1, ackSequenceNumber: 2 },
    },
  };
}

async function flushMicrotasks(): Promise<void> {
  for (let i = 0; i < 6; i++) await Promise.resolve();
}

async function readAndAcknowledge(
  proxy: SSProxy,
  reader: ReadableStreamDefaultReader<SignallingEvent>,
): Promise<SignallingEvent> {
  const result = await reader.read();
  if (result.done) throw new Error("unexpected end of signalling stream");
  proxy.acknowledgeEarlyC2C(result.value);
  return result.value;
}

async function openAttempt() {
  const promise = getSSProxy();
  const ws = MockWebSocket.instances.at(-1);
  if (ws === undefined) throw new Error("no WebSocket was constructed");
  ws.open();
  await flushMicrotasks();
  return { promise, ws };
}

async function connect(endpoint: EndpointIdentity = identity) {
  const attempt = await openAttempt();
  attempt.ws.receiveJSON(hello(endpoint));
  const proxy = await attempt.promise;
  return { ...attempt, proxy };
}

beforeEach(() => {
  MockWebSocket.instances = [];
  stubStorage();
  vi.stubGlobal("WebSocket", MockWebSocket);
});

afterEach(() => {
  closeSSProxy();
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

describe("endpoint hello", () => {
  it("uses the first-frame server identity over the cached hint", async () => {
    const cached = { userId: "cached-user", userSessionId: "cached-session" };
    const storage = stubStorage(cached);
    const server = { userId: "server-user", userSessionId: "server-session" };
    const { proxy } = await connect(server);

    expect(storage.getItem).toHaveBeenCalledWith(SS_IDENTITY_STORAGE_KEY);
    expect(proxy.identity).toEqual(server);
    expect(Object.isFrozen(proxy.identity)).toBe(true);
    expect(JSON.parse(storage.values.get(SS_IDENTITY_STORAGE_KEY)!)).toEqual(
      server,
    );
  });

  it("shares the in-flight handshake and the validated singleton", async () => {
    const attempt = await openAttempt();
    expect(getSSProxy()).toBe(attempt.promise);
    attempt.ws.receiveJSON(hello());
    const proxy = await attempt.promise;
    expect(await getSSProxy()).toBe(proxy);
  });

  it("reports close reasons and reconnects after disconnect and explicit close", async () => {
    const first = await connect();
    const firstDisconnect = vi.fn();
    first.proxy.ondisconnect = firstDisconnect;
    first.proxy.close();
    expect(firstDisconnect).toHaveBeenCalledTimes(1);
    expect(firstDisconnect).toHaveBeenCalledWith("reconnect");
    const second = await connect();
    expect(second.proxy).not.toBe(first.proxy);
    expect(second.ws).not.toBe(first.ws);

    const secondDisconnect = vi.fn();
    second.proxy.ondisconnect = secondDisconnect;
    closeSSProxy();
    expect(secondDisconnect).toHaveBeenCalledTimes(1);
    expect(secondDisconnect).toHaveBeenCalledWith("shutdown");
    expect(second.proxy.destroyed).toBe(true);
    const third = await connect();
    expect(third.proxy).not.toBe(second.proxy);
    expect(third.ws).not.toBe(second.ws);
  });

  it("times out a missing hello and closes the socket", async () => {
    vi.useFakeTimers();
    const attempt = await openAttempt();
    const rejection = expect(attempt.promise).rejects.toThrow(
      "timed out after 5000ms",
    );
    await vi.advanceTimersByTimeAsync(4999);
    expect(attempt.ws.close).not.toHaveBeenCalled();
    await vi.advanceTimersByTimeAsync(1);
    await rejection;
    expect(attempt.ws.close).toHaveBeenCalledTimes(1);
  });

  const invalidHellos: Array<[string, () => unknown]> = [
    [
      "wrong source service",
      () => ({ ...hello(), from: { serviceId: "not-ss" } }),
    ],
    ["empty message id", () => ({ ...hello(), msgId: "" })],
    ["missing endpoint", () => ({ ...hello(), s2CEv: {} })],
    [
      "half endpoint",
      () => ({ ...hello(), s2CEv: { endpoint: { userId: "user" } } }),
    ],
    ["half to address", () => hello(identity, { userId: "user" })],
    [
      "to/payload mismatch",
      () =>
        hello(identity, {
          userId: "user",
          userSessionId: "different-session",
        }),
    ],
    ["ordinary event before hello", () => s2c(identity, "ordinary-first")],
  ];

  it.each(invalidHellos)("rejects and closes on %s", async (_name, make) => {
    const attempt = await openAttempt();
    const rejection = expect(attempt.promise).rejects.toThrow(
      "invalid endpoint hello",
    );
    attempt.ws.receiveJSON(make());
    await rejection;
    expect(attempt.ws.close).toHaveBeenCalledTimes(1);
  });

  it.each(["not-json", new Uint8Array([1, 2, 3])])(
    "rejects a malformed or binary first frame",
    async (frame) => {
      const attempt = await openAttempt();
      const rejection = expect(attempt.promise).rejects.toThrow(
        "first frame is not a valid event",
      );
      attempt.ws.receive(frame);
      await rejection;
      expect(attempt.ws.close).toHaveBeenCalledTimes(1);
    },
  );

  it("does not let a discarded in-flight hello repopulate logout state", async () => {
    const storage = stubStorage(identity);
    const attempt = await openAttempt();
    closeSSProxy();
    storage.values.clear();
    attempt.ws.receiveJSON(hello());
    const proxy = await attempt.promise;
    await flushMicrotasks();

    expect(proxy.destroyed).toBe(true);
    expect(storage.values.has(SS_IDENTITY_STORAGE_KEY)).toBe(false);
  });
});

describe("recipient filtering and fan-out", () => {
  it("drops foreign, empty, and half-addressed c2c/s2c events", async () => {
    const { ws, proxy } = await connect();
    const reader = proxy.getReadStream().getReader();
    const foreign: EPAddr = { userId: "other", userSessionId: "other-session" };
    const otherSession: EPAddr = {
      userId: identity.userId,
      userSessionId: "other-session",
    };

    ws.receiveJSON(c2c(foreign, "foreign-c2c"));
    ws.receiveJSON(s2c(foreign, "foreign-s2c"));
    ws.receiveJSON(c2c(otherSession, "other-session-c2c"));
    ws.receiveJSON(s2c(otherSession, "other-session-s2c"));
    ws.receiveJSON(c2c({}, "empty-c2c"));
    ws.receiveJSON(s2c({}, "empty-s2c"));
    ws.receiveJSON(c2c({ userId: identity.userId }, "half-c2c"));
    ws.receiveJSON(s2c({ userSessionId: identity.userSessionId }, "half-s2c"));
    ws.receiveJSON(c2c(undefined, "missing-c2c"));
    ws.receiveJSON(s2c(undefined, "missing-s2c"));
    const own = c2c(identity, "own-after-foreign");
    ws.receiveJSON(own);

    const first = await reader.read();
    expect(first.done).toBe(false);
    expect(first.value).toEqual(own);
    await reader.cancel();
  });

  it("replays early self c2c once to the peer demux reader", async () => {
    const { ws, proxy } = await connect();
    const first = c2c(identity, "early-before-readers");
    ws.receiveJSON(c2c({ userId: "other", userSessionId: "other" }, "foreign"));
    ws.receiveJSON(first);

    // A ping/registration reader does not claim the peer backlog. An event that
    // arrives now is both live to that reader and retained for the peer demux.
    const ordinary = proxy.getReadStream().getReader();
    const second = c2c(identity, "early-after-ordinary-reader");
    ws.receiveJSON(second);

    // StrictMode may cancel the first effect's reader before its pending read
    // resolves. The next setup must still see every unacknowledged event.
    const cancelled = proxy.getReadStream({ replayEarlyC2C: true }).getReader();
    const cancelledRead = cancelled.read();
    await cancelled.cancel();
    const cancelledResult = await cancelledRead;
    expect(cancelledResult.done).toBe(false);
    // The queued read may still yield after cancel. Deliberately do not ack it,
    // matching the demux's post-cleanup live=false return before acknowledgement.
    expect(cancelledResult.value).toEqual(first);

    const demux = proxy.getReadStream({ replayEarlyC2C: true }).getReader();
    expect(await readAndAcknowledge(proxy, demux)).toEqual(first);
    expect(await readAndAcknowledge(proxy, demux)).toEqual(second);

    // The backlog is single-consumer and single-use: another demux-style reader
    // sees only subsequent live events, while the original demux keeps fan-out.
    const laterReader = proxy
      .getReadStream({ replayEarlyC2C: true })
      .getReader();
    const live = c2c(identity, "live-after-replay");
    ws.receiveJSON(live);
    expect((await demux.read()).value).toEqual(live);
    expect((await laterReader.read()).value).toEqual(live);

    await ordinary.cancel();
    await demux.cancel();
    await laterReader.cancel();
  });

  it("preserves normal c2c/s2c fan-out and ignores malformed/binary frames", async () => {
    const { ws, proxy } = await connect();
    const firstReader = proxy.getReadStream().getReader();
    const secondReader = proxy.getReadStream().getReader();
    ws.receive("not-json");
    ws.receive(new Uint8Array([1, 2, 3]));

    const ownC2C = c2c(identity, "own-c2c");
    const ownS2C = s2c(identity, "own-s2c");
    ws.receiveJSON(ownC2C);
    ws.receiveJSON(ownS2C);
    for (const expected of [ownC2C, ownS2C]) {
      expect((await firstReader.read()).value).toEqual(expected);
      expect((await secondReader.read()).value).toEqual(expected);
    }

    await firstReader.cancel();
    await secondReader.cancel();
  });
});
