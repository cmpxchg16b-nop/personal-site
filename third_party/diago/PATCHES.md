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
  with `codecsMatch`, which excludes `Fmtp` — the field annotates the
  local offer/answer and is never parsed from remote SDP, so with the
  upstream struct equality (`c == rc`) a local codec carrying `Fmtp`
  would never match its remote counterpart and nothing would negotiate.
  The `SDPCodecPreferLocalOrder` branch additionally re-attaches the
  local codec's `Fmtp` to the kept (remote) codec.

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
