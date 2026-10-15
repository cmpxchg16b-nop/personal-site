# Group chat

_Status: living document, built in tiny steps. Landed: **step 1 — the
virtual subscriber in the data model**. Everything after it is design
direction, to be refined as the later steps land._

The WebRTC chat subsystem (see `signalling-server.md`) is
person-to-person: every channel subscriber is one user agent — a browser
client or a bot — and a conversation is a (channel, subscriber) pair
holding one peer connection, two data channels (`dcmsg`/`dcbin`), and an
echoed message history. Group chat extends it to conversations of N
participants.

## The idea: actual and virtual subscribers

Everything in the subsystem already keys on `(channelId, subscriberId)`
— the member listing, profile queries, the c2c relay's addressing, the
browser's peer sessions, DCMsg addressing.

Until group chat, every subscriber was an **actual subscriber**: one
backed by a real UA registration — a browser client or a bot holding a
live registration with the SS.

Group chat introduces the **virtual subscriber**: a subscriber that
exists in a channel _without any real UA registration behind it_. **A
group is the first instance of that concept.** As a subscriber, it
inherits everything keyed on `(channelId, subscriberId)`: it shows up in
`listChannelMembers`, its profile (the group's name) resolves through
`userProfileQuery`, and any member can address it.

The alternative — groups as a separate concept with their own membership
lists and addressing — would fork every one of those paths.

Two consequences of being virtual, not actual:

- **Nobody can register as a group.** The register event carries no type
  at all; a person cannot claim to be a group. A group exists purely as
  an entry in a **group repository** that the SS consults alongside its
  registration state.
- **A virtual subscriber has no address.** The c2c relay cannot route to
  it, so making a group reachable _is_ the fan-out work (see _The road
  ahead_).

## Step 1 — the virtual subscriber in the data model (landed)

Every subscriber now has a **`subscriberType`**: `person` for an actual
subscriber (a registered UA — a browser client or a bot), `group` for a
group. The patches, applied in both type systems (the TypeScript
prototype in `web/site/src/api/ss/types.ts` is the wire-format source of
truth; the Go model in `pkg/models/ss/ss.go` is field- and
JSON-compatible with it):

1. **A new `SubscriberType` type** with the two values — a
   `"person" | "group"` union in TypeScript, a string type with named
   constants in Go. It never appears in a registration: the register
   event is unchanged, and every registered subscriber is a person.
2. **The profile reply gains `subscriberType`** —
   `UserProfile.subscriberType` (always present, both languages). This
   is how a client learns that a channel member is a group.
3. **A `GroupRepository` interface** (`pkg/models/ss/groups.go`) stores
   the virtual subscribers and their memberships. Groups: `CreateGroup`,
   `DeleteGroup`, `GetGroupsByChannelId`, `GetGroupBySubscriberId`.
   Memberships — the relationship tuples `(member, role, group)`:
   `JoinGroup` records one, `LeaveGroup` removes one. Role numbers run
   **the lower the number, the higher the privilege** (owner 0, admin 1,
   member 2); a repetitive join with an identical
   tuple is a no-op, and a re-join with a different role updates the
   membership — a promotion when the new role is lower-numbered, a
   demotion when higher-numbered. Both relationship methods take an **operator** (a subscriber
   id in the group's channel): when the operator IS the member, the
   member did it themselves; otherwise the call records an action _on_
   the member — e.g. an admin inviting, promoting, or kicking them. The
   repository records, it does not authorize: who may add or remove whom
   is the caller's policy. All methods are context-aware.
   `OnMemoryGroupRepository` is the in-memory implementation. The
   provider depends on the **interface only** (dependency injection):
   the repository is a constructor parameter, `nil` selecting a fresh
   in-memory one; `cmd/server` wires the in-memory implementation
   explicitly.
4. **Member listings blend the virtual subscribers in**:
   `listChannelMembers` answers the channel's actual subscribers
   **plus** its groups' subscriber ids, sorted and paged together.
5. **Profile queries resolve the virtual subscribers**:
   `userProfileQuery` tries the group repository and the registered
   subscribers successively until a hit; a group resolves with
   `subscriberType: "group"` and the group's name as the username.
6. **A group's id is occupied**: explicitly registering a subscriber id
   a group holds is rejected with the new error code 6
   `SubscriberIdTakenByGroup`, and the automatic id assignment skips
   group-held ids.
7. **Relaying to a group is not implemented**: a virtual subscriber has
   no address, so a c2c event addressed to a group fails with
   `SubscriberNotFound` like any unroutable destination — and when it
   carries an SDP **offer** (the shape of a client opening a session
   with the group, e.g. the user clicking a group chat in the browser),
   the SS additionally logs `group chat unimplemented (todo)` to the
   console.

The e2e wire view (`e2e/websocket_ss_test.go`) gained the profile field,
so wire-format drift fails there.

## The road ahead (not implemented)

Sketches, in the order they will likely be attacked — each its own small
step, this document growing a section per step:

- **Group creation.** No wire event creates a group yet; the repository
  is the seam (today only embedders/tests seed it). Open: who creates a
  group and how, and the creation-time conflict policy against live
  actual-subscriber registrations (registration already refuses a
  group's id, but nothing yet stops a group taking a live person's id).
  The creator presumably becomes the group's first `owner`.
- **Membership.** The repository already records the relationship
  tuples (`JoinGroup`/`LeaveGroup`, roles, operators — patch 3). Open:
  what reaches the wire (join/leave events? membership queries?) and the
  UI, plus the authorization policy the repository deliberately leaves
  to its caller.
- **Message fan-out** — making a group reachable. The candidate shapes,
  from possible to impossible:
  - _SFU (selective forwarding)_: the group gains a real backing entity
    — the existing bot machinery (`HeadlessRTCClient`) can host it —
    every member holds one session with the group, and the group
    forwards each member's message to the other members as-is. O(n)
    sessions, no transformation, and history consistency can live at the
    group (the echo rule's group analogue). The leading candidate.
  - _MCU (mixing)_: the center transforms instead of merely forwarding —
    it merges the members' inputs into one canonical output each member
    consumes (for media: mixed audio/video; for messages: the group owns
    the canonical history and re-emits it). Possible, but heavier than
    chat messages need.
  - _P2P (full mesh)_: the group's members keep pair sessions with each
    other; the group subscriber is a rendezvous only. Today's machinery
    exactly — but O(n²) sessions, and history consistency scatters
    across pairs: the echo rule is a two-party rule. Doesn't scale; the
    most impossible.
- **Calls in groups.** Voice/video in a group conversation — out of
  scope for now.
- **The UI.** Groups in the sidebar (read `subscriberType` off the
  member profiles), one conversation per group.
