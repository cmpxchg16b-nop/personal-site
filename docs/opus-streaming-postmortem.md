# Postmortem: the WebRTC music bot's Opus streaming roll-out

_Date: 2026-09-04. Status: all issues resolved; follow-ups listed at the end._

This note is for anyone touching the music bot, the RTC client, or the
negotiation layer: it explains what broke when Ogg Opus streaming
(`trancepulsefm`) shipped, why the symptoms pointed at the wrong layers
for days, and what the actual root causes were. Four distinct bugs —
three in negotiation/timing plumbing, one in the new opus pump — were
hiding behind each other. Each symptom disappeared only after the one
above it was fixed.

## Background

The music bot (`pkg/rtc/musicbot`, design doc `docs/music-bot.md`) plays
songs into WebRTC voice calls over the pair's long-lived peer
connection. This session added a third source family: **Ogg-framed Opus
at 48 kHz** — typically a remote radio stream over HTTP — whose packets
pass through to the wire untouched (no decode/re-encode). The first
real-world source was TrancePulse FM (`https://stream.trance.ie/opus`).

## Symptoms, as reported

1. **Every `/play`** produced a browser console error:
   `InvalidAccessError: Failed to set remote answer sdp: ... Failed to
   set SSL role for the transport.` (In the Next.js dev overlay this is a
   modal dialog, so it reads as "calls are broken".)
2. **After `/play trancepulsefm`, no other song would play** — the bot
   went silent for that peer until the server restarted. Other songs
   worked if the radio was never touched.
3. **`/play trancepulsefm` connected but produced no sound.**
4. **Hanging up killed the peer connection**: the chat showed
   "Disconnected", the data channels died
   (`datachannel: no open data channel`), and the bot became unreachable
   — again until restart.

## Debugging method

- Added SDP logging on both ends that stayed in the tree: the browser's
  `negotiate.ts` and Go's `pkg/rtc/negotiate.go` now log every
  offer/answer's per-section `mid` and `a=setup` role.
- Reproduced live: Go server on `:8080`, Next.js dev server on `:3000`,
  visitor login, real calls from Chrome, watched
  `chrome://webrtc-internals` (the event log's full SDPs and the
  RTP/jitter-buffer stats — `concealedSamples` told silence apart from
  flow).
- Probed the real radio stream with `curl` and a throwaway parser over
  pion's Ogg reader to see its actual page structure (pages of 50
  uniform 20 ms packets, granule = a huge live-edge counter).

## Root causes and fixes

### 1. The answer's DTLS role flipped (the "SSL role" error)

The bot is the polite peer, so it makes the initial offer and the
browser answers — making the **bot the DTLS server** from day one. When
the browser later re-offers (attaching its mic on call accept), pion's
`CreateAnswer` never consults the established transport: an `actpass`
offer is answered with `defaultDtlsRoleAnswer` = client, i.e.
`a=setup:active`. That is a role flip on an established transport, which
Chrome refuses — and then Chrome's state machine wedges in
`have-local-offer`, so **every later renegotiation fails too** —
including a song switch's track replacement, which is how this read as
"no sound" for the opus switch while the chiptune kept playing (its
music offer had already completed before the fatal answer).

**Fix** (`pkg/rtc/client.go`, `cmd/server/main.go`): the
peer-connection factories now take the session's politeness
(`NewPeerConnection func(polite bool)`) and pin
`SettingEngine.SetAnsweringDTLSRole` — `Server` for the polite side
(the initial offerer), `Client` for the impolite one. Pinned by
`TestDefaultPeerConnectionFactoryAnsweringRole`.

### 2. Packet durations came from granule positions (the radio's silence)

The first opus implementation derived each packet's duration from Ogg
page granule-position deltas, baselining at the ID header's pre-skip.
That is correct for a file starting at zero — and meaningless for a
**live stream**, whose granule is the encoder's cumulative counter since
the station started (measured: `771303869`). The first page's "delta"
came out as ~321 seconds of media **per packet**: the pump wrote one
packet, then paced itself hours into the future. The browser's stats
showed exactly one 233-byte packet and ever-growing
`silentConcealedSamples`.

**Fix** (`pkg/models/audiosource/ogg.go`): durations now come from each
packet's own **Opus table of contents** (RFC 6716 §3.1: the
configuration's frame size times the frame count — the same math pion's
decoder uses). Granule positions are no longer read; the count check
tolerates the encoder's pre-skip and tail padding. This also removed
the one-page settling latency of the original design.

### 3. The opus pump never listened outside its pacing wait (bot dead after the radio)

The packet pump serviced stop/switch/session-end only inside
`if wait > 0 { select … }`. A live stream's reads block on the network
and arrive at real time, so the wait never opens: `stop()` was ignored
(leaking the HTTP connection), and the next `/play`'s `setSource`
blocked forever on the switch channel, deadlocking the peer's whole
data-channel handler. Local/inline sources never showed it (instant
reads ⇒ the wait always opens).

**Fix** (`pkg/rtc/musicbot/player.go`): control messages are serviced
between every two packets; the timed select remains the pacing path.
Pinned by `TestPlayerPacketPumpLiveStream` (a self-paced fake source).

### 4. Hangup glare rebuilt the connection into an identity the browser can't accept (the "Disconnected")

At hangup both sides renegotiate at the same instant — the browser
removes its mic track, the bot removes the music track. Two offers in
flight is a glare, and the polite Go peer's yield is a **peer-connection
rebuild** (pion cannot roll a pending local offer back — verified in
v4.2.20: the state machine has no `have-local-offer →
setLocal(rollback)` transition). The rebuilt PC answers with a **fresh
ICE/DTLS identity** against an offer that was not an ICE restart. A
browser cannot fold that into the running transport: it goes
`connecting → disconnected → failed` — and once, under load, a
15,915-cycle offer/answer storm. pion↔pion tolerates the swap, which is
why the Go wire tests never caught it.

**Fix** (`pkg/rtc/musicbot/handler.go`): the flows are sequenced to
avoid colliding at all — the hangup's track withdrawal is deferred by
`hangupDetachDelay` (500 ms). The music is already off; the m-line's
removal goes out after the peer's teardown offer, alone. No glare, no
rebuild, transport stays connected.

## Verification

- All Go suites pass with CGO on and off; the audio packages are
  `-race`-clean. New pins: the DTLS-role factory test, the live-stream
  pump test, a live-edge-granule case in the Ogg stream tests, and the
  wire tests asserting RTP payloads equal the source packets byte for
  byte.
- Live in Chrome, end to end: call → radio plays (packets flowing,
  `silentConcealedSamples: 0`) → switch to chiptune → switch back →
  hang up → still "Connected" → `/play` reaches the bot at once. No
  console errors anywhere.

## Lessons

- **The browser is the only honest integration test for negotiation.**
  Every one of these bugs passed the pion↔pion wire suite — pion is far
  more tolerant than Chrome (setup-role flips, credential changes in
  answers, malformed glare recovery). The suite stays valuable for
  logic; negotiation compatibility needs a browser, and the SDP logging
  added here is what made it diagnosable.
- **Container counters are not clocks.** A granule position is "samples
  since an unknown epoch" for a live stream; only deltas on a stream you
  started are meaningful. Self-describing units (the Opus TOC) beat
  container metadata when both exist.
- **Symptoms stacked in reverse order of their causes**: the "opus
  streaming" feature was innocent; the failures lived in negotiation
  code written months earlier, exposed only because the new family
  changed *when* and *how often* renegotiations happened (a switch
  offer, a mic attach, a teardown).

## Follow-ups / known limitations

- **A mid-call glare against a browser is still fatal to the pair** (any
  collision outside the sequenced hangup path hits the rebuild). Tracked
  in `docs/signalling-server.md` _Current limitations_; the durable fix
  is pion-side (a true rollback, or a rebuild that performs a proper ICE
  restart).
- The `handleRebuildNote` machinery remains for Go↔Go peers; its
  browser-facing behavior is now documented as fragile rather than
  relied upon.
