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
