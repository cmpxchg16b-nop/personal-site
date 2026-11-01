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
  useRef,
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
   * subscription (main channel, assigned subscriber id); null before the
   * first registration or after the current registration fails. Retained
   * while a replacement socket refreshes the same in-page registration.
   */
  me: ChatUser | null;
  /**
   * The channels of the signalling server — id, name, and members with
   * resolved profiles (excluding the current user) — renewed every
   * CHANNEL_LIST_INTERVAL_MS while connected; empty until the first listing
   * or after a registration reset. Retained during socket reconnect.
   */
  channels: ChatChannel[];
  /** Hook-wide successful listing round; changes even when the channel array
   * is unchanged and never resets on socket reconnect. */
  listingRevision: number;
  /** Channels whose complete member pages and every member profile resolved. */
  completeMembershipChannelIds: readonly ChannelId[];
  /** The latest registration attempt on the current proxy failed. A retained
   * previous identity may still be present while roaming retries continue. */
  registrationFailed: boolean;
  /** The current proxy has completed its registration. Peer output and
   * membership reconciliation wait for this during a roaming reconnect. */
  registrationReady: boolean;
}

// NO_CHANNELS is the stable empty list SignallingState.channels falls
// back to, so consumers can depend on it without re-running effects on
// every render.
const NO_CHANNELS: ChatChannel[] = [];
const NO_COMPLETE_MEMBERSHIP_CHANNELS: ChannelId[] = [];

// PING_INTERVAL_MS is how often the SS is pinged while connected —
// deliberately well below the server's default subscriber aging (30s), so
// the pings double as registration keepalive.
const PING_INTERVAL_MS = 1000;

// PONG_TIMEOUT_MS bounds the wait for one ping's pong. A socket can look OPEN
// after a network switch while its old path is already dead, so timeout also
// recycles the proxy instead of leaving ICE signalling on that stale path.
const PONG_TIMEOUT_MS = 3000;

// REGISTER_REPLY_TIMEOUT_MS bounds the wait for the SS's registration
// reply.
const REGISTER_REPLY_TIMEOUT_MS = 5000;

// REGISTER_RETRY_INTERVAL_MS pauses between transient registration attempts on
// one open socket. Retrying here avoids sacrificing the retained incarnation
// merely because a dial succeeded before the network was ready to answer.
const REGISTER_RETRY_INTERVAL_MS = 2000;

// CHANNEL_LIST_TIMEOUT_MS bounds the wait for one listing round's page
// reads (channel list, member list) and each of its profile replies.
const CHANNEL_LIST_TIMEOUT_MS = 5000;

// CHANNEL_LIST_INTERVAL_MS is how often the channel list is renewed while
// connected.
const CHANNEL_LIST_INTERVAL_MS = 5000;

// CHANNEL_KEEPALIVE_INTERVAL_MS is how often the browser renews its
// channel membership once registered — deliberately well below the
// server's default subscriber aging (30s), like the pings.
const CHANNEL_KEEPALIVE_INTERVAL_MS = 5000;

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
  const timeout = new Promise<null>((resolve) => {
    timeoutId = window.setTimeout(() => resolve(null), timeoutMs);
  });
  try {
    for (;;) {
      const raced = await Promise.race([reader.read(), timeout]);
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

interface PagedReplies {
  events: SignallingEvent[];
  // The reply that ended paging, or null on timeout/EOF. Callers decide
  // whether that terminal reply proves successful completion.
  terminal: SignallingEvent | null;
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
): Promise<PagedReplies> {
  const reader = stream.getReader();
  const collected: SignallingEvent[] = [];
  let timeoutId: number | undefined;
  const timeout = new Promise<null>((resolve) => {
    timeoutId = window.setTimeout(() => resolve(null), timeoutMs);
  });
  try {
    for (;;) {
      const raced = await Promise.race([reader.read(), timeout]);
      if (raced === null || raced.done) {
        return { events: collected, terminal: null };
      }
      if (raced.value.inReplyTo !== msgId) {
        continue;
      }
      collected.push(raced.value);
      if (done(raced.value)) {
        return { events: collected, terminal: raced.value };
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
// channel, reading the SS's reply on its own short-lived stream. An initial
// page registration asks for a fresh auto-assigned subscriber id (the
// 1000-1999 range): a page load is a new incarnation, and a dead
// incarnation's subscriber id must NOT be reused. A replacement WebSocket in
// the same page instead passes its current id, which the SS accepts as a
// roaming refresh from the same authenticated session. Peer sessions are keyed
// by subscriber id and rebuilt only when the member listing observes a
// peer leaving and returning. Reusing an id after a page reload would be a
// same-tuple refresh, so peers would never see the leave, would keep their
// stale (dead) peer connection, and — the polite side having negotiated
// already — would never offer again. An impolite reincarnation would then
// wait forever (its fresh peer connection would sit at signalingState
// "stable" and ICE state "new", never connecting). A socket reconnect in the
// same page is different: both peer connections stay alive and keep their
// negotiators, so refreshing their existing id is exactly what they need.
// A fresh id makes every reload a
// brand-new member instead — the path that always worked; the SS evicts
// the tuple's dead registration the moment its successor registers, so
// peers tear their stale sessions down on their very next listing. The
// registration's display name is not the client's to choose: the server
// stamps the session's username onto the register event. Resolves to the
// registered subscriber id and the caller's user id — which the SS echoes
// in the reply's `to`, populated server-side from the session. The attempt
// separates a transient failure from an explicit id conflict, which is the only
// response that may make the caller abandon a retained roaming id.
interface SubscriberRegistration {
  subscriberId: SubscriberId;
  userId: UserId | null;
}

interface RegistrationAttempt {
  registration: SubscriberRegistration | null;
  idConflict: boolean;
}

async function registerSubscriber(
  read: () => ReadableStream<SignallingEvent>,
  send: (ev: SignallingEvent) => Promise<void>,
  reconnectSubscriberId: SubscriberId = "",
): Promise<RegistrationAttempt> {
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
        subscriberId: reconnectSubscriberId,
        channelId: WELLKNOWN_CH_ID_MAIN,
        username: "",
      },
    },
  });
  const reply = await replyPromise;
  if (!reply) {
    console.warn("signalling: registration timed out");
    return { registration: null, idConflict: false };
  }
  const result = reply.s2CEv?.registerResult;
  if (result) {
    console.info(`signalling: registered as subscriber ${result.subscriberId}`);
    return {
      registration: {
        subscriberId: result.subscriberId,
        userId: reply.to.userId ?? null,
      },
      idConflict: false,
    };
  }
  console.error("signalling: registration failed", reply.s2CEv?.err ?? reply);
  return {
    registration: null,
    idConflict:
      reply.s2CEv?.err?.errorCode === ErrorCode.SubscriberIdIsRegistered,
  };
}

// listChannels discovers the server's channels, resolving each into a
// ChatChannel: one listChannels request, reading pages until hasMore is
// false (an err page stops the read early), then listChannel per
// discovered id, all in parallel. A missing terminal page or any incomplete
// channel/member/profile resolution fails the whole round, so the caller can
// retain its last confirmed listing instead of publishing a partial outage.
interface ChannelListing {
  channels: ChatChannel[];
  completeMembershipChannelIds: ChannelId[];
}

interface ChannelResolution {
  channel: ChatChannel;
  membershipComplete: boolean;
}

async function listChannels(
  read: () => ReadableStream<SignallingEvent>,
  send: (ev: SignallingEvent) => Promise<void>,
  selfSubscriberId: SubscriberId | null,
): Promise<ChannelListing> {
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
  const pages = await pagesPromise;
  if (pages.terminal?.s2CEv?.channelListResult?.hasMore !== false) {
    throw new Error("signalling: channel list did not complete");
  }
  const ids = pages.events.flatMap(
    (p) => p.s2CEv?.channelListResult?.channels ?? [],
  );
  const resolved = await Promise.all(
    ids.map((channelId) =>
      listChannel(read, send, channelId, selfSubscriberId),
    ),
  );
  if (resolved.some((c) => c === null || !c.membershipComplete)) {
    throw new Error("signalling: channel membership did not fully resolve");
  }
  const channels = resolved.filter((c): c is ChannelResolution => c !== null);
  return {
    channels: channels.map((c) => c.channel),
    completeMembershipChannelIds: channels.map((c) => c.channel.id),
  };
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
): Promise<ChannelResolution | null> {
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
  const memberReplies = await mbsPromise;
  const memberIds = memberReplies.events
    .flatMap((p) => p.s2CEv?.channelMbsListResult?.members ?? [])
    .filter((id) => id !== selfSubscriberId);
  const pagesComplete =
    memberReplies.terminal?.s2CEv?.channelMbsListResult?.hasMore === false;

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
    channel: {
      id: profile.channelId,
      name: profile.channelName,
      members: members.filter((m): m is ChatUser => m !== null),
    },
    // A partial member page or a dropped profile is fine for best-effort UI,
    // but cannot prove that an unrendered member is absent.
    membershipComplete:
      pagesComplete && members.every((member) => member !== null),
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
  // The registration survives a socket reconnect: the replacement connection
  // refreshes the same in-page subscriber incarnation instead of rebuilding
  // its peer connections. A fresh page still starts with no retained id.
  const [registered, setRegistered] = useState<{
    proxy: SSProxy;
    me: ChatUser;
  } | null>(null);
  const registeredRef = useRef(registered);
  registeredRef.current = registered;
  const [registrationFailed, setRegistrationFailed] = useState<{
    proxy: SSProxy;
  } | null>(null);
  // Keep the last confirmed listing while a replacement socket registers and
  // lists again. Clearing it would make usePeerSessions mistake a transport
  // outage for every peer leaving. A changed/failed registration clears it.
  const [listed, setListed] = useState<{
    proxy: SSProxy;
    channels: ChatChannel[];
    completeMembershipChannelIds: ChannelId[];
    revision: number;
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
    const reconnectSubscriberId = registeredRef.current?.me.subscriberId ?? "";
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
      if (!live || proxy.destroyed || pending) {
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
      pending = { msgId, seq, t0: performance.now() };
      try {
        await writer.write(ping);
      } catch (err) {
        if (getPending()?.msgId === msgId) pending = null;
        if (live) console.error("signalling: ping write failed", err);
        return;
      }
      window.setTimeout(() => {
        if (live && getPending()?.msgId === msgId) {
          pending = null;
          setMeasured(null);
          console.warn("signalling: SS pong timed out; reconnecting");
          proxy.close();
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

    // Register the subscriber, and keep measuring latency. A retained
    // incarnation retries transient failures on this same open proxy; listing
    // and keepalive wait for success rather than replacing confirmed state with
    // an empty/unfiltered result. Initial-registration failure still resolves
    // null so the unauthenticated/empty initial UI can report that outcome.
    const registration: Promise<SubscriberRegistration | null> = (async () => {
      let requestedSubscriberId = reconnectSubscriberId;
      for (;;) {
        if (!live || proxy.destroyed) return null;
        // Registration needs an authenticated session. The profile's username
        // is only the local display name; the server stamps its own onto the
        // wire registration.
        let name: string | null = null;
        try {
          const profile = await fetchProfile(abort.signal);
          name = profile.username.trim() || profile.subject_id;
        } catch (err) {
          if (err instanceof DOMException && err.name === "AbortError")
            return null;
          if (err instanceof ProfileApiError && err.status === 401) {
            redirectToLogin();
            return null;
          }
          console.error(
            "signalling: cannot load /api/profile for registration",
            err,
          );
        }
        if (!live || proxy.destroyed) return null;
        if (name !== null) {
          try {
            const attempt = await registerSubscriber(
              () => proxy.getReadStream(),
              (ev) => writer.write(ev),
              requestedSubscriberId,
            );
            const reg = attempt.registration;
            if (
              live &&
              !proxy.destroyed &&
              reg !== null &&
              reg.userId !== null
            ) {
              const previous = registeredRef.current;
              if (
                previous !== null &&
                previous.me.subscriberId !== reg.subscriberId
              ) {
                // Only an explicit conflict fallback should reach this identity
                // change. The old listing is not valid for the new incarnation.
                setListed(null);
              }
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
              setRegistrationFailed(null);
              return reg;
            }
            if (attempt.idConflict && requestedSubscriberId !== "") {
              // The old id is definitively unavailable (for example, another
              // session claimed it while the server was down). Only now start
              // a fresh incarnation; timeouts and transient errors keep roaming.
              console.warn(
                `signalling: subscriber ${requestedSubscriberId} is registered elsewhere; requesting a fresh id`,
              );
              requestedSubscriberId = "";
            }
          } catch (err) {
            if (live) console.error("signalling: registration failed", err);
          }
        }
        if (!live || proxy.destroyed) return null;
        setRegistrationFailed({ proxy });
        if (registeredRef.current === null) return null;
        await new Promise<void>((resolve) => {
          window.setTimeout(resolve, REGISTER_RETRY_INTERVAL_MS);
        });
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
        const listing = await listChannels(
          () => proxy.getReadStream(),
          (ev) => writer.write(ev),
          reg?.subscriberId ?? null,
        );
        if (!live || proxy.destroyed) return;
        // Preserve an unchanged channel array for consumers that depend on
        // its identity, but advance one hook-wide revision on every successful
        // round. Reconnect renders are not new membership evidence.
        setListed((prev) => {
          if (!live || proxy.destroyed) return prev;
          const revision = (prev?.revision ?? 0) + 1;
          if (prev !== null && sameChannels(prev.channels, listing.channels)) {
            return {
              proxy,
              channels: prev.channels,
              completeMembershipChannelIds:
                listing.completeMembershipChannelIds,
              revision,
            };
          }
          return {
            proxy,
            channels: listing.channels,
            completeMembershipChannelIds: listing.completeMembershipChannelIds,
            revision,
          };
        });
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
    me: registered?.me ?? null,
    channels: listed?.channels ?? NO_CHANNELS,
    listingRevision: listed?.revision ?? 0,
    completeMembershipChannelIds:
      listed?.completeMembershipChannelIds ?? NO_COMPLETE_MEMBERSHIP_CHANNELS,
    registrationFailed:
      proxy !== null &&
      registered?.proxy !== proxy &&
      registrationFailed?.proxy === proxy,
    registrationReady: proxy !== null && registered?.proxy === proxy,
  };
}
