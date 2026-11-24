# Vendored diago — local patches

This is `github.com/emiago/diago` **v0.32.2**, vendored verbatim (module
cache copy, MPL-2.0 — see LICENSE.txt) plus the local patches below,
selected by the `replace` directive in the repository's `go.mod`. Upstream
was checked through v0.40.0: the SDP generator still hardcodes the fmtp
lines there, so the patch is still required.

## Patch 1: per-codec fmtp lines in generated SDP

Upstream `generateSDPForAudio` hardcodes the fmtp line per well-known
payload type (`a=fmtp:96 useinbandfec=0` for opus, `a=fmtp:101 0-16` for
telephone-event, none otherwise) and `media.Codec` has no field to carry
one. The sip bot must announce stereo opus toward the SIP network
(`a=fmtp:96 useinbandfec=1;stereo=1`, RFC 7587), so:

- `media/codec.go`: `Codec` gains an `Fmtp string` field — the codec's
  format-specific parameters string, emitted as its `a=fmtp` line.
- `media/media_session.go` `generateSDPForAudio`: a non-empty `Fmtp`
  replaces the built-in default for the well-known payload types, and
  adds an `a=fmtp` line for any other payload type that sets one
  (`codecFmtpLine` helper).
- `media/media_session.go` `updateRemoteCodecs`: codec matching compares
  with `codecsMatch`, which excludes `Fmtp` and compares encoding names
  case-insensitively (for example, `OPUS` matches `opus`). Payload type,
  sample rate, sample duration, and channel count still use exact equality.
  `Fmtp` annotates the local offer/answer and is never parsed from remote
  SDP, so with the upstream struct equality (`c == rc`) a local codec
  carrying `Fmtp` would never match its remote counterpart and nothing
  would negotiate. The `SDPCodecPreferLocalOrder` branch additionally
  re-attaches the local codec's `Fmtp` to the kept (remote) codec.

No other behavior changes. When `Fmtp` is empty everywhere (the upstream
constants), the generated SDP is byte-identical to upstream.

## Patch 2: G.722 joins the codec vocabulary

Upstream's codec list stops at PCMU/PCMA/opus/telephone-event; the sip
bot's transcoding-free relay passes G.722 end to end, so diago must
speak it:

- `media/codec.go`: `CodecAudioG722` — PT 9, and SampleRate **8000**:
  RFC 3551 clocks G.722's RTP timestamps at 8000 Hz although the audio
  is 16 kHz. `CodecAudioFromPayloadType` and `mapSupportedCodec` map the
  static PT to it, and `CodecsFromSDPRead` parses a bare `9` format
  without an rtpmap line (the same shorthand PCMU/PCMA already had).
- `media/sdp/formats.go`: the `FORMAT_TYPE_G722` constant.
- `media/media_session.go` `generateSDPForAudio`: a G.722 case emitting
  the canonical `a=rtpmap:9 G722/8000` — the generic branch would append
  a spurious `/1` channel count.

No other behavior changes; SDP without PT 9 is byte-identical to
upstream.

## Patch 3: SIP MESSAGE support (RFC 3428 pager mode)

Upstream diago never registers sipgo's `OnMessage` handler (an inbound
MESSAGE falls to sipgo's no-route default) and keeps its per-transport
`*sipgo.Client` private, so a MESSAGE can neither arrive nor leave. The
sip bot bridges pager-mode messages (docs/sip-bot.md §7.3), so:

- `diago.go`: `Diago` gains a `messageHandler` field and the
  `WithMessageHandler(func(req *sip.Request, tx sip.ServerTransaction)
error)` option; `NewDiago` registers `server.OnMessage` through the
  same errHandler wrapper as its siblings. The zero value answers
  405 Method Not Allowed — an option-less diago behaves exactly as
  upstream (sipgo's no-route answer is also a 405).
- `diago.go`: `Diago.Client(transport string) (*sipgo.Client, error)`
  exposes the per-transport client (the existing getTransport/getClient
  pair) so out-of-dialog requests the library has no helper for can be
  sent on the account's own socket — the client's Via pinning and NAT
  behavior are the transport's own.

No other behavior changes; diago's dialog machinery is untouched.

## Patch 4: multi-media SDP parsing (the m-line count guard is per type)

Upstream `SessionDescription.MediaDescription` refuses any SDP carrying
more than ONE m-line total (`if len(values) > 1`), so every audio+video
SDP fails to parse — and with it `MediaSession.InitWithSDP` and
`MediaSession.RemoteSDP`, the two seams a video call's negotiation runs
through (both directions: the offer/answer the bot parses itself, and
the callee's answer diago's own client answer path parses). The
type-filtering loop below the guard shows the intent was per-type
counting; as written it is dead code for multi-media SDPs. The sip
bot's /video-call and inbound video calls bridge exactly those, so the
guard now counts only m-lines OF THE REQUESTED TYPE:

- `media/sdp/sdp.go` `MediaDescription`: a second m-line of the same
  media type still errors; m-lines of other types are skipped. A
  single-m-line SDP (everything upstream ever parses) behaves
  byte-identically.

No other behavior changes; SDP generation is untouched.

## Patch 5: serial REGISTER lease and transaction lifecycle

The user-level registration service drives one transaction serially and needs
more lifecycle information and cleanup than upstream v0.32.2 provides:

- `RegisterTransaction.Expiry` exposes the latest granted lease. Successful
  responses prefer this UA's `Contact;expires=` over top-level `Expires`, parse
  comma-separated and repeated Contact headers, compare URI transport when both
  sides carry it, and retain a valid fallback when a value is malformed.
- Initial REGISTER attempts always build a fresh Via. The hand-written UDP
  client transaction is terminated on success, error, and context cancellation
  so its retransmission timers cannot outlive the attempt.
- Unregister expires only the transaction's own Contact with `expires=0`; it
  does not send `Contact: *` and remove other devices' bindings.

The service intentionally serializes calls on each transaction; this patch does
not make a single `RegisterTransaction` safe for concurrent use.

## Patch 6: cancellation-safe cleanup after a final INVITE answer

A final 2xx establishes the dialog even when the caller cancels its INVITE
context while the answer is being processed. If applying the answer's SDP then
fails, upstream's cleanup reused that canceled context and accidentally called
`Ack` a second time on ACK failure instead of sending BYE.

- `dialog_client_session.go` `waitAnswer`: after `applyRemoteSDP` fails, diago
  now attempts ACK and then BYE with separate five-second contexts derived from
  `context.Background`, so cancellation cannot suppress either mandatory
  request. The original negotiation error is preserved and joined with any ACK
  and BYE errors. Successful negotiation and pre-answer cancellation behavior
  are unchanged. Dialog/media closing remains the caller's responsibility, as
  before.

## Addendum: CANCEL transaction handling (review issue 7)

The `diago.go` `OnCancel` comment now records the transaction-layer contract
explicitly. This is a comment-only vendored change; there is no response-path
behavior change and no dialog-cache-based second response:

- In sipgo v1.4.3 (the nested diago module) and v1.6.0 (the root module), a
  CANCEL matching a pending INVITE is intercepted before diago's `OnCancel`
  handler. Sipgo sends 200 to CANCEL, terminates the INVITE with 487, and the
  dialog callbacks end a pre-established dialog's context.
- Only a CANCEL without a matching INVITE transaction reaches diago's handler,
  so its 481 response is intentional and remains unchanged. Responding 200 in
  this handler would both violate RFC 3261 §9.2 for unknown CANCELs and risk
  duplicate responses if transaction routing changes.
- `pkg/sipregistration/inbound_wire_test.go` in the root module pins the full
  real-UDP runtime behavior: INVITE/180, matched CANCEL/200, INVITE/487, dialog
  cancellation, and unknown CANCEL/481.

## Patch 7: one codec per SDP payload format

Upstream v0.32.2's `CodecsFromSDPRead` appends a codec for every matching
`rtpmap` attribute even though `MediaSession` allocates the output with one
slot per `m=` format. Audio and video media sections may legally reuse a
dynamic payload type for different codecs; scanning all attributes then writes
past the audio slice and panics. A FreeSWITCH offer with audio/video collisions
on PT 96/97/98/99 reproduced `index out of range [10] with length 10`.

- `media/codec.go`: stop scanning after the first valid `rtpmap` for each
  format, matching current upstream's one-codec-per-format control flow. This
  preserves the audio mapping when its media section precedes video and keeps
  the existing caller/output-size contract unchanged.
- `media/media_session_test.go`: the real 10-format/40-attribute offer now
  negotiates audio codecs without a panic.
- `pkg/sipregistration/media_test.go`: the complete inbound selection, remote
  offer, and pinned-answer path covers Opus/96 audio plus VP8/99 video.

## Patch 8: explicit primary-audio clock for relay sender reports

`RTPSession.WriteRTP` learns its sender-report clock from the first packet on a
new SSRC. The user SIP gateway intentionally shares one audio writer/SSRC for
primary audio and RFC 4733 telephone-event. If DTMF precedes Opus, upstream
would retain 8 kHz and extrapolate later Opus sender-report timestamps at the
wrong rate.

- `media/rtp_session.go`: `PrimeWriteStats(codec, ssrc)` lets a relay initialize
  the SSRC and primary codec clock before its first packet. It is a no-op once
  packet statistics exist, so ordinary diago writers and live streams keep their
  original behavior. A private primed state suppresses reports only until the
  first relay packet, so priming alone never emits a zero-timestamp report while
  existing manually seeded statistics retain their previous RTCP behavior.
- `pkg/sipregistration/media.go`: the audio relay writer primes this clock from
  the selected primary SIP audio codec when writing through an `RTPSession`.
- `media/rtp_session_test.go`: a DTMF-first/Opus-second sequence verifies that
  the sender report advances at 48 kHz and retains the shared writer SSRC.
