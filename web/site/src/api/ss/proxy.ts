"use client";

/**
 * SSProxy is the browser-side transport to the signalling server: it
 * wraps one connected WebSocket and vends it as native streams — inbound
 * SignallingEvents fanned out to per-consumer ReadableStreams: every
 * getReadStream() call returns a brand new stream that duplicates every
 * SS message, so any number of consumers (e.g. useSignalling and a
 * PerfectNegotiator) can read concurrently and never touch the socket
 * themselves; outbound ones fanned in from per-consumer WritableStreams:
 * every getWriteStream() call returns a brand new stream whose writes all
 * funnel into the socket, so any number of writers can hold a lock
 * concurrently. Besides JSON framing, it exposes the identity validated by
 * the connection hello and drops flooded events addressed to anyone else.
 *
 * A single module-level singleton is managed by getSSProxy(): the first
 * call connects and every later call shares the same instance until the
 * connection drops. On disconnect — whatever the reason — the SSProxy
 * destroys itself (the read stream closes, writes start failing), and the
 * next getSSProxy() call builds a fresh one.
 */

import {
  isEndpointIdentity,
  readIdentityHint,
  sameEndpointIdentity,
  writeStoredIdentity,
  type EndpointIdentity,
} from "./identity";
import { WELLKNOWN_SVC_ID_SS, type SignallingEvent } from "./types";

/**
 * The default signalling server endpoint. A path is resolved against the
 * page origin by the WebSocket constructor (http→ws upgrade implied); in
 * development next.config.ts proxies /api/* to the Go server.
 */
export const DEFAULT_SS_URL = "/api/ss/ws";

export type SSProxyDisconnectReason = "reconnect" | "shutdown";

export class SSProxy {
  /** The authoritative, immutable session endpoint from this socket's hello. */
  readonly identity: EndpointIdentity;

  /**
   * Assignable disconnect notification, fired exactly once after teardown.
   * `reconnect` covers network loss and the default close(); `shutdown` is an
   * explicit logout/401 close that must not trigger an automatic reconnect.
   */
  ondisconnect: ((reason: SSProxyDisconnectReason) => void) | null = null;

  private readonly ws: WebSocket;
  // Every getReadStream() call registers one subscriber controller;
  // inbound events are duplicated to all of them.
  private readonly subscribers = new Set<
    ReadableStreamDefaultController<SignallingEvent>
  >();
  // Eager CAM can deliver self-addressed c2c after hello but before React's
  // peer demux subscribes. Other readers (ping/registration) must not consume
  // that backlog. The demux acknowledges events as it actually reads them;
  // unacknowledged events survive a StrictMode reader cancellation/re-setup.
  private earlyC2C: SignallingEvent[] = [];
  private earlyC2CReaderActive = false;
  private earlyC2CDone = false;
  private destroyedFlag = false;

  /**
   * Wraps an already-connected WebSocket whose first-frame endpoint hello has
   * been validated, taking over its close and message handlers; the proxy owns
   * the socket from here on.
   */
  constructor(ws: WebSocket, identity: EndpointIdentity) {
    this.ws = ws;
    this.identity = Object.freeze({
      userId: identity.userId,
      userSessionId: identity.userSessionId,
    });

    ws.onclose = () => this.destroy("reconnect");
    ws.onmessage = (m) => {
      if (this.destroyedFlag || typeof m.data !== "string") {
        return;
      }
      let ev: SignallingEvent;
      try {
        const parsed: unknown = JSON.parse(m.data);
        if (parsed === null || typeof parsed !== "object") return;
        ev = parsed as SignallingEvent;
      } catch {
        return; // malformed frames are dropped, mirroring the server side
      }
      if (
        (ev.c2CEv !== undefined || ev.s2CEv !== undefined) &&
        (!isEndpointIdentity(ev.to) ||
          !sameEndpointIdentity(ev.to, this.identity))
      ) {
        return;
      }
      if (ev.c2CEv !== undefined && !this.earlyC2CDone) {
        this.earlyC2C.push(ev);
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
   * concurrently. The peer demux passes replayEarlyC2C to claim self-addressed
   * c2c retained since the hello; ordinary readers never receive that backlog.
   * Cancelling the stream unsubscribes it — the socket keeps serving the
   * others. A stream created after destruction closes immediately.
   */
  getReadStream({
    replayEarlyC2C = false,
  }: { replayEarlyC2C?: boolean } = {}): ReadableStream<SignallingEvent> {
    let controller: ReadableStreamDefaultController<SignallingEvent> | null =
      null;
    let ownsEarlyC2CReader = false;
    return new ReadableStream<SignallingEvent>({
      start: (c) => {
        controller = c;
        if (this.destroyedFlag) {
          c.close();
        } else {
          if (
            replayEarlyC2C &&
            !this.earlyC2CDone &&
            !this.earlyC2CReaderActive
          ) {
            this.earlyC2CReaderActive = true;
            ownsEarlyC2CReader = true;
            for (const ev of this.earlyC2C) c.enqueue(ev);
          }
          this.subscribers.add(c);
        }
      },
      cancel: () => {
        if (controller) {
          this.subscribers.delete(controller);
        }
        if (ownsEarlyC2CReader) {
          this.earlyC2CReaderActive = false;
          ownsEarlyC2CReader = false;
        }
      },
    });
  }

  /**
   * Acknowledges one early c2c event after the peer demux's reader.read has
   * yielded it. Unacknowledged events remain replayable if StrictMode cancels
   * and recreates that reader. Events are compared by identity, matching the
   * exact object produced by the socket's JSON parse.
   */
  acknowledgeEarlyC2C(ev: SignallingEvent): void {
    if (this.earlyC2CDone) return;
    const index = this.earlyC2C.indexOf(ev);
    if (index < 0) return;
    this.earlyC2C.splice(index, 1);
    if (this.earlyC2C.length === 0) {
      this.earlyC2CDone = true;
      this.earlyC2CReaderActive = false;
    }
  }

  /**
   * Outbound signalling events: every call returns a brand new
   * WritableStream whose writes all funnel into the socket — the fan-in
   * counterpart of getReadStream's fan-out, so any number of writers
   * (e.g. useSignalling and several PerfectNegotiators) can write
   * concurrently. Writes reject once the socket is no longer open;
   * closing a stream only retires that writer, never the socket.
   */
  getWriteStream(): WritableStream<SignallingEvent> {
    return new WritableStream<SignallingEvent>({
      write: (ev) => this.send(ev),
    });
  }

  /** True once the connection has dropped and this proxy is unusable. */
  get destroyed(): boolean {
    return this.destroyedFlag;
  }

  /**
   * Closes the connection and destroys the proxy (ondisconnect fires, the
   * read streams close, writes start failing). Idempotent. The default asks
   * the provider to reconnect; closeSSProxy uses `shutdown` instead.
   */
  close(reason: SSProxyDisconnectReason = "reconnect"): void {
    this.destroy(reason);
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
  private destroy(reason: SSProxyDisconnectReason): void {
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
    this.earlyC2C = [];
    try {
      this.ws.close();
    } catch {
      // ignore
    }
    this.ondisconnect?.(reason);
  }
}

// connectWebSocket opens a WebSocket to url and resolves with it once the
// connection is established; it rejects when the connection fails before
// that. The WebSocket constructor itself can throw synchronously (e.g. on
// an unparseable URL) — that is converted to a rejection, so callers only
// ever deal with the promise.
function connectWebSocket(url: string | URL): Promise<WebSocket> {
  console.info(`SSProxy: connecting to ${url}`);
  return new Promise<WebSocket>((resolve, reject) => {
    let ws: WebSocket;
    try {
      ws = new WebSocket(url);
    } catch (err) {
      console.error(`SSProxy: connecting to ${url} threw`, err);
      reject(err);
      return;
    }
    ws.onopen = () => {
      // Detach the connection-phase handlers before handing the socket
      // over; the SSProxy assigns its own.
      ws.onopen = ws.onerror = ws.onclose = null;
      console.info(`SSProxy: connected to ${ws.url}`);
      resolve(ws);
    };
    const fail = (what: string) => {
      console.warn(`SSProxy: connecting to ${ws.url} failed: ${what}`);
      reject(new Error(`SSProxy: connecting to ${ws.url} failed: ${what}`));
    };
    ws.onerror = () => fail("websocket error");
    ws.onclose = (e) => fail(`closed before open (code ${e.code})`);
  });
}

const ENDPOINT_HELLO_TIMEOUT_MS = 5000;

// The first frame must prove the exact session bound to this socket. Ordinary
// events are deliberately not buffered or interpreted before that proof.
function waitForEndpointHello(ws: WebSocket): Promise<EndpointIdentity> {
  return new Promise<EndpointIdentity>((resolve, reject) => {
    let settled = false;
    const cleanup = () => {
      clearTimeout(timeoutId);
      ws.onmessage = ws.onerror = ws.onclose = null;
    };
    const fail = (what: string) => {
      if (settled) return;
      settled = true;
      cleanup();
      try {
        ws.close();
      } catch {
        // The failed handshake is already unusable.
      }
      reject(new Error(`SSProxy: endpoint hello failed: ${what}`));
    };

    ws.onmessage = (m) => {
      if (settled) return;
      let ev: SignallingEvent;
      try {
        if (typeof m.data !== "string") throw new Error("binary first frame");
        const parsed: unknown = JSON.parse(m.data);
        if (parsed === null || typeof parsed !== "object") {
          throw new Error("non-object first frame");
        }
        ev = parsed as SignallingEvent;
      } catch {
        fail("first frame is not a valid event");
        return;
      }
      const endpoint = ev.s2CEv?.endpoint;
      if (
        ev.from?.serviceId !== WELLKNOWN_SVC_ID_SS ||
        typeof ev.msgId !== "string" ||
        ev.msgId.length === 0 ||
        !isEndpointIdentity(ev.to) ||
        !isEndpointIdentity(endpoint) ||
        !sameEndpointIdentity(ev.to, endpoint)
      ) {
        fail("invalid endpoint hello");
        return;
      }
      settled = true;
      cleanup();
      resolve({
        userId: endpoint.userId,
        userSessionId: endpoint.userSessionId,
      });
    };
    ws.onerror = () => fail("websocket error");
    ws.onclose = (e) => fail(`closed before hello (code ${e.code})`);
    const timeoutId = setTimeout(
      () => fail(`timed out after ${ENDPOINT_HELLO_TIMEOUT_MS}ms`),
      ENDPOINT_HELLO_TIMEOUT_MS,
    );
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

// Providers also need the shutdown intent while a connection attempt is still
// in flight, when there is no SSProxy on which to receive a close reason yet.
const shutdownSubscribers = new Set<() => void>();

export function subscribeSSProxyShutdown(cb: () => void): () => void {
  shutdownSubscribers.add(cb);
  return () => {
    shutdownSubscribers.delete(cb);
  };
}

/**
 * Returns the singleton SSProxy, connecting first if needed: the promise
 * resolves once the WebSocket is established and its endpoint hello validated.
 * Concurrent callers share one connection attempt. After a disconnect — or a
 * failed attempt — the next call builds a fresh instance.
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
  // Read the stale-allowed hint before dialing, but never use it to validate
  // this connection or filter its events. The server hello below replaces it.
  readIdentityHint();
  const promise = connectWebSocket(url)
    .then(async (ws) => {
      const identity = await waitForEndpointHello(ws);
      // A discarded logout/401 attempt must not repopulate the cleared cache.
      if (current?.promise === promise) {
        writeStoredIdentity(identity);
      }
      return new SSProxy(ws, identity);
    })
    .then(
      (proxy) => {
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
  for (const cb of [...shutdownSubscribers]) cb();
  if (!conn) {
    return;
  }
  if (conn.proxy !== null) {
    conn.proxy.close("shutdown");
  } else {
    // The attempt is still connecting: close whatever it produces.
    void conn.promise.then(
      (proxy) => proxy.close("shutdown"),
      () => {}, // a failed attempt needs no closing
    );
  }
}
