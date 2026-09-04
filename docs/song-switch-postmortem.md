# Postmortem: the music bot's song switch went silent across codec families

_Date: 2026-09-04. Status: resolved and verified live in Chrome._

This note complements `docs/opus-streaming-postmortem.md`: that one was
about negotiation and timing bugs the opus rollout exposed; this one is
about a **browser-side** bug that hid behind perfectly clean
negotiation the whole time. It is for anyone touching the chat page's
audio graph or the music bot's switch logic.

## Background

A mid-call `/play` switches songs one of two ways (see
`docs/music-bot.md` §4/§6):

- **Within a codec family** (μ-law → μ-law, linear PCM → linear PCM):
  a stream swap inside the player — same track, same codec, same
  sender. No renegotiation, no SIP message; the next 20 ms tick reads
  the new stream. The browser notices nothing.
- **Across families** (μ-law ↔ linear PCM ↔ opus): the track's codec
  changes with the song, so the whole player is replaced — the new
  track is attached _before_ the old one is detached, so the music
  never leaves the wire, and the two operations coalesce into one
  renegotiation. The browser receives a **fresh track on a new m-line**
  while the old one is briefly still live.

## Symptoms, as reported

1. `/play some-music` → accept → the music plays.
2. `/play another-music` → the bot confirms "Now playing X", and…
   sometimes sound, sometimes silence. The pair never disconnected; no
   SDP error ever appeared in the console.
3. The failing combinations, once collected, were all cross-family:
   `chiptune` (μ-law) → any linear-PCM song; anything → `trancepulsefm`
   (opus). Same-family switches always worked.

## Debugging method

- Reproduced live (Go server on :8080, Next.js dev on :3000, visitor
  login, Chrome): the SDP logging that stayed in the tree after the
  opus postmortem showed clean switch offers — one coalesced offer per
  switch, `collision=false`, no glare, no rebuild. **Negotiation was
  exonerated in the first minute.**
- The audio graph's own logs then told the story: on every failing
  switch the old track logged `remote muted` and nothing else — the
  new track's `remote connected` line never appeared. The track
  arrived (`ontrack` fired); it was simply never connected.
- Two days' worth of "why intermittent" came from pion's transceiver
  recycling — see below.

## Root cause

`AudioGraph.addRemote` (`web/site/src/api/audio/audiograph.tsx`) keyed
each remote binding by the pair (`channelId:peerSubscriber`) and
**refused to rebind an occupied key**:

```ts
if (this.remotes.has(id)) return; // "An id already connected is left alone"
```

That invariant — one remote audio track per pair per page lifetime —
holds for browser↔browser calls, where a pair's tracks are born and
die with the call. The music bot's cross-family switch breaks it by
design: attach-before-detach hands the pair a second track while the
first is still bound.

The dropped track would have been recoverable if the old entry went
away — it only tears down on its track's `ended` event. But pion's
`RemoveTrack` _direction-flips_ the old m-line (sendrecv → recvonly or
inactive) instead of removing it, so on the browser side the old track
fires `mute` and **never `ended`**. The stale entry lingered for the
rest of the call, and every replacement track for that pair fell into
it: silence.

## Why "sometimes"

pion's `RemoveTrack` nulls the transceiver's sender
(`setSendingTrack(nil)`), so on a _later_ switch `AddTrack` can recycle
the freed transceiver (`isSendAllowed`: kind matches, no sender,
current direction not send\*). When that happens, the new music rides
an m-line the browser already has a receiver for — Chrome re-binds the
new RTP to the **already-bound receiver track** and never fires
`ontrack`. The stale graph entry then plays the new song by accident.

The first cross-family switch on a pair has no freed transceiver to
recycle (the old music m-line is still occupied at attach time), so it
always allocates a new m-line, always fires `ontrack`, and always hit
the drop. Whether a later switch recycles depends on the m-lines'
occupancy at that instant — switch order and where the mic landed —
hence "sometimes it works, sometimes it doesn't".

## Fix

`audiograph.tsx` only; the Go side needed nothing:

- `RemoteEntry` now records the track it is bound to.
- `addRemote`: the same track under an existing id is still a no-op; a
  **different** track under the same id tears the old entry down and
  binds the new stream — a replacement, not a collision.
- The `ended` self-teardown listener now removes the entry only if it
  still belongs to that track, so a replaced track's late `ended`
  cannot kill its successor (it shares the id).

One doc line in `docs/music-bot.md` §7 records the contract: a pair's
graph binding is replaceable by a fresh track, and a direction-flipped
m-line fires `mute`, not `ended`.

## Verification

- `tsc --noEmit` and `eslint` clean.
- Live in Chrome, end to end (the session the "sometimes" list came
  from): call → `chiptune` plays → `dubmood-fight-for-fun` (μ-law →
  linear PCM) → `trancepulsefm` (linear → opus, a live HTTP radio) →
  `chiptune` (opus → μ-law) → `red-winsnap-3-0-5-crk` (μ-law → linear)
  → `dubmood-fight-for-fun` (linear → linear, same family) → hang up
  (pair stays Connected) → a fresh call plays at once. The telltale
  triple — `remote muted` → `remote connected` → `remote unmuted` —
  appears on every cross-family switch; the same-family switch logs
  nothing at all, as designed. Zero console errors or warnings; the
  server log shows one clean offer/answer per switch, `collision=false`.

## Lessons

- **An idempotence guard is a silent trap the day the key starts
  carrying replacements.** `if (has(id)) return` was correct when a
  pair's remote track lived exactly as long as the binding; the music
  bot's attach-before-detach switch introduced replacement semantics,
  and the guard turned into a drop with no log line. If a keyed
  resource can be replaced, the replace path must be explicit — and it
  should log.
- **`mute` is not `ended`.** A direction flip is not a removal: the
  m-line lives, the track stays live, `ended` never fires. Don't use
  `ended` to observe a track's withdrawal while its m-line survives —
  and don't let teardown that waits on `ended` be the only way a
  resource is freed.
- **Presentation-layer bugs hide behind clean negotiation.** Every SDP
  offer/answer was green; the failure lived one map lookup away from
  the speaker. When "the call is up but there's no sound", follow the
  track all the way to the destination node before suspecting the
  wire.
- **The browser is the only honest integration test** — reprised from
  the opus postmortem. The Go wire suite has no audio graph, so this
  bug passed it trivially. The two console instruments that found it
  (`negotiate.ts`'s SDP summaries, the graph's connect/mute lines) are
  the same kind of always-on logging that made the opus debugging
  possible; keep them.

## Follow-ups / known limitations

- **m-line churn on cross-family switches** is bounded by pion's
  recycling but never shrinks: a pair that has seen N simultaneous
  audio m-lines keeps them in every later offer. Cosmetic; revisit only
  if a pathological sequence ever strains Chrome's m-line handling.
- The mid-call **glare fragility** documented in
  `docs/opus-streaming-postmortem.md` and `docs/signalling-server.md`
  is unchanged: a browser track change colliding with a bot-side one
  outside the sequenced paths still hits the polite peer's rebuild,
  which a browser cannot survive. The durable fix remains pion-side.
