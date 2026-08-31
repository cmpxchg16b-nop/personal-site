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
 * one), renews the membership with a channel keepalive every few
 * seconds, renews the server's channel list every five seconds —
 * resolving each channel's profile and its members' profiles into
 * ChatChannels — and pings the SS once per second, reporting the latest
 * round-trip time. The activities read their own streams from the proxy
 * — every getReadStream() call returns a stream duplicating the SS
 * messages.
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
  type ChannelId,
  type ChatChannel,
  type ChatUser,
  type SignallingEvent,
  type SubscriberId,
  type UserId,
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

// useSSProxy reads the SSProxy provided by SSProxyProvider: the current
// singleton connection, or null while none is established (before the
// first connect resolves, or between a disconnect and its reconnect).
export function useSSProxy(): SSProxy | null {
  return useContext(SSProxyContext);
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
  /**
   * The current user: `id` is the app-level user id the SS echoes in
   * every reply's `to` (populated server-side from the caller's
   * session), `name` the profile username, plus the signalling
   * subscription (main channel, assigned subscriber id); null while not
   * registered (no connection yet, or registration failed).
   */
  me: ChatUser | null;
  /**
   * The channels of the signalling server — id, name, and members with
   * resolved profiles (excluding the current user) — renewed every
   * CHANNEL_LIST_INTERVAL_MS while connected; empty while no listing is
   * available (no connection yet, or no round has succeeded).
   */
  channels: ChatChannel[];
}

// NO_CHANNELS is the stable empty list SignallingState.channels falls
// back to, so consumers can depend on it without re-running effects on
// every render.
const NO_CHANNELS: ChatChannel[] = [];

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

// CHANNEL_LIST_TIMEOUT_MS bounds the wait for one listing round's page
// reads (channel list, member list) and each of its profile replies.
const CHANNEL_LIST_TIMEOUT_MS = 5000;

// CHANNEL_LIST_INTERVAL_MS is how often the channel list is renewed while
// connected.
const CHANNEL_LIST_INTERVAL_MS = 5000;

// CHANNEL_KEEPALIVE_INTERVAL_MS is how often the browser renews its
// channel membership once registered — deliberately well below the
// server's default subscriber aging (10s), like the pings.
const CHANNEL_KEEPALIVE_INTERVAL_MS = 5000;

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

// awaitRepliesOn is the paged-reply counterpart of awaitReplyOn: it
// collects every event correlated (via inReplyTo) to msgId until `done`
// accepts one of them or the timeout fires; the stream is cancelled —
// i.e. unsubscribed from the proxy — either way.
async function awaitRepliesOn(
  stream: ReadableStream<SignallingEvent>,
  msgId: string,
  timeoutMs: number,
  done: (ev: SignallingEvent) => boolean,
): Promise<SignallingEvent[]> {
  const reader = stream.getReader();
  const collected: SignallingEvent[] = [];
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
        return collected;
      }
      if (raced.value.inReplyTo !== msgId) {
        continue;
      }
      collected.push(raced.value);
      if (done(raced.value)) {
        return collected;
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
// forgotten and retried once with an empty id. The registration's display
// name is not the client's to choose: the server stamps the session's
// username onto the register event, so the wire field goes empty. It
// resolves to the registered subscriber id and the caller's user id —
// which the SS echoes in the reply's `to`, populated server-side from the
// session — or null when registration failed.
async function registerSubscriber(
  read: () => ReadableStream<SignallingEvent>,
  send: (ev: SignallingEvent) => Promise<void>,
): Promise<{ subscriberId: SubscriberId; userId: UserId | null } | null> {
  let subscriberId = loadStoredSubscriberId() ?? "";
  for (let attempt = 0; attempt < 2; attempt++) {
    const msgId = crypto.randomUUID();
    const replyPromise = awaitReplyOn(read(), msgId, REGISTER_REPLY_TIMEOUT_MS);
    await send({
      // `from` is populated server-side from the caller's session; so is
      // the registration's username — the wire value is ignored.
      from: {},
      to: { serviceId: WELLKNOWN_SVC_ID_SS },
      msgId,
      c2SEv: {
        register: {
          subscriberId,
          channelId: WELLKNOWN_CH_ID_MAIN,
          username: "",
        },
      },
    });
    const reply = await replyPromise;
    if (!reply) {
      console.warn("signalling: registration timed out");
      return null;
    }
    const result = reply.s2CEv?.registerResult;
    if (result) {
      if (result.subscriberId !== subscriberId) {
        storeSubscriberId(result.subscriberId);
      }
      console.info(
        `signalling: registered as subscriber ${result.subscriberId}`,
      );
      return {
        subscriberId: result.subscriberId,
        userId: reply.to.userId ?? null,
      };
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
    return null;
  }
  return null;
}

// listChannels discovers the server's channels, resolving each into a
// ChatChannel: one listChannels request, reading pages until hasMore is
// false (an err page stops the read early), then listChannel per
// discovered id, all in parallel. Channels whose resolution fails are
// dropped from the result.
async function listChannels(
  read: () => ReadableStream<SignallingEvent>,
  send: (ev: SignallingEvent) => Promise<void>,
  selfSubscriberId: SubscriberId | null,
): Promise<ChatChannel[]> {
  const listMsgId = crypto.randomUUID();
  const pagesPromise = awaitRepliesOn(
    read(),
    listMsgId,
    CHANNEL_LIST_TIMEOUT_MS,
    (ev) =>
      ev.s2CEv?.err !== undefined ||
      ev.s2CEv?.channelListResult?.hasMore === false,
  );
  await send({
    // `from` is populated server-side from the caller's session.
    from: {},
    to: { serviceId: WELLKNOWN_SVC_ID_SS },
    msgId: listMsgId,
    c2SEv: { listChannels: {} },
  });
  const ids = (await pagesPromise).flatMap(
    (p) => p.s2CEv?.channelListResult?.channels ?? [],
  );
  const channels = await Promise.all(
    ids.map((channelId) =>
      listChannel(read, send, channelId, selfSubscriberId),
    ),
  );
  return channels.filter((c): c is ChatChannel => c !== null);
}

// listChannel resolves one channel id into a ChatChannel: a
// channelProfileQuery for the name, a paged listChannelMembers for the
// member ids (reading pages until hasMore is false, an err page stops
// the read early), and one userProfileQuery per member for its username
// — the member queries in parallel. The current user is filtered out of
// the members: you don't open a direct message with yourself. Returns
// null when the channel profile query fails or times out (e.g. the
// channel went away between listing and querying); members whose profile
// query fails are dropped.
async function listChannel(
  read: () => ReadableStream<SignallingEvent>,
  send: (ev: SignallingEvent) => Promise<void>,
  channelId: ChannelId,
  selfSubscriberId: SubscriberId | null,
): Promise<ChatChannel | null> {
  const profileMsgId = crypto.randomUUID();
  const profilePromise = awaitReplyOn(
    read(),
    profileMsgId,
    CHANNEL_LIST_TIMEOUT_MS,
  );
  await send({
    from: {},
    to: { serviceId: WELLKNOWN_SVC_ID_SS },
    msgId: profileMsgId,
    c2SEv: { channelProfileQuery: { channelId } },
  });
  const profile = (await profilePromise)?.s2CEv?.channelProfile;
  if (!profile) {
    return null;
  }

  const mbsMsgId = crypto.randomUUID();
  const mbsPromise = awaitRepliesOn(
    read(),
    mbsMsgId,
    CHANNEL_LIST_TIMEOUT_MS,
    (ev) =>
      ev.s2CEv?.err !== undefined ||
      ev.s2CEv?.channelMbsListResult?.hasMore === false,
  );
  await send({
    from: {},
    to: { serviceId: WELLKNOWN_SVC_ID_SS },
    msgId: mbsMsgId,
    c2SEv: { listChannelMembers: { channelId } },
  });
  const memberIds = (await mbsPromise)
    .flatMap((p) => p.s2CEv?.channelMbsListResult?.members ?? [])
    .filter((id) => id !== selfSubscriberId);

  const members = await Promise.all(
    memberIds.map(async (subscriberId) => {
      const msgId = crypto.randomUUID();
      const replyPromise = awaitReplyOn(read(), msgId, CHANNEL_LIST_TIMEOUT_MS);
      await send({
        from: {},
        to: { serviceId: WELLKNOWN_SVC_ID_SS },
        msgId,
        c2SEv: { userProfileQuery: { subscriberId, channelId } },
      });
      const userProfile = (await replyPromise)?.s2CEv?.profile;
      if (!userProfile) {
        return null;
      }
      const user: ChatUser = {
        // The wire carries no app-level user identity yet, so the
        // subscriber id — all the SS knows — doubles as the chat user id.
        id: userProfile.subscriberId,
        name: userProfile.username,
        // A listed member is online: the SS sweeps expired registrations
        // before answering listChannelMembers.
        online: true,
        channelId: userProfile.channelId,
        subscriberId: userProfile.subscriberId,
      };
      return user;
    }),
  );
  return {
    id: profile.channelId,
    name: profile.channelName,
    members: members.filter((m): m is ChatUser => m !== null),
  };
}

// sameChannels reports whether two listings carry the same channels —
// the same (id, name) pairs, each with the same members in the same
// order. The server lists channels sorted by channel id and members
// sorted by subscriber id, so the order is stable across rounds.
function sameChannels(a: ChatChannel[], b: ChatChannel[]): boolean {
  return (
    a.length === b.length &&
    a.every((c, i) => {
      const o = b[i];
      return (
        o !== undefined &&
        c.id === o.id &&
        c.name === o.name &&
        c.members.length === o.members.length &&
        c.members.every(
          (m, j) => m.id === o.members[j]?.id && m.name === o.members[j]?.name,
        )
      );
    })
  );
}

/**
 * Reads the SSProxy from context. While connected it registers the browser
 * as a subscriber of the main channel (see registerSubscriber), renews the
 * membership with a channel keepalive every CHANNEL_KEEPALIVE_INTERVAL_MS,
 * renews the server's channel list every CHANNEL_LIST_INTERVAL_MS (see
 * listChannels), and pings the SS every PING_INTERVAL_MS — one ping session
 * per connection:
 * a fixed ping id, the seq chaining rule from the prototype (the next ping's
 * seq is the ack of the last reply), at most one ping in flight — reporting
 * the latest round-trip time. A successful registration also exposes the
 * current user (`me`): its id is the user id the SS echoes in the register
 * reply's `to`. Registration, the channel listing, and the ping loop each
 * read their own stream from the proxy.
 */
export function useSignalling(): SignallingState {
  const proxy = useSSProxy();
  // The last answered ping is recorded together with the proxy it was
  // measured on, so a stale value never outlives its connection: after a
  // disconnect or reconnect the pair no longer matches and lastPing
  // derives to null.
  const [measured, setMeasured] = useState<{
    proxy: SSProxy;
    ping: PingStat;
  } | null>(null);
  // The current user follows the same rule as lastPing: recorded together
  // with the proxy the registration happened on, so a stale identity never
  // outlives its connection.
  const [registered, setRegistered] = useState<{
    proxy: SSProxy;
    me: ChatUser;
  } | null>(null);
  // The channel listing follows the same rule as lastPing: recorded
  // together with the proxy it was discovered on, so a stale listing
  // never outlives its connection.
  const [listed, setListed] = useState<{
    proxy: SSProxy;
    channels: ChatChannel[];
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
    // is best-effort: a failure is logged and never stops the pings or
    // the listing. The promise resolves to this browser's registration
    // (null when registration failed); listing rounds await it so the
    // current user is excluded from the members from the first round on.
    const registration: Promise<{
      subscriberId: SubscriberId;
      userId: UserId | null;
    } | null> = (async () => {
      // The login guard: registration needs an authenticated session (the
      // WS handshake is not on the JWT whitelist), and a session can die
      // mid-connection — expired or logged out elsewhere. Check the
      // profile before registering; a 401 bounces to the login page. The
      // profile's username is only reused below as `me`'s display name —
      // the registration itself carries no username: the server stamps
      // the session's own.
      let name = "";
      try {
        const profile = await fetchProfile(abort.signal);
        name = profile.username.trim() || profile.subject_id;
      } catch (err) {
        if (err instanceof DOMException && err.name === "AbortError")
          return null;
        // The session is gone (e.g. expired or logged out elsewhere
        // mid-connection): bounce to the login page.
        if (err instanceof ProfileApiError && err.status === 401) {
          redirectToLogin();
          return null;
        }
        console.error(
          "signalling: cannot load /api/profile for registration",
          err,
        );
        return null;
      }
      if (!live) {
        return null;
      }
      try {
        const reg = await registerSubscriber(
          () => proxy.getReadStream(),
          (ev) => writer.write(ev),
        );
        if (live && reg !== null && reg.userId !== null) {
          setRegistered({
            proxy,
            me: {
              id: reg.userId,
              name,
              online: true,
              channelId: WELLKNOWN_CH_ID_MAIN,
              subscriberId: reg.subscriberId,
            },
          });
        }
        return reg;
      } catch (err) {
        if (live) console.error("signalling: registration failed", err);
        return null;
      }
    })();

    // Renew the channel membership every CHANNEL_KEEPALIVE_INTERVAL_MS,
    // starting once the registration resolves. Fire-and-forget: the SS
    // answers nothing on success (an err reply — meaning the membership
    // is gone — is only visible to stream readers, none of which listen
    // for it at this design stage).
    let keepAliveIntervalId: number | undefined;
    void (async () => {
      const reg = await registration;
      if (!live || reg === null) {
        return;
      }
      const keepAlive = () => {
        const msgId = crypto.randomUUID();
        writer
          .write({
            // `from` is populated server-side from the caller's session.
            from: {},
            to: { serviceId: WELLKNOWN_SVC_ID_SS },
            msgId,
            c2SEv: {
              channelKeepAlive: {
                channelId: WELLKNOWN_CH_ID_MAIN,
                subscriberId: reg.subscriberId,
              },
            },
          })
          .then(() => {
            if (live)
              console.log(
                `signalling: channel keepalive sent ` +
                  `(channel_id=${WELLKNOWN_CH_ID_MAIN} ` +
                  `subscriber=${reg.subscriberId} msg_id=${msgId})`,
              );
          })
          .catch((err) => {
            if (live)
              console.error("signalling: channel keepalive write failed", err);
          });
      };
      keepAlive();
      keepAliveIntervalId = window.setInterval(
        keepAlive,
        CHANNEL_KEEPALIVE_INTERVAL_MS,
      );
    })();

    // Renew the channel listing every CHANNEL_LIST_INTERVAL_MS. Best-effort:
    // a failed round is logged and leaves the previous listing in place.
    let listing = false;
    const doListChannels = async () => {
      if (!live || listing) {
        return; // the previous round is still running
      }
      listing = true;
      try {
        const reg = await registration;
        const channels = await listChannels(
          () => proxy.getReadStream(),
          (ev) => writer.write(ev),
          reg?.subscriberId ?? null,
        );
        if (!live) return;
        // Keep the previous listing's identity when nothing changed, so an
        // unchanged round never re-renders consumers.
        setListed((prev) =>
          prev !== null &&
          prev.proxy === proxy &&
          sameChannels(prev.channels, channels)
            ? prev
            : { proxy, channels },
        );
      } catch (err) {
        if (live) console.error("signalling: channel listing failed", err);
      } finally {
        listing = false;
      }
    };
    void doListChannels();
    const listIntervalId = window.setInterval(
      doListChannels,
      CHANNEL_LIST_INTERVAL_MS,
    );

    void doPing();
    const intervalId = window.setInterval(doPing, PING_INTERVAL_MS);

    return () => {
      live = false;
      abort.abort();
      window.clearInterval(intervalId);
      window.clearInterval(listIntervalId);
      if (keepAliveIntervalId !== undefined) {
        window.clearInterval(keepAliveIntervalId);
      }
      // Cancelling the reader also unsubscribes its stream from the proxy.
      void reader.cancel();
      writer.releaseLock();
    };
  }, [proxy]);

  return {
    lastPing:
      proxy !== null && measured?.proxy === proxy ? measured.ping : null,
    me: proxy !== null && registered?.proxy === proxy ? registered.me : null,
    channels:
      proxy !== null && listed?.proxy === proxy ? listed.channels : NO_CHANNELS,
  };
}
