import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const mocks = vi.hoisted(() => ({
  runtime: { current: null as unknown },
}));

vi.mock("react", () => ({
  createContext: () => ({
    Provider: ({ children }: { children?: unknown }) => children ?? null,
  }),
  useContext: () => null,
  useCallback: (fn: unknown) => fn,
  useMemo: (factory: () => unknown) => factory(),
  useEffect: (effect: Effect, deps: readonly unknown[]) =>
    (mocks.runtime.current as HookRuntime).effect(effect, deps),
  useRef: <T,>(initial: T) =>
    (mocks.runtime.current as HookRuntime).ref(initial),
  useState: <T,>(initial: T) =>
    (mocks.runtime.current as HookRuntime).state(initial),
}));

vi.mock("@/api/profile", () => ({
  fetchProfile: async () => ({ username: "browser", subject_id: "user" }),
  ProfileApiError: class ProfileApiError extends Error {},
}));

import { closeSSProxy, getSSProxy } from "./proxy";
import { SSProxyProvider } from "./react";
import { WELLKNOWN_SVC_ID_SS, type SignallingEvent } from "./types";

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

// Minimal provider-only hook runtime. State updates deliberately do not render:
// reconnect and shutdown are driven by proxy callbacks, not by React effects.
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
    this.index = 0;
    this.changedEffects = [];
    // Deliberate direct invocation: this class is the test's hook runtime.
    SSProxyProvider({ children: null });
    for (const index of this.changedEffects) {
      const cleanup = this.slots[index].cleanup;
      if (typeof cleanup === "function") cleanup();
    }
    for (const index of this.changedEffects) {
      const slot = this.slots[index];
      slot.cleanup = slot.effect?.();
    }
  }

  unmount(): void {
    for (const slot of this.slots) {
      if (typeof slot.cleanup === "function") slot.cleanup();
      slot.cleanup = undefined;
    }
  }
}

class MockWebSocket {
  static readonly OPEN = 1;
  static instances: MockWebSocket[] = [];

  readonly url: string;
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

  send(): void {}

  close = vi.fn((): void => {
    if (this.readyState === 3) return;
    this.readyState = 3;
    this.onclose?.({ code: 1000 });
  });
}

function stubWindow(): void {
  const values = new Map<string, string>();
  vi.stubGlobal("window", {
    localStorage: {
      getItem: (key: string) => values.get(key) ?? null,
      setItem: (key: string, value: string) => values.set(key, value),
      removeItem: (key: string) => values.delete(key),
    },
    setTimeout: globalThis.setTimeout.bind(globalThis),
    clearTimeout: globalThis.clearTimeout.bind(globalThis),
  });
}

function hello(): SignallingEvent {
  const endpoint = { userId: "user", userSessionId: "session" };
  return {
    from: { serviceId: WELLKNOWN_SVC_ID_SS },
    to: endpoint,
    msgId: "hello-message",
    s2CEv: { endpoint },
  };
}

async function flushMicrotasks(): Promise<void> {
  for (let i = 0; i < 10; i++) await Promise.resolve();
}

function latestSocket(): MockWebSocket {
  const ws = MockWebSocket.instances.at(-1);
  if (ws === undefined) throw new Error("no WebSocket was constructed");
  return ws;
}

async function completeHello(ws: MockWebSocket): Promise<void> {
  ws.open();
  await flushMicrotasks();
  ws.receiveJSON(hello());
  await flushMicrotasks();
}

let runtime: HookRuntime;

beforeEach(() => {
  MockWebSocket.instances = [];
  runtime = new HookRuntime();
  mocks.runtime.current = runtime;
  stubWindow();
  vi.stubGlobal("WebSocket", MockWebSocket);
});

afterEach(async () => {
  runtime.unmount();
  closeSSProxy();
  await flushMicrotasks();
  vi.useRealTimers();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
});

describe("SSProxyProvider shutdown semantics", () => {
  it("reconnects after an ordinary proxy close", async () => {
    runtime.render();
    const first = latestSocket();
    await completeHello(first);
    const firstProxy = await getSSProxy();

    firstProxy.close();
    await flushMicrotasks();
    expect(MockWebSocket.instances).toHaveLength(2);
    const second = latestSocket();
    await completeHello(second);
    expect(await getSSProxy()).not.toBe(firstProxy);
  });

  it("does not reconnect after closeSSProxy on a live proxy", async () => {
    runtime.render();
    const ws = latestSocket();
    await completeHello(ws);

    closeSSProxy();
    await flushMicrotasks();
    expect(ws.close).toHaveBeenCalledTimes(1);
    expect(MockWebSocket.instances).toHaveLength(1);
  });

  it("does not reconnect when an in-flight hello completes after shutdown", async () => {
    runtime.render();
    const ws = latestSocket();
    ws.open();
    await flushMicrotasks();
    const attempt = getSSProxy();

    closeSSProxy();
    ws.receiveJSON(hello());
    const proxy = await attempt;
    await flushMicrotasks();
    expect(proxy.destroyed).toBe(true);
    expect(MockWebSocket.instances).toHaveLength(1);
  });

  it("cancels an already scheduled retry on shutdown", async () => {
    vi.useFakeTimers();
    stubWindow();
    vi.spyOn(console, "error").mockImplementation(() => {});
    runtime.render();
    const ws = latestSocket();
    ws.open();
    await flushMicrotasks();
    const attempt = getSSProxy();
    const rejection = expect(attempt).rejects.toThrow("endpoint hello failed");
    ws.receive("not-json");
    await rejection;
    await flushMicrotasks();

    closeSSProxy();
    await vi.advanceTimersByTimeAsync(2000);
    expect(MockWebSocket.instances).toHaveLength(1);
  });
});
