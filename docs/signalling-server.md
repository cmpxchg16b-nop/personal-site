# Signalling server

The signalling sub-system lets the site's visitors discover each other and
broker WebRTC session establishment (session descriptions and trickle ICE
candidates) between them. It is a prototype: client identity comes from
experimental HTTP headers, only the main channel exists, and all state lives
in process memory.

## Components

| Layer          | Location                       | Role                                                                                                                                      |
| -------------- | ------------------------------ | ----------------------------------------------------------------------------------------------------------------------------------------- |
| Wire prototype | `web/site/src/api/ss/types.ts` | The browser-side TypeScript prototype; the source of truth for the wire format.                                                           |
| Model          | `pkg/models/ss`                | Go wire types (field- and JSON-compatible with the prototype), the `SignallingServiceProvider` interface, and `SimpleOnMemorySSProvider`. |
| Transport      | `pkg/api/websocket_ss`         | `WebSocketSSHandler`, an `http.Handler` bridging WebSocket connections to a provider.                                                     |
| Wiring         | `cmd/server/main.go`           | Mounts the handler at `/api/ss/ws`, backed by the in-memory provider.                                                                     |
| E2E tests      | `e2e/websocket_ss_test.go`     | Drives the real server binary over WebSocket with deliberately independent wire views.                                                    |

```mermaid
graph TD
    A[browser client A] <-->|WebSocket| H["WebSocketSSHandler: hub goroutine + CAM table"]
    B[browser client B] <-->|WebSocket| H
    H -->|inMsg service channel| P["SimpleOnMemorySSProvider: single Run goroutine owns all state"]
    P -->|outMsg| H
```

## Wire protocol

Every message is a single envelope, `SignallingEvent`:

- **`from` / `to` (`EPAddr`)** — packed addresses holding `userId`,
  `userSessionId`, or `serviceId`. A browser client may leave `from` empty;
  the server handler populates it from the request's identity headers.
- **`msgId`** — generated uniquely, statelessly, independently per message.
- **`inReplyTo`** — correlates a reply with the message it answers.
- **One payload slot**: `c2SEv` (client → SS), `s2CEv` (SS → client), or
  `c2CEv` (client ↔ client, relayed).

All identifier types (`UserId`, `UserSessionId`, `ServiceId`,
`SubscriberId`, `ChannelId`, `MsgId`) are opaque strings: implementations
must not guess or assume their formation. Two well-known ids exist: the SS
service itself (`253dc952-a55b-4d01-a885-c5e240e95fdb`) and the main
channel (`f887f5b0-7b78-4ceb-a051-f42879f9d98e`), the only channel an
implementation must provide.

### Client → SS (`c2SEv`)

- **`register {subscriberId, channelId, username}`** — registers the client
  as a subscriber of a channel. Answered by a
  `registerResult {channelId, subscriberId}` on success or an `err`
  otherwise. An **empty `subscriberId`** asks the SS to assign one,
  sequentially, from the automatic assignment range **1000-1999** (the
  assigned id comes back in `registerResult`; empty always mints a fresh
  subscriber, even from an already-registered tuple). Clients picking
  their own subscriber ids are recommended to **preserve the 1000-1999
  range** for automatic registration. A subscriber id is bound to zero or
  one (user id, user session id, channel id) tuple: re-registration of a
  live id by the same tuple is a refresh (renewing `lastActive`,
  optionally updating the username), while another tuple is rejected
  with `SubscriberIdIsRegistered`.
- **`channelKeepAlive {channelId, subscriberId}`** — renews the caller's
  membership of a channel: the SS refreshes the subscriber's
  `lastActive`. Sent periodically once registered. **Nothing is answered
  on success**; an `err` is answered otherwise — `ChannelNotFound`, or
  `SubscriberNotFound` when the registration has expired or is bound to
  another (user id, user session id) tuple — meaning the membership is
  gone and the client should re-register.
- **`userProfileQuery {subscriberId, channelId}`** — answered with a
  `profile {subscriberId, channelId, username}` reply.
- **`listChannelMembers {channelId}`** — answered with **one or more**
  `channelMbsListResult {channelId, members, hasMore}` messages, all
  `inReplyTo` the request. `members` is a page of subscriber ids (resolve
  them via `userProfileQuery`). The client may time out on its own, or
  keep reading until `hasMore` is `false`; even an empty channel yields
  one final empty page.
- **`listChannels {}`** — dynamic channel discovery; carries no fields.
  Answered with **one or more** `channelListResult {channels, hasMore}`
  messages, all `inReplyTo` the request, with the same paging semantics
  as `listChannelMembers`. Resolve a channel's profile via
  `channelProfileQuery`.
- **`channelProfileQuery {channelId}`** — answered with a
  `channelProfile {channelId, channelName}` reply; an unknown channel
  yields `ChannelNotFound`.
- **`ping {pingId, sequenceNumber}`** — out-of-band liveness ping to the
  SS itself (browsers cannot send protocol-level WebSocket pings). **No
  registration is needed first.** Answered with a `pong` that keeps the
  ping id and carries `ackSequenceNumber = sequenceNumber + 1`; the pong's
  own sequence number is 0 (the SS keeps no per-session sequence space).

### SS → client (`s2CEv`)

Carries `err`, `registerResult`, `profile`, `channelProfile`, `pong`,
`channelMbsListResult`, or `channelListResult`. Every reply
comes `from` the well-known SS service id, is addressed `to` the origin's
`from`, and is correlated via `inReplyTo`. Errors pair a well-defined
`errorCode` with a descriptive `errorMsg`:

| Code | Name                       | Meaning                                                  |
| ---- | -------------------------- | -------------------------------------------------------- |
| 1    | `SubscriberIdIsRegistered` | subscriber id already registered                         |
| 2    | `SubscriberNotFound`       | subscriber not found in the given channel                |
| 3    | `ChannelNotFound`          | channel not found (only the main channel is implemented) |
| 4    | `UsernameTaken`            | username already taken in the channel                    |
| 5    | `NoSubscriberIdAvailable`  | no subscriber id free in the 1000-1999 auto-assign range |

### Client ↔ client relay (`c2CEv`)

Addressed by `{channelId, fromSubscriber, toSubscriber}` and carries
`sessionDesc` and `rtcICECandidate` (modelled with
`github.com/pion/webrtc/v4` types — `webrtc.SessionDescription` and
`webrtc.ICECandidateInit` — which marshal to the exact browser JSON
shapes), plus out-of-band `ping`/`pong`.

Think of a subscriber id as an IP address and the channel as the VLAN it
lives in: a subscriber id alone does not determine an endpoint, only
`(channelId, subscriberId)` does. There is deliberately a single channel
id — no from/to pair — because caller and callee are expected to be
registered in the same channel.

The sender addresses the event **to the SS**; the SS translates
`(channel, subscriber)` addressing into an `EPAddr` — learned from each
subscriber's `from` at registration — and **rewrites the envelope's `to`**
to the resolved destination. `from` and the payload pass through
unchanged. An unknown channel yields a `ChannelNotFound` error; an
unknown subscriber in it yields `SubscriberNotFound`.

Ping sessions between clients follow the sequence rules: a `pong` keeps
the `pingId` and answers `ackSequenceNumber = sequenceNumber + 1`; the
next `ping`'s `sequenceNumber` is the ack of the last reply.

## In-band peer messaging (data channels)

Discovery and session establishment go through the SS, but chat traffic
itself never does: once the c2c relay has brokered the WebRTC handshake,
the two browsers talk **directly and in-band** over a point-to-point peer
connection. The browser-side codecs and session management live in
`web/site/src/api/ss/datachannel.tsx` (the source of truth for the
messaging frame format); `useDataChannel` owns the sessions and the
message store.

- **One peer connection, two data channels per pair.** The polite peer
  (the smaller subscriber id) creates both — which also starts perfect
  negotiation over the c2c relay — the impolite peer receives them via
  `ondatachannel`, dispatched by label: `dcmsg` carries the JSON DCMsgs
  below, `dcbin` carries compact binary frames (see _Binary file
  transfer_). Sessions track the channel membership: they appear as the
  listing discovers members and are torn down when a member drops out.
- **ICE servers** come from `GET /api/iceServers` (the `<iceServer/>`
  entries of `serverConfig.xml`), so a deployment can steer peers at its
  own STUN/TURN instance.
- **Echo.** A data channel does not echo; the recipient bounces every
  message back with `echo: true` so the sender sees its own message.
  Echoes are never echoed again. Both sides therefore build the same
  history from the same frames.

Every frame is one JSON `DCMsg`: `mimeVersion` (`1.0`), `channelId`,
`fromSubscriberId`, `toSubscriberId`, `creationTimestamp` (Unix seconds),
`msgId`, optional `inReplyTo`, optional `echo`, a `mimeType` body-kind
tag, and the body. Malformed frames are dropped silently, mirroring the
SS's rule for malformed events.

| `mimeType`                           | Body                                                                                                         | Meaning                                                                                                                                                                                                   |
| ------------------------------------ | ------------------------------------------------------------------------------------------------------------ | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `text/plain`                         | `plaintext`                                                                                                  | A plain-text chat line.                                                                                                                                                                                   |
| `application/x-file-transfer-status` | `fileTransfer {fileId, filename, fileMIMEType, fileSizeTotalBytes, fileSizeTransferred, fileTransferStatus}` | The UI state of a file transfer (`pending` → `running` → `done`). The file's bytes never travel in the message; the opaque, globally unique `fileId` is the handle a recipient passes back to fetch them. |
| `application/x-chat-control`         | `chatControl {subtype, targetMessageId, text?, fileTransfer?}`                                               | Mutates one of the sender's earlier messages instead of adding a line.                                                                                                                                    |

Chat-control semantics: `delete` drops the target message; `amend`
rewrites the target's body — `text` for a text message, `fileTransfer`
for a file-transfer status — while keeping its `msgId` and
`creationTimestamp`, so an amendment never moves or reattributes a
message. Only the sender's own messages can be targeted; unknown targets
and body-kind mismatches are no-ops. The receiver applies a control
message on arrival, the sender when its echo comes back, so both
histories stay identical; control messages themselves are never stored.

### Binary file transfer (`dcbin`)

The second data channel of every pair carries compact binary frames; the
source of truth for the format is `web/site/src/api/ss/binaryframes.ts`
(the codec), and the transfer engine is `useBinaryDataChannel` in
`binarydatachannel.tsx`, built on the `BinaryTransport` `useDataChannel`
exposes for the same sessions. All multi-byte integers are big-endian.
Two frame kinds exist, selected by the 4-byte ASCII `frame_type`:

`FILE` — one (possibly fragmented) block of a file, sender → receiver; a
48-byte header followed by the payload:

| Offset | Size | Field                                                                                          |
| ------ | ---- | ---------------------------------------------------------------------------------------------- |
| 0      | 4    | `frame_type` — ASCII `FILE`                                                                    |
| 4      | 16   | `file_id` — the file's UUID, packed into 16 bytes                                              |
| 20     | 4    | `seq` — uint32, per-`file_id` frame sequence from 0 (a `file_id` is a stream; files multiplex) |
| 24     | 8    | `offset` — uint64 byte offset of the payload in the original file (the first frame's is 0)     |
| 32     | 8    | `total` — uint64 size of the whole file in bytes                                               |
| 40     | 8    | `payload_size` — uint64 size of the payload; must equal the remaining frame length             |
| 48     | var  | `payload` — the (possibly fragmented) file content                                             |

`FACK` — receiver → sender acknowledgement of the contiguous prefix of a
file's stream received so far; 32 bytes, no payload:

| Offset | Size | Field                                                                                   |
| ------ | ---- | --------------------------------------------------------------------------------------- |
| 0      | 4    | `frame_type` — ASCII `FACK`                                                             |
| 4      | 16   | `file_id` — the file's UUID, packed into 16 bytes                                       |
| 20     | 4    | `ack_seq` — uint32, cumulative: the next expected seq (one past the highest contiguous) |
| 24     | 8    | `acked_bytes` — uint64, cumulative: the contiguously received byte count                |

The sender streams a file in 16 KiB chunks with at most 64 frames
(1 MiB) unacknowledged — a sliding window over the per-file frame
sequence, which is also the send-side flow control — and the receiver
acknowledges every accepted frame. SCTP delivery is ordered and
reliable, so reassembly is strict concatenation: a gap, an overlap or a
mismatched `total` marks the stream corrupt and the transfer is dropped.
An empty file is one `FILE` frame with an empty payload.

The transfer's status rides `dcmsg`, never `dcbin`: `sendFile` returns a
reader yielding the transfer's status (`pending` → `running` → `done`)
as the receiver's acknowledgements advance — throttled to a few updates
per second — and the caller forwards every status as a chat-control
amend of the original file-transfer-status message, whose echo updates
the sender's own history; both UIs therefore render what the receiver
actually holds, in real time. Completed files stay in memory on both
ends: the receiver hands its reassembled Blob out by
`getFileByFileId(fileId)`, the sender registers the original File under
the same id, so a completed card downloads from whichever side clicks it.

### Magic commands

The composer intercepts three debugging commands on send — a message
matching one is never sent as text but turned into the corresponding
chat-control message (`web/site/src/components/chat/magic.ts`):

- `/magic a90926e3-c768-45b7-ab93-4709c5f4aa91 <target_msg_id>` — delete
  the target message.
- `/magic 1f734b69-9c46-4629-9e73-0aed96166f7c <target_msg_id> <content>` —
  amend a text chat message (`content` may contain spaces).
- `/magic 7f8d9d4e-f41e-4e18-958e-ebc990690666 <target_msg_id> <status> <transferred> <total>` —
  amend a file-transfer status message (`status` is `pending`, `running`
  or `done`; the sizes are byte counts).

Amendments copy the target's immutable fields (a file transfer's
`fileId`, name and MIME type) from the local history, so an unknown
target id or a kind mismatch makes the command a no-op.

## Addressing and routing

- The provider is the **only** component that understands subscriber
  addressing; it resolves `(channel, subscriberId)` → `EPAddr`.
- The WebSocket handler **never learns subscriber ids**. Like a learning
  switch, it builds the association between a connection's remote
  `ip:port` and the `(userId, userSessionId)` pair purely by sniffing the
  `from` field of inbound messages (after header population).
- Outbound events are routed by their `to` EPAddr to **every
  connection the address was learned on** — one address can map to
  several connections (two tabs sharing a session, or a reconnect whose
  old connection has not been noticed closed yet); there is no flooding
  beyond that. Events whose destination was never learned are dropped;
  a disconnect purges the closed connection from all of its learned
  addresses (link-down aging), and a reconnect re-learns onto the new
  connection.

**Roaming.** Registrations outlive connections (the prototype has no
unregister message), and ping-session state (ping id, seq/ack chain) is
purely client-side — so a client that quickly disconnects and reconnects
with the same identity keeps its registration, its profile stays
queryable (even while it is offline), and in-flight ping sessions
continue transparently on the new connection — as long as it reconnects
within the aging interval (below). The only requirement is that the
reconnected client speaks first — any message (e.g. a liveness `ping`)
re-learns its address onto the new connection, switch-style.

## Subscriber aging

Every subscriber carries a `lastActive` timestamp, refreshed by any
activity from its learned address — including c2s liveness pings — and
explicitly by `channelKeepAlive`, so a client that pings periodically
keeps its registration alive. A
registration whose `lastActive` is longer ago than the aging interval
(the `--ss-aging` server flag, default `10s`) is considered invalid:
profile queries answer `SubscriberNotFound`, relays fail, and the
subscriber id and username are free again. Expiration is **lazy** —
there is no timer: the list-members operation does an all-at-once
mark-and-sweep of a channel's expired entries, while every other
operation just tests the validity of the specific subscriber before
using it (evicting that single entry if it has expired).

## Concurrency model

Both stateful components follow the CSP pattern — no mutexes on any data
path:

- **Provider** (`SimpleOnMemorySSProvider`): the single `Run` goroutine
  owns all state (channels → subscribers) on its own stack and is fed
  exclusively by `inMsg`, the service channel; replies and relays go to
  `outMsg`, which `Run` closes on return. `Shutdown` is idempotent
  (`sync.Once`) and takes priority over buffered input. Buffered channels
  provide natural backpressure.
- **Handler** (`WebSocketSSHandler`): one hub goroutine owns the
  connection set and the CAM table; each connection has a read pump
  (parse JSON, populate `from`, forward to the hub's ingress channel) and
  a write pump (drain a per-connection queue to the socket, write
  deadline configurable via `WriteTimeout`, default 10s). A full queue makes a slow consumer get dropped rather than
  stall the hub. The hub exits on context cancellation or provider
  shutdown, closing every queue, which cascades to connection closure.

One integration detail: `pkg/log`'s logging `responseWriter` implements
`Hijack` explicitly, because `gorilla/websocket`'s upgrader type-asserts
`http.Hijacker` directly instead of going through
`http.ResponseController` — without the passthrough, every upgrade behind
the logging middleware would fail with 500.

## Identity (experimental)

For now, client identity comes from two request headers:
`X-Exp-UserId` and `X-Exp-UserSessionId`. A handshake missing either is
rejected with `400` before the upgrade. This stands in for the future
cookie/JWT-based population described by the prototype.

## Current limitations

- **In-memory only.** Registrations are lost on restart.
- **No unregister/leave message** in the prototype — registration state
  lives until it ages out (see _Subscriber aging_) or the provider's
  `Run` returns.
- **Malformed events are dropped silently** — the prototype defines no
  error code for them.
- **Username format** (lowercase, no-space, valid DNS label) is declared
  by the prototype but not validated server-side.
- Channel member pages are fixed at 64 members.

## Testing

- `pkg/models/ss` and `pkg/api/websocket_ss` carry `-race`-clean unit
  suites: registration and its error codes, profile query, c2s ping/pong
  session rules, channel-keepalive renewal and its error codes, relay
  pass-through with `to` rewriting, member-list paging, shutdown/close
  semantics, header rejection, unicast routing, and address re-learning
  across reconnects.
- `e2e/websocket_ss_test.go` exercises the real built server binary:
  register + profile, a 3-round c2c ping/pong session (seq/ack chaining),
  RTC payload pass-through, the members list, c2s ping before
  registration, and handshake rejection. Its wire views are deliberately
  independent copies, so accidental wire-format drift fails there.
