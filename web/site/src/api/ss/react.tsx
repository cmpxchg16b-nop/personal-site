"use client";

/**
 * React integration for the signalling server connection.
 *
 * SSProxyProvider — mounted by the /chat segment's layout — owns the
 * SSProxy singleton in React state: it connects on mount, temporarily
 * swaps the provided value to null when the connection drops, and
 * re-provides the fresh instance getSSProxy() reconnects to; a failed
 * connection attempt is retried until one succeeds. Components
 * therefore always see the latest proxy, or null.
 *
 * useSignalling() reads that context. At this first design stage it
 * registers the browser as a subscriber of the main channel (reusing the
 * subscriber id persisted in localStorage, or asking the SS to assign
 * one) and pings the SS once per second, reporting the latest round-trip
 * time. The two activities read their own streams from the proxy — every
 * getReadStream() call returns a stream duplicating the SS messages.
 */

import {
  createContext,
  useContext,
  useEffect,
  useState,
  type ReactNode,
} from "react";

import { fetchProfile, ProfileApiError } from "@/api/profile";
import { DEFAULT_SS_URL, getSSProxy, type SSProxy } from "./proxy";
import {
  ErrorCode,
  WELLKNOWN_CH_ID_MAIN,
  WELLKNOWN_SVC_ID_SS,
  type SignallingEvent,
} from "./types";

// SSProxyContext carries the current SSProxy singleton, or null while no
// connection is established (before the first connect resolves, or in the
// window between a disconnect and the reconnect resolving).
const SSProxyContext = createContext<SSProxy | null>(null);

/**
 * Provides the SSProxy singleton to the tree: connects on mount, swaps
 * the provided value to null while the connection is down, and
 * re-provides the fresh instance getSSProxy() reconnects to.
 *
 * There is no login-state gating here: a failed attempt — an anonymous
 * caller's handshake 401s, or the server is down — is retried until it
 * succeeds, so the provider's only concern is keeping the singleton
 * connected. Callers that require a session check the profile
 * themselves (see useSignalling).
 */
export function SSProxyProvider({
  url = DEFAULT_SS_URL,
  children,
}: {
  url?: string;
  children: ReactNode;
}) {
  const [proxy, setProxy] = useState<SSProxy | null>(null);

  useEffect(() => {
    let live = true;
    let retryTimer: number | undefined;

    const connect = () => {
      getSSProxy(url)
        .then((p) => {
          if (!live) return;
          p.ondisconnect = () => {
            if (!live) return;
            setProxy(null);
            connect();
          };
          setProxy(p);
        })
        .catch((err) => {
          // The attempt failed — the caller may be anonymous (the
          // handshake 401s until a login happens) or the server is
          // restarting: wait and retry getSSProxy() until it succeeds.
          console.error("signalling: connection failed, retrying in 2s", err);
          if (live) {
            retryTimer = window.setTimeout(connect, 2000);
          }
        });
    };
    connect();

    return () => {
      live = false;
      if (retryTimer !== undefined) {
        window.clearTimeout(retryTimer);
      }
    };
  }, [url]);

  return (
    <SSProxyContext.Provider value={proxy}>{children}</SSProxyContext.Provider>
  );
}

// PingStat is one answered ping of the current connection's ping session.
export interface PingStat {
  /** ping id of the ping session this measurement belongs to */
  id: string;
  /** sequence number of the ping whose pong produced this measurement */
  seq: number;
  /** round-trip time of that ping, in milliseconds */
  rtt: number;
  /** wall-clock time (epoch ms) the pong was measured at */
  at: number;
}

export interface SignallingState {
  /**
   * The latest answered ping of the current connection; null while none
   * (no connection yet, or the latest ping went unanswered).
   */
  lastPing: PingStat | null;
}

// PING_INTERVAL_MS is how often the SS is pinged while connected —
// deliberately well below the server's default subscriber aging (10s), so
// the pings double as registration keepalive.
const PING_INTERVAL_MS = 1000;

// PONG_TIMEOUT_MS bounds the wait for one ping's pong; on timeout the RTT
// falls back to null until a later pong recovers it.
const PONG_TIMEOUT_MS = 3000;

// REGISTER_REPLY_TIMEOUT_MS bounds the wait for the SS's registration
// reply.
const REGISTER_REPLY_TIMEOUT_MS = 5000;

// SUBSCRIBER_STORAGE_KEY is the localStorage key holding the subscriber id
// the SS assigned to this browser, reused across reconnects and reloads.
const SUBSCRIBER_STORAGE_KEY = "ss.subscriberId";

function loadStoredSubscriberId(): string | null {
  try {
    return window.localStorage.getItem(SUBSCRIBER_STORAGE_KEY);
  } catch {
    return null; // storage unavailable (e.g. private mode)
  }
}

function storeSubscriberId(id: string | null): void {
  try {
    if (id === null) {
      window.localStorage.removeItem(SUBSCRIBER_STORAGE_KEY);
    } else {
      window.localStorage.setItem(SUBSCRIBER_STORAGE_KEY, id);
    }
  } catch {
    // storage unavailable; the registration itself still worked
  }
}

// redirectToLogin bounces to the login page, with redirect_if_succeed set
// to the current location so a successful login comes back here.
function redirectToLogin(): void {
  const here =
    window.location.pathname + window.location.search + window.location.hash;
  window.location.assign(
    `/login?redirect_if_succeed=${encodeURIComponent(here)}`,
  );
}

// awaitReplyOn reads stream until the event correlated (via inReplyTo) to
// msgId arrives or the timeout fires; the stream is cancelled — i.e.
// unsubscribed from the proxy — either way.
async function awaitReplyOn(
  stream: ReadableStream<SignallingEvent>,
  msgId: string,
  timeoutMs: number,
): Promise<SignallingEvent | null> {
  const reader = stream.getReader();
  let timeoutId: number | undefined;
  try {
    for (;;) {
      const raced = await Promise.race([
        reader.read(),
        new Promise<null>((resolve) => {
          timeoutId = window.setTimeout(() => resolve(null), timeoutMs);
        }),
      ]);
      if (raced === null || raced.done) {
        return null;
      }
      if (raced.value.inReplyTo === msgId) {
        return raced.value;
      }
    }
  } finally {
    if (timeoutId !== undefined) {
      window.clearTimeout(timeoutId);
    }
    await reader.cancel();
  }
}

// registerSubscriber registers this browser as a subscriber of the main
// channel, reading the SS's reply on its own short-lived stream: the
// stored subscriber id is reused when present, an empty id asks the SS to
// assign one (from the 1000-1999 range) which is then persisted for
// reuse. A stored id the SS rejects as bound to another session is
// forgotten and retried once with an empty id.
async function registerSubscriber(
  read: () => ReadableStream<SignallingEvent>,
  send: (ev: SignallingEvent) => Promise<void>,
  username: string,
): Promise<void> {
  let subscriberId = loadStoredSubscriberId() ?? "";
  for (let attempt = 0; attempt < 2; attempt++) {
    const msgId = crypto.randomUUID();
    const replyPromise = awaitReplyOn(read(), msgId, REGISTER_REPLY_TIMEOUT_MS);
    await send({
      // `from` is populated server-side from the caller's session.
      from: {},
      to: { serviceId: WELLKNOWN_SVC_ID_SS },
      msgId,
      c2SEv: {
        register: {
          subscriberId,
          channelId: WELLKNOWN_CH_ID_MAIN,
          username,
        },
      },
    });
    const reply = await replyPromise;
    if (!reply) {
      console.warn("signalling: registration timed out");
      return;
    }
    const result = reply.s2CEv?.registerResult;
    if (result) {
      if (result.subscriberId !== subscriberId) {
        storeSubscriberId(result.subscriberId);
      }
      console.info(
        `signalling: registered as subscriber ${result.subscriberId} (${username})`,
      );
      return;
    }
    const err = reply.s2CEv?.err;
    if (
      err?.errorCode === ErrorCode.SubscriberIdIsRegistered &&
      subscriberId !== ""
    ) {
      // The stored id is still bound to another (e.g. older, not yet aged
      // out) session: forget it and mint a fresh one.
      storeSubscriberId(null);
      subscriberId = "";
      continue;
    }
    console.error("signalling: registration failed", err ?? reply);
    return;
  }
}

/**
 * Reads the SSProxy from context. While connected it registers the browser
 * as a subscriber of the main channel (see registerSubscriber) and pings
 * the SS every PING_INTERVAL_MS — one ping session per connection: a fixed
 * ping id, the seq chaining rule from the prototype (the next ping's seq
 * is the ack of the last reply), at most one ping in flight — reporting
 * the latest round-trip time. Registration and the ping loop each read
 * their own stream from the proxy.
 */
export function useSignalling(): SignallingState {
  const proxy = useContext(SSProxyContext);
  // The last answered ping is recorded together with the proxy it was
  // measured on, so a stale value never outlives its connection: after a
  // disconnect or reconnect the pair no longer matches and lastPing
  // derives to null.
  const [measured, setMeasured] = useState<{
    proxy: SSProxy;
    ping: PingStat;
  } | null>(null);

  useEffect(() => {
    // Everything this hook does needs an authenticated session (the SS
    // endpoint is not on the JWT whitelist — an anonymous caller's WS
    // handshake fails with a 401). Check the profile up front and bounce
    // anonymous callers to the login page, instead of letting them stare
    // at a disconnected page. The abort controller cancels the fetch on
    // unmount — StrictMode's mount-unmount-mount would otherwise fire it
    // twice.
    const abort = new AbortController();
    fetchProfile(abort.signal).catch((err) => {
      if (err instanceof DOMException && err.name === "AbortError") return;
      if (err instanceof ProfileApiError && err.status === 401) {
        redirectToLogin();
      }
    });
    return () => abort.abort();
  }, []);

  useEffect(() => {
    if (!proxy) {
      return;
    }
    let live = true;
    const abort = new AbortController();
    // The ping loop's own stream; registration reads another one.
    const reader = proxy.getReadStream().getReader();
    const writer = proxy.getWriteStream().getWriter();

    const pingId = crypto.randomUUID();
    let nextSeq = 1;
    // The one in-flight ping, if any. Read it through getPending():
    // pending is assigned from doPing, and TS's flow analysis would
    // otherwise mis-narrow it to null inside the closures below.
    let pending: { msgId: string; seq: number; t0: number } | null = null;
    const getPending = () => pending;

    const doPing = async () => {
      if (!live || pending) {
        return; // the previous ping is still unanswered
      }
      const msgId = crypto.randomUUID();
      const seq = nextSeq;
      const ping: SignallingEvent = {
        // `from` is populated server-side from the caller's session.
        from: {},
        to: { serviceId: WELLKNOWN_SVC_ID_SS },
        msgId,
        c2SEv: { ping: { pingId, sequenceNumber: seq, ackSequenceNumber: 0 } },
      };
      try {
        await writer.write(ping);
      } catch (err) {
        if (live) console.error("signalling: ping write failed", err);
        return;
      }
      pending = { msgId, seq, t0: performance.now() };
      window.setTimeout(() => {
        if (live && getPending()?.msgId === msgId) {
          pending = null;
          setMeasured(null);
          console.warn("signalling: SS pong timed out");
        }
      }, PONG_TIMEOUT_MS);
    };

    void (async () => {
      try {
        for (;;) {
          const { done, value: ev } = await reader.read();
          if (done || !live) return;
          const pong = ev.s2CEv?.pong;
          const p = getPending();
          if (
            !pong ||
            !p ||
            pong.pingId !== pingId ||
            ev.inReplyTo !== p.msgId
          ) {
            continue;
          }
          setMeasured({
            proxy,
            ping: {
              id: pingId,
              seq: p.seq,
              rtt: performance.now() - p.t0,
              at: Date.now(),
            },
          });
          if (pong.ackSequenceNumber === p.seq + 1) {
            nextSeq = pong.ackSequenceNumber;
          }
          pending = null;
        }
      } catch (err) {
        if (live) console.error("signalling: read failed", err);
      }
    })();

    // Register the subscriber, and keep measuring latency. Registration
    // is best-effort: a failure is logged and never stops the pings.
    void (async () => {
      let username = "";
      try {
        username = (await fetchProfile(abort.signal)).username.trim();
      } catch (err) {
        if (err instanceof DOMException && err.name === "AbortError") return;
        // The session is gone (e.g. expired or logged out elsewhere
        // mid-connection): bounce to the login page.
        if (err instanceof ProfileApiError && err.status === 401) {
          redirectToLogin();
          return;
        }
        console.error(
          "signalling: cannot load /api/profile for registration",
          err,
        );
      }
      if (!live || username === "") {
        return;
      }
      try {
        await registerSubscriber(
          () => proxy.getReadStream(),
          (ev) => writer.write(ev),
          username,
        );
      } catch (err) {
        if (live) console.error("signalling: registration failed", err);
      }
    })();

    void doPing();
    const intervalId = window.setInterval(doPing, PING_INTERVAL_MS);

    return () => {
      live = false;
      abort.abort();
      window.clearInterval(intervalId);
      // Cancelling the reader also unsubscribes its stream from the proxy.
      void reader.cancel();
      writer.releaseLock();
    };
  }, [proxy]);

  return {
    lastPing:
      proxy !== null && measured?.proxy === proxy ? measured.ping : null,
  };
}
