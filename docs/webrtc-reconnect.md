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

A replacement socket goes through these steps:

1. **Reconnect with the same subscriber id.** `useSignalling` keeps the current
   page's registration in memory and sends that id in the replacement socket's
   `register` request. For the same authenticated `(userId, userSessionId,
channelId)` tuple, the SS accepts this as a roaming refresh. A new page load
   still sends an empty `subscriberId`, deliberately creating a fresh
   incarnation rather than resurrecting a dead one.
2. **Keep UI and WebRTC state.** The last confirmed identity and channel listing
   stay available while the socket is down and roaming registration is pending.
   Existing `RTCPeerConnection`s, their tracks and data channels, and the phone
   call model are not reset merely because the proxy changed.
3. **Queue signalling, but do not send too early.** Each peer has an ordered
   in-memory output queue. SDP, candidates, and restart requests produced while
   disconnected wait there. Output remains detached until registration on the
   current replacement socket succeeds, because an open WebSocket alone does not
   prove that the subscriber registration is still valid.
4. **Resume and reconcile.** Once registration succeeds, queued events flush in
   order and normal negotiation continues. Fresh membership listings then
   reconcile actual arrivals and departures. Async registration/listing results
   from a socket that has already died are discarded instead of overwriting the
   retained state.

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

Every `HeadlessRTCClient.Run` tears down all of its peer sessions before it
returns. Data-channel and track handler registrations survive for the next run,
but peer connections do not. Consequently, a bot's WebSocket reconnect is
currently a **rebuild**, not the browser's media-preserving roaming path.

A configured bot `subscriberId` is registered again on the next run. If the
configuration left it empty, each run asks for a fresh automatically assigned
id. A configured-id conflict is retried once with an empty id.

Implementation: `cmd/server/main.go` (`startBotClient`) and `pkg/rtc/client.go`.

### What the server preserves

A subscriber registration is not owned by the WebSocket. A same-tuple
new-incarnation registration with a different id evicts it immediately;
otherwise it remains in the in-memory SS provider until it ages out or the
provider stops. The default aging interval is **10 seconds**, configurable with
`--ss-aging`.

While that registration remains valid:

- its profile can still be queried;
- a same-tuple, same-id registration refreshes it;
- the first message on a replacement connection re-learns the authenticated
  address onto that connection, allowing replies and relays to reach it;
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
- **The peer really leaves or reloads.** This is not a roaming blip. Membership
  reconciliation removes the old session; a page reload's fresh subscriber id
  creates a new incarnation and negotiates a new peer connection.
- **A Go bot's WebSocket drops.** Its current run and peer sessions end, then a
  later run registers and discovers peers again. The browser-side preservation
  guarantee does not apply to the bot's own reconnect.

## Timing reference

| Knob                                              | Default                    |
| ------------------------------------------------- | -------------------------- |
| Browser retry after an established socket drops   | Immediate                  |
| Browser retry after a failed connection attempt   | 2s fixed                   |
| Go bot `reconnectInterval`                        | 5s, configurable per bot   |
| Browser registration reply timeout                | 5s                         |
| Browser ping / channel keepalive / member listing | 1s / 5s / 5s               |
| SS subscriber aging                               | 10s (`--ss-aging`)         |
| ICE disconnected grace                            | 3s                         |
| ICE retry base / nominal cap                      | 2s / 30s, with ±20% jitter |

## Guarantees and limits

- Browser signalling queues are **ordered, best-effort, and in memory**. They
  are not durable storage or an exactly-once replay protocol. A page reload
  loses them, and events already handed to a failed network stack cannot be
  recalled or guaranteed delivered.
- Roaming does not hide a real identity change, registration failure, peer
  departure, or server-side loss of state.
- ICE recovery cannot manufacture a viable network path. STUN/TURN, firewall,
  device, and remote-availability failures still require their own resolution.
- The browser's preservation behavior and the Go bot's rebuild behavior are
  intentionally documented separately; do not infer one from the other.

Focused coverage lives in `web/site/src/api/ss/peersessions.test.tsx` and
`ice_restart.test.ts`, and in `pkg/rtc/ice_restart_test.go`. Server roaming and
address re-learning are covered in `pkg/models/ss` and `pkg/api/websocket_ss`.
