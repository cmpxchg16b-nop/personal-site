"use client";

/**
 * SSProxy is the browser-side transport to the signalling server: it
 * wraps one connected WebSocket and vends it as native streams — outbound
 * SignallingEvents on a single WritableStream, inbound ones fanned out to
 * per-consumer ReadableStreams: every getReadStream() call returns a brand
 * new stream that duplicates every SS message, so any number of consumers
 * (e.g. useSignalling and a PerfectNegotiator) can read concurrently and
 * never touch the socket themselves. It carries no protocol logic beyond
 * JSON framing.
 *
 * A single module-level singleton is managed by getSSProxy(): the first
 * call connects and every later call shares the same instance until the
 * connection drops. On disconnect — whatever the reason — the SSProxy
 * destroys itself (the read stream closes, writes start failing), and the
 * next getSSProxy() call builds a fresh one.
 */

import type { SignallingEvent } from "./types";

/**
 * The default signalling server endpoint. A path is resolved against the
 * page origin by the WebSocket constructor (http→ws upgrade implied); in
 * development next.config.ts proxies /api/* to the Go server.
 */
export const DEFAULT_SS_URL = "/api/ss/ws";

export class SSProxy {
  /**
   * Assignable disconnect notification, fired exactly once when the
   * connection drops (for any reason), after the proxy has torn itself
   * down. Consumers — e.g. the React provider — use it to swap to a fresh
   * instance from getSSProxy().
   */
  ondisconnect: (() => void) | null = null;

  private readonly ws: WebSocket;
  private readonly writeStream: WritableStream<SignallingEvent>;
  // Every getReadStream() call registers one subscriber controller;
  // inbound events are duplicated to all of them.
  private readonly subscribers = new Set<
    ReadableStreamDefaultController<SignallingEvent>
  >();
  private destroyedFlag = false;

  /**
   * Wraps an already-connected WebSocket (connecting one is
   * connectWebSocket's job — the WebSocket constructor can throw
   * synchronously), taking over its close and message handlers; the proxy
   * owns the socket from here on.
   */
  constructor(ws: WebSocket) {
    this.ws = ws;

    this.writeStream = new WritableStream<SignallingEvent>({
      write: (ev) => this.send(ev),
    });

    ws.onclose = () => this.destroy();
    ws.onmessage = (m) => {
      if (this.destroyedFlag || typeof m.data !== "string") {
        return;
      }
      let ev: SignallingEvent;
      try {
        ev = JSON.parse(m.data) as SignallingEvent;
      } catch {
        return; // malformed frames are dropped, mirroring the server side
      }
      for (const sub of this.subscribers) {
        try {
          sub.enqueue(ev);
        } catch {
          // the subscriber cancelled between its cancel callback and here
        }
      }
    };
  }

  /**
   * Returns a brand new ReadableStream carrying a copy of every inbound
   * signalling event from now on; any number of consumers can read
   * concurrently. Cancelling the stream unsubscribes it — the socket keeps
   * serving the others. A stream created after destruction closes
   * immediately.
   */
  getReadStream(): ReadableStream<SignallingEvent> {
    let controller: ReadableStreamDefaultController<SignallingEvent> | null =
      null;
    return new ReadableStream<SignallingEvent>({
      start: (c) => {
        controller = c;
        if (this.destroyedFlag) {
          c.close();
        } else {
          this.subscribers.add(c);
        }
      },
      cancel: () => {
        if (controller) {
          this.subscribers.delete(controller);
        }
      },
    });
  }

  /**
   * Outbound signalling events; writes reject once the socket is no
   * longer open.
   */
  getWriteStream(): WritableStream<SignallingEvent> {
    return this.writeStream;
  }

  /** True once the connection has dropped and this proxy is unusable. */
  get destroyed(): boolean {
    return this.destroyedFlag;
  }

  /**
   * Closes the connection and destroys the proxy (ondisconnect fires, the
   * read streams close, writes start failing). Idempotent.
   */
  close(): void {
    this.destroy();
  }

  private send(ev: SignallingEvent): void {
    if (this.destroyedFlag || this.ws.readyState !== WebSocket.OPEN) {
      throw new Error("SSProxy: the connection is not open");
    }
    this.ws.send(JSON.stringify(ev));
  }

  /**
   * Tears the connection down exactly once: closes every subscriber stream
   * (so pending readers see done), makes writes fail, closes the socket,
   * then notifies ondisconnect.
   */
  private destroy(): void {
    if (this.destroyedFlag) return;
    this.destroyedFlag = true;
    for (const sub of this.subscribers) {
      try {
        sub.close();
      } catch {
        // already closed
      }
    }
    this.subscribers.clear();
    try {
      this.ws.close();
    } catch {
      // ignore
    }
    this.ondisconnect?.();
  }
}

// connectWebSocket opens a WebSocket to url and resolves with it once the
// connection is established; it rejects when the connection fails before
// that. The WebSocket constructor itself can throw synchronously (e.g. on
// an unparseable URL) — that is converted to a rejection, so callers only
// ever deal with the promise.
function connectWebSocket(url: string | URL): Promise<WebSocket> {
  return new Promise<WebSocket>((resolve, reject) => {
    let ws: WebSocket;
    try {
      ws = new WebSocket(url);
    } catch (err) {
      reject(err);
      return;
    }
    ws.onopen = () => {
      // Detach the connection-phase handlers before handing the socket
      // over; the SSProxy assigns its own.
      ws.onopen = ws.onerror = ws.onclose = null;
      resolve(ws);
    };
    const fail = (what: string) =>
      reject(new Error(`SSProxy: connecting to ${ws.url} failed: ${what}`));
    ws.onerror = () => fail("websocket error");
    ws.onclose = (e) => fail(`closed before open (code ${e.code})`);
  });
}

// A Connection is one connection attempt: the promise handed out to
// callers, plus the SSProxy it produced — null while the WebSocket is
// still connecting.
interface Connection {
  proxy: SSProxy | null;
  promise: Promise<SSProxy>;
}

// The module-level singleton, shared by every getSSProxy() caller until
// the proxy dies.
let current: Connection | null = null;

/**
 * Returns the singleton SSProxy, connecting first if needed: the promise
 * resolves once the WebSocket is established. Concurrent callers share
 * one connection attempt. After a disconnect — or a failed attempt — the
 * next call builds a fresh instance.
 *
 * `url` is used only when a new connection is opened; while a live
 * singleton exists it is returned as-is, whatever URL is passed.
 */
export function getSSProxy(
  url: string | URL = DEFAULT_SS_URL,
): Promise<SSProxy> {
  // Share the live instance — or the in-flight attempt, whose proxy is
  // not assigned yet.
  if (current && (current.proxy === null || !current.proxy.destroyed)) {
    return current.promise;
  }
  const promise = connectWebSocket(url).then(
    (ws) => {
      const proxy = new SSProxy(ws);
      // Skip publishing when closeSSProxy() discarded the attempt while
      // it was connecting — it has armed its own close of the result.
      if (current?.promise === promise) {
        current.proxy = proxy;
      }
      return proxy;
    },
    (err: unknown) => {
      // A failed attempt is not cached: the next call starts fresh.
      if (current?.promise === promise) {
        current = null;
      }
      throw err;
    },
  );
  current = { proxy: null, promise };
  return promise;
}

/**
 * Closes and discards the current singleton, if any — e.g. when the
 * session is gone (logout). An in-flight attempt is closed as soon as it
 * produces a proxy. The next getSSProxy() call builds a fresh instance.
 */
export function closeSSProxy(): void {
  const conn = current;
  current = null;
  if (!conn) {
    return;
  }
  if (conn.proxy !== null) {
    conn.proxy.close();
  } else {
    // The attempt is still connecting: close whatever it produces.
    void conn.promise.then(
      (proxy) => proxy.close(),
      () => {}, // a failed attempt needs no closing
    );
  }
}
