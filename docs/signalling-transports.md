# WebRTC signalling and transport map

This document distinguishes **session signalling** from application data and
media. The rule is deliberately not “everything JSON is signalling” and not
“everything on a DataChannel is signalling”: SDP, ICE and call control are
signalling; business payloads and their direct flow control are not.

## Transport matrix

| Exchange | Path | Current use |
| --- | --- | --- |
| Browser ↔ signalling server | WebSocket `/api/ss/ws`, JSON `SignallingEvent` | Registration, roaming, member/channel discovery, keepalive, SS ping/pong |
| Browser ↔ browser | The same SS WebSocket relay, `c2CEv` | SDP offer/answer, Trickle ICE, ICE-restart request, SIP-subset call dialog, call-status amendments |
| Browser ↔ echo/music/SIP bot | Exactly the same WS c2c relay | The bot is an ordinary authenticated `HeadlessRTCClient`, not an in-process signalling shortcut |
| Go UA / test UA ↔ bot or another Go UA | `WebSocketSignallingTransport` + the same SS c2c relay | Same wire types and negotiation rules as the browser |
| Chat and file metadata | `dcmsg` on the pair's existing RTCPeerConnection | Plain text, file-transfer status, ordinary text/file chat-control and their business echoes |
| File bytes and chunk ACKs | `dcbin` | `FILE` chunks and `FACK` cumulative acknowledgements; ACK is part of the file-transfer protocol, not session signalling |
| Audio/video | RTP over the existing RTCPeerConnection | Media tracks added/removed after call acceptance; SDP for those changes is still on WS |
| DTMF | RFC 4733 RTP `telephone-event` | Not WebSocket, not DataChannel |
| SIP bot ↔ external SIP UA/PBX | sipgo/diago SIP sockets + RTP | Real REGISTER, INVITE, ACK, CANCEL, BYE, MESSAGE and RTP; separate from the browser-facing JSON SIP subset |
| WHEP playback | HTTP POST/PATCH/DELETE | A separate HTTP signalling API, not part of the SS/DataChannel subsystem |

## WebSocket wire protocol

Every SS message is a JSON `SignallingEvent` with `from`, `to`, `msgId`,
optional `inReplyTo`, and one of `c2SEv`, `s2CEv`, `c2CEv`.

The WebRTC/call `c2CEv` addressing fields are `channelId`, `fromSubscriber`
and `toSubscriber`. Its payload alternatives include:

- `sessionDesc {type,sdp}` — SDP offer/answer and renegotiation;
- `rtcICECandidate` — Trickle ICE, including end-of-candidates;
- `iceRestartRequest` — asks the polite peer to own an ICE-restart offer;
- `call` — a call dialog or call-status amendment;
- `ping` / `pong` — the reserved c2c liveness shape (not used by the current
  browser or headless call implementation).

`call` retains the useful fields of the application's historical message
envelope: `mimeVersion`, `channelId`, `fromSubscriberId`, `toSubscriberId`,
`creationTimestamp`, `msgId`, optional `inReplyTo`, `mimeType`, `plaintext`,
and the body. Clients validate that its three addressing fields exactly match
the outer `c2CEv`; the outer WebSocket endpoint also stamps the authenticated
session address.

### Call dialog

`call.mimeType = application/x-sip` carries a JSON SIP subset with no SDP body:

- `INVITE` opens a voice/video dialog and rings the callee;
- `200 OK` accepts it; `603 Decline` rejects it;
- `CANCEL` aborts ringing; `BYE` ends an accepted call;
- `callId` identifies the dialog; `X-Media` is `voice` or `video`;
- `X-Call-Status` is the history entry's UI state.

There is no SIP ACK, CSeq or SDP body in this subset. WebRTC renegotiation is
performed independently by `PerfectNegotiator` using `sessionDesc` and ICE on
the same WS relay.

`call.mimeType = application/x-chat-control` with `chatControl.sip` is also
signalling: it amends the caller's INVITE history entry as the dialog becomes
accepted, rejected, cancelled or ended. It shares the dialog's ordered WS path,
so the amendment cannot overtake the INVITE on a different transport. Ordinary
text/file chat-control remains dcmsg business data.

Call messages are not bounced. Each sender folds its own copy after the WS
write succeeds; each receiver folds the relayed event. Browser peer sessions
route `call` before the SDP/ICE negotiator. Go `HeadlessRTCClient` likewise
uses a separate `CallSignallingHandler` instead of passing it to `Negotiator`.

Early call events are held by the browser until the matching
`(channelId, selfSubscriber, peerSubscriber)` session exists. Events addressed
to another subscriber incarnation are discarded; pending events expire after
the same two complete membership omissions used for early offers.

## DataChannel boundary

`dcmsg` and `dcbin` still exist, but they carry no SDP, ICE, SIP-subset dialog
or `chatControl.sip`. Receivers deliberately drop legacy DataChannel call
frames; there is no WS-preferred/DC-fallback dual stack.

The deployment consequence is intentional: old clients that only understand
SIP on `dcmsg` cannot complete calls with this version. Browser, Go UA and bots
must be deployed together. Chat/file compatibility is unaffected.

## Implementation references

- Wire types: `web/site/src/api/ss/types.ts`, `pkg/models/ss/ss.go`
- WS endpoint/relay: `pkg/api/websocket_ss/websocket_ss.go`, `pkg/models/ss/ss.go`
- Browser demux/output: `web/site/src/api/ss/peersessions.tsx`
- Browser call store/dialog: `web/site/src/api/ss/datachannel.tsx`,
  `web/site/src/components/chat/usePhoneCalls.ts`
- Go transport/client: `pkg/rtc/transport.go`, `pkg/rtc/client.go`
- Bot call adapter: `pkg/rtc/msg_handler/`
- Bot wiring: `cmd/server/main.go`
- Reconnect behavior: `docs/webrtc-reconnect.md`
