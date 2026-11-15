# WebSocket and ICE auto-reconnect

The chat/WebRTC stack has two independent recovery loops. They repair different
layers and should not be confused:

| Layer                | What it carries                                                         | What reconnects                                                         | Effect on an established call                                             |
| -------------------- | ----------------------------------------------------------------------- | ----------------------------------------------------------------------- | ------------------------------------------------------------------------- |
| Signalling WebSocket | Registration, membership, SDP, ICE candidates, and ICE-restart requests | A new WebSocket and, where applicable, the same subscriber registration | A browser socket blip alone does not close peer connections or stop media |
| ICE transport        | The actual WebRTC data channels and audio/video packets                 | ICE restart negotiation on the existing peer connection                 | Media can pause until a usable ICE path is restored                       |

The WebSocket is the control plane, not the media path. Once ICE is connected,
chat data and phone/music traffic flow directly between the peers. Conversely,
an ICE restart usually needs the signalling relay for its offer/answer and new
candidates, so recovering both layers together requires coordination.

The wire fields and registration model are documented in
[Signalling server](signalling-server.md). This document describes the recovery
behavior implemented around them.

## Signalling WebSocket auto-reconnect

### Browser: preserve the in-page incarnation

`SSProxy` wraps one open WebSocket. When it closes, `SSProxyProvider` starts a
replacement immediately. If a connection attempt itself fails, the provider
retries after a fixed **2 seconds**; this loop does not use ICE's exponential
backoff.

A network switch can instead leave a stale socket that still reports `OPEN`.
The browser therefore pings the SS every **500 milliseconds** and recycles the
proxy when a pong does not arrive within **3 seconds** of that ping. Ping bookkeeping records
the pending request before the asynchronous write so a fast pong cannot be lost.
This gives ICE recovery a working signalling path even when the browser never
receives a socket close event.

A replacement socket goes through these steps:

1. **Validate the connection endpoint before registration.** The server's first
   frame is an endpoint hello carrying the exact connection's
   `(userId,userSessionId)`. The proxy uses it to reject any flooded c2c/s2c
   addressed to another endpoint, and atomically replaces the browser's
   localStorage hint. The same pair preserves the roaming behavior below; a
   changed pair instead clears retained registration, listing, peer sessions,
   and pre-registration offers/calls before starting a fresh subscriber
   incarnation. Logout/401 clears the cached pair and shuts down without an
   automatic reconnect.
2. **Reconnect with the same subscriber id.** `useSignalling` keeps the current
   page's registration in memory and sends that id in the replacement socket's
   `register` request. For the same authenticated `(userId, userSessionId,
channelId)` tuple, the SS accepts this as a roaming refresh. A new page load
   still sends an empty `subscriberId`, deliberately creating a fresh
   incarnation rather than resurrecting a dead one. If the profile/registration
   attempt times out or fails on an open socket, the retained incarnation is not
   cleared: the browser retries every **2 seconds**. Only an explicit
   `SubscriberIdIsRegistered` conflict abandons that id for a fresh incarnation.
3. **Keep UI and WebRTC state.** The last confirmed identity and channel listing
   stay available while the socket is down and roaming registration is pending.
   Existing `RTCPeerConnection`s, their tracks and data channels, and the phone
   call model are not reset merely because the proxy changed.
4. **Queue signalling, but do not send too early.** Each peer has an ordered
   in-memory output queue. SDP, candidates, and restart requests produced while
   disconnected wait there. Output remains detached until registration on the
   current replacement socket succeeds, because an open WebSocket alone does not
   prove that the subscriber registration is still valid. A proxy swap also
   cancels an unsettled write on the old transport, so repeated network changes
   cannot leave later ICE messages blocked behind it.
5. **Resume and reconcile.** Self-addressed c2c received after hello but before
   the peer demux has subscribed is retained by `SSProxy` and replayed in
   arrival order; ping and registration readers cannot consume this backlog.
   The demux acknowledges events as its reads yield them, so a StrictMode
   reader cancellation/re-setup replays only the still-unacknowledged suffix.
   Once registration succeeds, queued events flush in
   order and normal negotiation continues. A peer still in `have-local-offer`
   also retransmits that offer, because a stale socket may have consumed the
   original write without delivering it. Identical unresolved offers are
   coalesced, while the retransmitted `localDescription` includes candidates
   gathered since the original send. Fresh membership listings then
   reconcile actual arrivals and departures. Async registration/listing results
   from a socket that has already died are discarded instead of overwriting the
   retained state. Incomplete listing pages or unresolved channel/member
   profiles fail the whole listing round for the same reason: a partial outage
   must not masquerade as an empty roster.

This also covers repeated disconnects, including a second socket failure while
its registration or membership listing is still pending. The browser does not
force an ICE restart just because the WebSocket reconnected: if the direct ICE
path stayed healthy, audio, music, and data channels can continue without that
extra renegotiation.

Implementation: `web/site/src/api/ss/proxy.ts`, `react.tsx`, and
`peersessions.tsx` under `web/site/src/api/ss/`.

### Go bots: reconnect and rebuild the run

The built-in echo/music bots use a different lifecycle today.
`startBotClient` runs one `WebSocketSignallingTransport` plus one
`HeadlessRTCClient.Run` per connection attempt. When either side ends, it waits
for the configured `reconnectInterval` and starts another attempt. The default
is **5 seconds**.

The Go transport sends WebSocket protocol pings every **500 milliseconds** and
uses pongs to extend a **3-second** read deadline. A missing pong therefore ends a
half-open connection even when no close frame arrives. Any application-message
or ping write failure also closes the socket immediately, waking the read loop;
otherwise the write pump could exit while `Run` stayed blocked forever and the
outer reconnect loop never ran.

Each Go transport also waits for the endpoint hello before starting its writer
or forwarding business events, then applies the same destination-pair filter as
the browser. Eager CAM learning means a still-live old registration can produce
c2c traffic before the new run's register reply: `HeadlessRTCClient` preserves
those events in arrival order and replays them after the final subscriber id is
known, rather than dropping an early offer, candidate, or INVITE.

Every `HeadlessRTCClient.Run` tears down all of its peer sessions before it
returns. Data-channel and track handler registrations survive for the next run,
but peer connections do not. Consequently, a bot's WebSocket reconnect is
currently a **rebuild**, not the browser's media-preserving roaming path.

A correlated keepalive `SubscriberNotFound` or `ChannelNotFound` reply also ends
the current run. Keepalive message ids carry a unique per-run prefix, so even an
arbitrarily delayed reply is recognized without a finite correlation-history
window. This is necessary when a TCP connection resumes after its SS registration
has already aged out: continuing to write on that socket cannot relay anything.
The outer loop reconnects and registers again. An unrelated c2c error, such as a
destination subscriber leaving, does not tear down the sender's own run.

A configured bot `subscriberId` is registered again on the next run. If the
configuration left it empty, each run asks for a fresh automatically assigned
id. A configured-id conflict is retried once with an empty id.

Implementation: `cmd/server/main.go` (`startBotClient`), `pkg/rtc/client.go`,
and `pkg/rtc/transport.go`.

### What the server preserves

A subscriber registration is not owned by the WebSocket. A same-tuple
new-incarnation registration with a different id evicts it immediately;
otherwise it remains in the in-memory SS provider until it ages out or the
provider stops. The default aging interval is **30 seconds**, configurable with
`--ss-aging`.

While that registration remains valid:

- its profile can still be queried;
- a same-tuple, same-id registration refreshes it;
- a replacement connection's authenticated address is learned at upgrade,
  before its endpoint hello, allowing replies and relays to reach it even before
  the client sends its first message; source learning from inbound messages is
  retained as well;
- peers do not see a membership leave/rejoin merely because the socket changed.

Registration state is not durable. A server restart loses it, and a sufficiently
long outage can age it out. The browser then has to register again and reconcile
from fresh listings; recovery must not pretend that an actually departed peer
is still present.

## ICE auto-reconnect

Each perfect negotiator owns an ICE recovery watchdog. Browser and Go use the
same policy and defaults:

| Behavior                | Policy                                                                         |
| ----------------------- | ------------------------------------------------------------------------------ |
| When recovery arms      | Only after ICE has reached `connected` or `completed` at least once            |
| `disconnected`          | Wait a **3-second** grace period before restarting                             |
| Eligible `failed` state | Start recovery immediately; do not accelerate an already active retry sequence |
| Retry interval          | Exponential backoff from **2 seconds**, nominally capped at **30 seconds**     |
| Jitter                  | Multiply each retry delay by a random factor from **0.8 to 1.2**               |
| `checking` / `new`      | Not recovery outcomes; an active watchdog keeps running                        |
| Healthy again           | `connected` or `completed` cancels recovery and resets backoff                 |
| Closed/torn down        | Stop recovery                                                                  |

There is no fixed maximum attempt count. A transient state must not cancel the
watchdog, and a timer firing at the same time as a healthy state notification
must not restart an already recovered transport.

### One side owns restart offers

The lexicographically smaller subscriber id is the **polite** peer. To prevent
both sides from creating colliding restart offers:

- the polite peer originates the ICE-restart offer;
- the impolite peer sends `c2CEv.iceRestartRequest: true` and keeps its own retry
  watchdog;
- only the polite peer acts on that request, using its normal offer/answer and
  trickle-candidate path.

The browser uses `RTCPeerConnection.restartIce()`, which retains the request
across in-flight negotiation. The Go negotiator keeps an explicit pending latch
and emits the restart offer once signalling can proceed.

A retry must also retry the **signalling transaction**, not just the local API
call. `restartIce()` need not emit another `negotiationneeded` while an offer is
unanswered, and Pion cannot create a new offer in `have-local-offer`. On every
watchdog retry, both implementations therefore retransmit the current pending
local offer. The polite peer does the same when it receives an ICE-restart
request. This covers a lost original offer and a lost answer: applying the
duplicate offer makes the remote send another answer. An impolite peer also
retransmits its own pending ordinary offer. After an impolite peer ignores a
colliding offer, either implementation starts a spaced **2-second** retry only
if that glare remains unresolved; normal glare converges without an immediate
duplicate that could cause repeated rollback/rebuild. The delayed retry is still
required when the impolite peer's ICE is healthy: the polite peer cannot roll
back or rebuild and answer until it receives that offer, after which the active
watchdog can finish ICE recovery. Retransmission reads the current
`LocalDescription`, so it may include
newly gathered candidates while retaining the same ICE credentials.

Go additionally tracks whether that pending local offer already requested fresh
ICE credentials. Retransmitting an ordinary offer leaves the restart latched, so
its answer is followed by the required restart offer. Retransmitting an actual
restart offer satisfies the latch and does not trigger a redundant second
restart after its answer.

An ICE restart renegotiates the transport on the existing peer connection; it
is not a new phone session and does not intentionally drop tracks or data
channels. It cannot succeed without usable local/remote candidates, and its SDP
and candidates need a working signalling path. In the browser, restart-related
messages produced during a socket outage therefore join the same ordered queue
described above.

Implementation: `web/site/src/api/ss/ice_restart.ts` and `negotiate.ts`, plus
`pkg/rtc/negotiate.go` and `ice_restart_test.go`.

## Combined failure scenarios

- **WebSocket drops, ICE stays connected.** Browser media and data channels keep
  flowing. The socket roams back to the same subscriber id; no peer-session or
  call reset occurs.
- **ICE drops, WebSocket stays connected.** After the grace/failure policy, one
  side requests or originates an ICE restart. Offer/answer and candidates travel
  over c2c until ICE becomes healthy or the session closes.
- **Both drop.** The ICE watchdog may fire while signalling is unavailable.
  Browser c2c output waits for socket reconnection and registration readiness,
  then resumes. Media can remain interrupted until ICE itself recovers, but the
  WebSocket outage does not add a separate UI/call teardown.
- **The socket looks healthy, but one SDP delivery is lost.** Later ICE watchdog
  attempts retransmit the pending offer even if neither side installs a new
  proxy. A single lost offer or answer therefore no longer leaves the polite
  peer permanently stuck in `have-local-offer` with an ineffective restart timer.
- **The peer really leaves or reloads.** This is not a roaming blip. Membership
  reconciliation removes the old session; a page reload's fresh subscriber id
  creates a new incarnation and negotiates a new peer connection.
- **A Go bot's WebSocket drops.** Its current run and peer sessions end, then a
  later run registers and discovers peers again. The browser-side preservation
  guarantee does not apply to the bot's own reconnect.

## Timing reference

| Knob                                               | Default                    |
| -------------------------------------------------- | -------------------------- |
| Browser retry after an established socket drops    | Immediate                  |
| Browser retry after a failed connection attempt    | 2s fixed                   |
| Go bot `reconnectInterval`                         | 5s, configurable per bot   |
| Browser SS ping / pong liveness timeout            | 500ms / 3s                 |
| Go WebSocket ping / pong read deadline             | 500ms / 3s                 |
| Browser registration reply timeout                 | 5s                         |
| Browser retry after transient registration failure | 2s fixed                   |
| Browser channel keepalive / member listing         | 5s / 5s                    |
| SS subscriber aging                                | 30s (`--ss-aging`)         |
| ICE disconnected grace                             | 3s                         |
| ICE retry base / nominal cap                       | 2s / 30s, with ±20% jitter |
| Unresolved-glare offer retry                       | 2s, until stable or closed |

## Guarantees and limits

- Browser signalling queues are **ordered, best-effort, and in memory**. They
  are not durable storage or an exactly-once replay protocol. A page reload
  loses them, and events already handed to a failed network stack cannot be
  recall or guaranteed delivered. While ICE recovery remains active, pending
  local offers are nevertheless retried with the watchdog rather than depending
  on one delivery or on a WebSocket-proxy change.
- Roaming retries transient registration failures without clearing confirmed
  UI state, but it does not hide an explicit id conflict, a real identity
  change, peer departure, or unrecoverable server-side loss of state.
- ICE recovery cannot manufacture a viable network path. STUN/TURN, firewall,
  device, and remote-availability failures still require their own resolution.
- The browser's preservation behavior and the Go bot's rebuild behavior are
  intentionally documented separately; do not infer one from the other.

Focused coverage lives in `web/site/src/api/ss/peersessions.test.tsx` and
`ice_restart.test.ts`, and in `pkg/rtc/ice_restart_test.go`. Server roaming and
address re-learning are covered in `pkg/models/ss` and `pkg/api/websocket_ss`.
