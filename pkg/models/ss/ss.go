// Package ss models the signalling server (SS) that lets the site's
// visitors discover each other and broker WebRTC session establishment.
// The wire types mirror the message prototype defined for the browser
// client in web/site/src/api/ss/types.ts and stay field-compatible with
// it (JSON keys included); RTC payloads (session descriptions and ICE
// candidates) reuse the types of github.com/pion/webrtc/v4.
//
// SignallingServiceProvider is the contract between the transport layer
// (HTTP/WebSocket handlers, which parse and serialize the wire format)
// and a signalling server implementation: a provider consumes parsed
// SignallingEvents from inMsg and emits replies and relayed events on
// outMsg. It handles messages, and messages only — a SignallingEvent IS
// a message, already parsed in memory.
//
// SimpleOnMemorySSProvider is an in-memory implementation that follows
// the CSP pattern: the single Run goroutine owns all state, so the data
// path needs no mutexes at all.
package ss

import (
	"context"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/pion/webrtc/v4"
)

// Opaque identifier types. Implementations must treat values of these
// types opaquely and must not guess or assume their formation.
type (
	// UserId globally and uniquely identifies a user.
	UserId string

	// UserSessionId globally and uniquely identifies a user session; a
	// user can have multiple sessions to the signalling server.
	UserSessionId string

	// ServiceId identifies an entity that is neither a user nor has a
	// "session" to the signalling server.
	ServiceId string

	// SubscriberId is local to a channel of a signalling server and
	// uniquely identifies an entity in that channel.
	SubscriberId string

	// ChannelId globally and uniquely identifies a channel.
	ChannelId string

	// MsgId uniquely identifies a message globally and correlates a
	// request with its reply.
	MsgId string
)

const (
	// WellKnownSvcIdSS is the well-known service id of the signalling
	// server.
	WellKnownSvcIdSS ServiceId = "253dc952-a55b-4d01-a885-c5e240e95fdb"

	// WellKnownChIdMain is the well-known channel id of the main channel;
	// a SS implementation should at least implement the main channel.
	WellKnownChIdMain ChannelId = "f887f5b0-7b78-4ceb-a051-f42879f9d98e"
)

// EPAddr is a packed address struct. When the message is constructed in
// the browser, the client can leave UserId/UserSessionId empty and the
// HTTP/WS server handler populates them from cookie, header, or JWT.
type EPAddr struct {
	UserId        UserId        `json:"userId,omitempty"`
	UserSessionId UserSessionId `json:"userSessionId,omitempty"`

	// ServiceId is set when the client is talking to a service.
	ServiceId ServiceId `json:"serviceId,omitempty"`
}

// ClientToSSRegEv registers a client with the server.
//
// A subscriber id is bound to zero or one (user id, user session id,
// channel id) tuple: re-registration of a live id by the same tuple is
// allowed and acts as a refresh — it renews lastActive and may update
// the username; registration of an id bound to another tuple is
// rejected with ErrorCodeSubscriberIdIsRegistered.
type ClientToSSRegEv struct {
	// SubscriberId may be empty: the SS then assigns one sequentially
	// from the automatic assignment range 1000-1999 and echoes it in the
	// registerResult reply. An empty id always creates a fresh subscriber,
	// even from an already-registered tuple. Clients picking their own
	// subscriber ids are recommended to preserve that range for
	// automatic registration.
	SubscriberId SubscriberId `json:"subscriberId"`
	ChannelId    ChannelId    `json:"channelId"`

	// Username is descriptive: lowercase, no-space, valid DNS label, for
	// displaying the subscriber in the UI. The WebSocket endpoint
	// overrides it with the caller's session username (the JWT's username
	// claim) before the event reaches the provider, so clients send it
	// empty; the value only matters to SS providers reached through other
	// transports.
	Username string `json:"username"`
}

// ClientToSSChannelKeepAlive renews the subscriber's membership of a
// channel — its lastActive — and is sent periodically once registered.
// The SS answers nothing on success; an err reply
// (ErrorCodeChannelNotFound, or ErrorCodeSubscriberNotFound when the
// registration has expired or is bound to another (user id, user
// session id) tuple) means the membership is gone and the client should
// re-register.
type ClientToSSChannelKeepAlive struct {
	ChannelId    ChannelId    `json:"channelId"`
	SubscriberId SubscriberId `json:"subscriberId"`
}

// ClientToSSUserProfileQuery queries the user profile of another
// subscriber.
type ClientToSSUserProfileQuery struct {
	SubscriberId SubscriberId `json:"subscriberId"`
	ChannelId    ChannelId    `json:"channelId"`
}

// ClientToSSChannelProfileQuery queries the profile of a channel. The SS
// answers with a s2c channelProfile.
type ClientToSSChannelProfileQuery struct {
	ChannelId ChannelId `json:"channelId"`
}

// ClientToSSListChannelMembers lists the members of a channel. The SS
// answers with one or more s2c channelMbsListResult messages, all
// inReplyTo the request.
type ClientToSSListChannelMembers struct {
	ChannelId ChannelId `json:"channelId"`
}

// ClientToSSListChannels lists the channels of the server. It carries no
// fields. The SS answers with one or more s2c channelListResult
// messages, all inReplyTo the request.
type ClientToSSListChannels struct{}

// ClientToSSEv is a client-to-signalling-server event.
type ClientToSSEv struct {
	Register         *ClientToSSRegEv            `json:"register,omitempty"`
	UserProfileQuery *ClientToSSUserProfileQuery `json:"userProfileQuery,omitempty"`

	// ChannelKeepAlive renews the caller's membership of a channel; the
	// SS answers nothing on success, an err otherwise.
	ChannelKeepAlive *ClientToSSChannelKeepAlive `json:"channelKeepAlive,omitempty"`

	// ChannelProfileQuery asks for the profile of a channel; the SS
	// answers with a s2c channelProfile.
	ChannelProfileQuery *ClientToSSChannelProfileQuery `json:"channelProfileQuery,omitempty"`

	// Ping pings the signalling server itself (out of band
	// liveness/keepalive, e.g. from browsers, which cannot send
	// protocol-level WebSocket pings); the SS answers with a s2c pong.
	// No registration is needed first.
	Ping *PingPongMsg `json:"ping,omitempty"`

	// ListChannelMembers asks for the members of a channel.
	ListChannelMembers *ClientToSSListChannelMembers `json:"listChannelMembers,omitempty"`

	// ListChannels asks for the channels of the server.
	ListChannels *ClientToSSListChannels `json:"listChannels,omitempty"`
}

// ErrorCode is the well-defined code of an SSToClientErrEv.
type ErrorCode int

const (
	// ErrorCodeSubscriberIdIsRegistered: the subscriber id has already
	// been registered.
	ErrorCodeSubscriberIdIsRegistered ErrorCode = iota + 1

	// ErrorCodeSubscriberNotFound: the subscriber was not found in the
	// given channel.
	ErrorCodeSubscriberNotFound

	// ErrorCodeChannelNotFound: the channel was not found (or only the
	// main channel is implemented and a different one was specified).
	ErrorCodeChannelNotFound

	// ErrorCodeUsernameTaken: the requested username has already been
	// taken by someone else.
	ErrorCodeUsernameTaken

	// ErrorCodeNoSubscriberIdAvailable: no subscriber id is free in the
	// automatic assignment range 1000-1999 (all are registered).
	ErrorCodeNoSubscriberIdAvailable
)

// SSToClientErrEv carries a well-defined error code plus a descriptive,
// human-readable message.
type SSToClientErrEv struct {
	ErrorCode ErrorCode `json:"errorCode"`
	ErrorMsg  string    `json:"errorMsg"`
}

// SSToClientChannelMbsListResult is one page of a channel members list
// result. Multiple result messages can be sent per one
// listChannelMembers request, all correlated via InReplyTo; the client
// might decide a timeout on its own, or wait until HasMore is false.
type SSToClientChannelMbsListResult struct {
	ChannelId ChannelId `json:"channelId"`

	// Members holds the subscriber ids of (a page of) the members of the
	// channel; use ClientToSSUserProfileQuery to resolve a member's
	// profile.
	Members []SubscriberId `json:"members"`

	// HasMore is true when more result messages follow for the same
	// request.
	HasMore bool `json:"hasMore"`
}

// SSToClientChannelListResult is one page of a channel list result.
// Multiple result messages can be sent per one listChannels request, all
// correlated via InReplyTo; the client might decide a timeout on its
// own, or wait until HasMore is false.
type SSToClientChannelListResult struct {
	// Channels holds (a page of) the channel ids of the server; use
	// ClientToSSChannelProfileQuery to resolve a channel's profile.
	Channels []ChannelId `json:"channels"`

	// HasMore is true when more result messages follow for the same
	// request.
	HasMore bool `json:"hasMore"`
}

// SSToClientEv is a signalling-server-to-client event.
type SSToClientEv struct {
	// Err is present only when an error occurred.
	Err *SSToClientErrEv `json:"err,omitempty"`

	// RegisterResult carries the reply payload of a successful
	// registration.
	RegisterResult *RegisterResult `json:"registerResult,omitempty"`

	// Profile carries the reply payload of a user profile query.
	Profile *UserProfile `json:"profile,omitempty"`

	// ChannelProfile carries the reply payload of a channel profile
	// query.
	ChannelProfile *ChannelProfile `json:"channelProfile,omitempty"`

	// Pong answers a c2s ping: it keeps the ping id and answers
	// ack = ping's seq + 1.
	Pong *PingPongMsg `json:"pong,omitempty"`

	// ChannelMbsListResult is one page of the answer to a
	// listChannelMembers request.
	ChannelMbsListResult *SSToClientChannelMbsListResult `json:"channelMbsListResult,omitempty"`

	// ChannelListResult is one page of the answer to a listChannels
	// request.
	ChannelListResult *SSToClientChannelListResult `json:"channelListResult,omitempty"`
}

// UserProfile is the profile data the SS holds about a registered
// subscriber.
type UserProfile struct {
	SubscriberId SubscriberId `json:"subscriberId"`
	ChannelId    ChannelId    `json:"channelId"`
	Username     string       `json:"username"`
}

// ChannelProfile is the profile data the SS holds about a channel.
type ChannelProfile struct {
	ChannelId   ChannelId `json:"channelId"`
	ChannelName string    `json:"channelName"`
}

// RegisterResult is the SS's answer to a successful registration: the
// channel and the subscriber id the registration is bound to. The
// subscriber id is assigned by the SS when the request left it empty.
type RegisterResult struct {
	ChannelId    ChannelId    `json:"channelId"`
	SubscriberId SubscriberId `json:"subscriberId"`
}

// PingPongMsg is a ping or ping-reply message: between two clients
// (relayed by the SS), or between a client and the SS itself (the SS
// answers a c2s ping with a s2c pong).
type PingPongMsg struct {
	// PingId distinguishes multiple simultaneous ping sessions.
	PingId         string `json:"pingId"`
	SequenceNumber uint64 `json:"sequenceNumber"`

	// AckSequenceNumber is sent in a pong message and should be seq + 1.
	AckSequenceNumber uint64 `json:"ackSequenceNumber"`
}

// ClientToClientEv is a client-to-client signalling event; a signalling
// server simply passes it, rewriting only the envelope's To EPAddr to
// the address (ChannelId, ToSubscriber) resolves to.
type ClientToClientEv struct {
	FromSubscriber SubscriberId `json:"fromSubscriber"`
	ToSubscriber   SubscriberId `json:"toSubscriber"`

	// ChannelId scopes the two subscriber ids — think of a subscriber id
	// as an IP address and the channel as the VLAN it lives in: a
	// subscriber id alone does not determine an endpoint, only
	// (ChannelId, SubscriberId) does. There is deliberately a single
	// channel id, no from/to pair: caller and callee are expected to be
	// registered in the same channel.
	ChannelId ChannelId `json:"channelId"`

	// SessionDesc is a WebRTC/SIP-flavor session description, e.g.
	// {type: "answer", sdp: "..."}. It marshals to the same JSON shape
	// as the browser's RTCSessionDescription.
	SessionDesc *webrtc.SessionDescription `json:"sessionDesc,omitempty"`

	// RTCICECandidate is a trickle ICE candidate. It marshals to the
	// same JSON shape as the browser's RTCIceCandidateInit.
	RTCICECandidate *webrtc.ICECandidateInit `json:"rtcICECandidate,omitempty"`

	// Ping is an out-of-band ping message.
	Ping *PingPongMsg `json:"ping,omitempty"`

	// Pong is an out-of-band ping reply message.
	Pong *PingPongMsg `json:"pong,omitempty"`
}

// SignallingEvent is the single envelope type every SS message takes.
type SignallingEvent struct {
	From EPAddr `json:"from"`
	To   EPAddr `json:"to"`

	// MsgId is generated uniquely, statelessly, independently per
	// message.
	MsgId MsgId `json:"msgId"`

	// InReplyTo holds the message id of the origin message being
	// answered.
	InReplyTo *MsgId `json:"inReplyTo,omitempty"`

	C2SEv *ClientToSSEv     `json:"c2SEv,omitempty"`
	S2CEv *SSToClientEv     `json:"s2CEv,omitempty"`
	C2CEv *ClientToClientEv `json:"c2CEv,omitempty"`
}

// SignallingServiceProvider is a signalling server: it consumes parsed
// SignallingEvents from inMsg and emits replies and relayed events on
// outMsg. It handles messages, and messages only — all transport
// concerns (HTTP, WebSocket, serialization) belong to the caller.
//
// Run blocks until ctx is done, inMsg is closed, or the implementation
// is shut down, and closes outMsg when it returns; callers must not
// close outMsg themselves. Call Run exactly once per instance.
type SignallingServiceProvider interface {
	Run(ctx context.Context, inMsg <-chan *SignallingEvent, outMsg chan<- *SignallingEvent)
}

// SimpleOnMemorySSProvider is an in-memory SignallingServiceProvider
// that implements only the main channel (WellKnownChIdMain).
//
// It follows the CSP pattern: the single Run goroutine owns all state on
// its own stack and is fed exclusively by inMsg (the service channel),
// so the data path needs no mutexes. The only synchronization primitives
// are the channels themselves and a sync.Once that makes Shutdown
// idempotent.
//
// Registration state lives until it ages out or Run returns: the
// TypeScript prototype defines no unregister/leave event yet. Every
// subscriber carries a lastActive timestamp, refreshed by any activity
// from its address — including c2s liveness pings; a registration whose
// lastActive is longer ago than the aging interval is considered
// invalid. Expiration is lazy: there is no timer; the list-members
// operation does an all-at-once mark-and-sweep of a channel's expired
// entries, while every other operation just tests the validity of the
// specific subscriber before using it.
type SimpleOnMemorySSProvider struct {
	aging        time.Duration
	shutdownCh   chan struct{}
	shutdownOnce sync.Once
}

// DefaultSubscriberAging is the default aging interval of a
// SimpleOnMemorySSProvider.
const DefaultSubscriberAging = 10 * time.Second

// mainChannelName is the profile name of the well-known main channel.
const mainChannelName = "main"

// The automatic subscriber id assignment range: a registration with an
// empty subscriber id is assigned the next free id in
// [autoSubscriberIdRangeStart, autoSubscriberIdRangeEnd).
const (
	autoSubscriberIdRangeStart = 1000
	autoSubscriberIdRangeEnd   = 2000 // exclusive: ids run 1000-1999
)

// NewSimpleOnMemorySSProvider constructs a SimpleOnMemorySSProvider with
// the default subscriber aging. Call Run to start processing events.
func NewSimpleOnMemorySSProvider() *SimpleOnMemorySSProvider {
	return NewSimpleOnMemorySSProviderWithAging(DefaultSubscriberAging)
}

// NewSimpleOnMemorySSProviderWithAging constructs a
// SimpleOnMemorySSProvider whose subscriber registrations expire after
// aging without activity. A non-positive aging selects the default.
func NewSimpleOnMemorySSProviderWithAging(aging time.Duration) *SimpleOnMemorySSProvider {
	if aging <= 0 {
		aging = DefaultSubscriberAging
	}
	return &SimpleOnMemorySSProvider{aging: aging, shutdownCh: make(chan struct{})}
}

// Shutdown asks Run to stop. It is idempotent and safe to call before,
// during, or after Run; events still buffered in inMsg when Shutdown
// takes effect are left unprocessed.
func (p *SimpleOnMemorySSProvider) Shutdown() {
	p.shutdownOnce.Do(func() { close(p.shutdownCh) })
}

// Run implements SignallingServiceProvider.
func (p *SimpleOnMemorySSProvider) Run(ctx context.Context, inMsg <-chan *SignallingEvent, outMsg chan<- *SignallingEvent) {
	defer close(outMsg)

	// All provider state lives on this goroutine's stack and every event
	// is handled sequentially, so no mutex is needed anywhere.
	channels := map[ChannelId]*channelState{
		WellKnownChIdMain: newChannelState(mainChannelName),
	}

	for {
		// Prioritize termination over pending input so Shutdown takes
		// effect even while inMsg holds buffered events.
		select {
		case <-ctx.Done():
			return
		case <-p.shutdownCh:
			return
		default:
		}
		select {
		case <-ctx.Done():
			return
		case <-p.shutdownCh:
			return
		case ev, ok := <-inMsg:
			if !ok {
				return
			}
			touch(channels, ev, time.Now())
			if !p.handle(ctx, channels, ev, outMsg) {
				return
			}
		}
	}
}

// handle dispatches a single event and reports whether the Run loop
// should continue (false means the provider is terminating mid-reply).
func (p *SimpleOnMemorySSProvider) handle(ctx context.Context, channels map[ChannelId]*channelState, ev *SignallingEvent, outMsg chan<- *SignallingEvent) bool {
	switch {
	case ev == nil:
		return true
	case ev.C2SEv != nil && ev.C2SEv.Register != nil:
		return p.handleRegister(ctx, channels, ev, outMsg)
	case ev.C2SEv != nil && ev.C2SEv.ChannelKeepAlive != nil:
		return p.handleChannelKeepAlive(ctx, channels, ev, outMsg)
	case ev.C2SEv != nil && ev.C2SEv.UserProfileQuery != nil:
		return p.handleQueryProfile(ctx, channels, ev, outMsg)
	case ev.C2SEv != nil && ev.C2SEv.Ping != nil:
		return p.handlePing(ctx, ev, outMsg)
	case ev.C2SEv != nil && ev.C2SEv.ListChannelMembers != nil:
		return p.handleListChannelMembers(ctx, channels, ev, outMsg)
	case ev.C2SEv != nil && ev.C2SEv.ListChannels != nil:
		return p.handleListChannels(ctx, channels, ev, outMsg)
	case ev.C2SEv != nil && ev.C2SEv.ChannelProfileQuery != nil:
		return p.handleQueryChannelProfile(ctx, channels, ev, outMsg)
	case ev.C2CEv != nil:
		return p.handleRelay(ctx, channels, ev, outMsg)
	default:
		// The TypeScript prototype defines no error code for malformed
		// events, so they are dropped silently.
		return true
	}
}

func (p *SimpleOnMemorySSProvider) handleRegister(ctx context.Context, channels map[ChannelId]*channelState, ev *SignallingEvent, outMsg chan<- *SignallingEvent) bool {
	reg := ev.C2SEv.Register
	ch, ok := channels[reg.ChannelId]
	if !ok {
		return p.send(ctx, outMsg, replyErr(ev, ErrorCodeChannelNotFound,
			"channel not found; currently only the main channel is implemented, please specify the correct channel id"))
	}
	now := time.Now()
	// An empty subscriber id asks the SS to assign one, sequentially,
	// from the automatic assignment range; the assigned id is echoed in
	// the registerResult reply. Empty always creates a fresh subscriber,
	// even from an already-registered tuple.
	if reg.SubscriberId == "" {
		reg.SubscriberId = ch.assignSubscriberId(now, p.aging)
		if reg.SubscriberId == "" {
			return p.send(ctx, outMsg, replyErr(ev, ErrorCodeNoSubscriberIdAvailable,
				"no subscriber id is available in the automatic assignment range 1000-1999"))
		}
	}
	// A client registers exactly once per incarnation — a browser page
	// load mints a fresh subscriber id (a reload is a new incarnation; see
	// the browser's registerSubscriber). Any OTHER live registration of
	// the registrar's own (user id, user session id) tuple is therefore a
	// previous, now-dead incarnation of the same client, and is evicted
	// here: the incarnation change becomes immediately visible to member
	// listings (peers rebuild their stale peer sessions for the old id at
	// once, not after aging), and the dead registration cannot keep
	// limping along on its successor's address-matched activity (touch).
	// The registrar's own id is exempt, so an ordinary same-id refresh —
	// a roaming reconnect, or a bot re-registering its configured id —
	// keeps its registration (two tabs of one session do compete; the
	// later load wins, matching the one-incarnation-per-page model).
	for id, sub := range ch.subscribers {
		if id != reg.SubscriberId && sameRegistrationTuple(sub.addr, ev.From) {
			ch.evict(id)
		}
	}
	if existing := ch.live(reg.SubscriberId, now, p.aging); existing != nil {
		if !sameRegistrationTuple(existing.addr, ev.From) {
			return p.send(ctx, outMsg, replyErr(ev, ErrorCodeSubscriberIdIsRegistered,
				"subscriber id has already been registered by another (user id, user session id, channel id) tuple"))
		}
		// Re-registration by the same tuple is a refresh: renew
		// lastActive, and update the username if it changed.
		if reg.Username != existing.username {
			if owner, taken := ch.idsByUsername[reg.Username]; taken &&
				ch.live(owner, now, p.aging) != nil {
				return p.send(ctx, outMsg, replyErr(ev, ErrorCodeUsernameTaken,
					"the requested username has already been taken by someone else"))
			}
			delete(ch.idsByUsername, existing.username)
			ch.idsByUsername[reg.Username] = existing.id
			existing.username = reg.Username
		}
		existing.lastActive = now
		return p.send(ctx, outMsg, reply(ev, &SSToClientEv{RegisterResult: &RegisterResult{
			ChannelId:    reg.ChannelId,
			SubscriberId: reg.SubscriberId,
		}}))
	}
	if owner, taken := ch.idsByUsername[reg.Username]; taken &&
		ch.live(owner, now, p.aging) != nil {
		return p.send(ctx, outMsg, replyErr(ev, ErrorCodeUsernameTaken,
			"the requested username has already been taken by someone else"))
	}
	ch.subscribers[reg.SubscriberId] = &subscriberState{
		id:         reg.SubscriberId,
		username:   reg.Username,
		addr:       ev.From,
		lastActive: now,
	}
	ch.idsByUsername[reg.Username] = reg.SubscriberId
	return p.send(ctx, outMsg, reply(ev, &SSToClientEv{RegisterResult: &RegisterResult{
		ChannelId:    reg.ChannelId,
		SubscriberId: reg.SubscriberId,
	}}))
}

// handleChannelKeepAlive renews the caller's membership of the channel:
// the named subscriber's lastActive is refreshed, silently — a success
// is answered with nothing. Only the registration's own (user id, user
// session id) tuple may renew it: a keepalive for an unknown channel,
// an unknown or expired subscriber, or a mismatched identity is
// rejected (ChannelNotFound / SubscriberNotFound), so the caller learns
// its membership is gone and can re-register.
func (p *SimpleOnMemorySSProvider) handleChannelKeepAlive(ctx context.Context, channels map[ChannelId]*channelState, ev *SignallingEvent, outMsg chan<- *SignallingEvent) bool {
	ka := ev.C2SEv.ChannelKeepAlive
	ch, ok := channels[ka.ChannelId]
	if !ok {
		return p.send(ctx, outMsg, replyErr(ev, ErrorCodeChannelNotFound, "channel not found"))
	}
	sub := ch.live(ka.SubscriberId, time.Now(), p.aging)
	if sub == nil {
		return p.send(ctx, outMsg, replyErr(ev, ErrorCodeSubscriberNotFound,
			"subscriber not found in the given channel"))
	}
	if !sameRegistrationTuple(sub.addr, ev.From) {
		return p.send(ctx, outMsg, replyErr(ev, ErrorCodeSubscriberNotFound,
			"the subscriber's registration is bound to another (user id, user session id) tuple"))
	}
	sub.lastActive = time.Now()
	return true
}

// sameRegistrationTuple reports whether from is the non-empty (user id,
// user session id) identity a subscriber registered with. The channel id
// of the tuple is equal by construction: both resolved to the same
// channel.
func sameRegistrationTuple(registered, from EPAddr) bool {
	return from.UserId != "" && from.UserSessionId != "" &&
		registered.UserId == from.UserId && registered.UserSessionId == from.UserSessionId
}

func (p *SimpleOnMemorySSProvider) handleQueryProfile(ctx context.Context, channels map[ChannelId]*channelState, ev *SignallingEvent, outMsg chan<- *SignallingEvent) bool {
	query := ev.C2SEv.UserProfileQuery
	ch, ok := channels[query.ChannelId]
	if !ok {
		return p.send(ctx, outMsg, replyErr(ev, ErrorCodeChannelNotFound, "channel not found"))
	}
	sub := ch.live(query.SubscriberId, time.Now(), p.aging)
	if sub == nil {
		return p.send(ctx, outMsg, replyErr(ev, ErrorCodeSubscriberNotFound,
			"subscriber not found in the given channel"))
	}
	return p.send(ctx, outMsg, reply(ev, &SSToClientEv{Profile: &UserProfile{
		SubscriberId: sub.id,
		ChannelId:    query.ChannelId,
		Username:     sub.username,
	}}))
}

// handleQueryChannelProfile answers a channel profile query with the
// channel's profile; an unknown channel is rejected with
// ErrorCodeChannelNotFound.
func (p *SimpleOnMemorySSProvider) handleQueryChannelProfile(ctx context.Context, channels map[ChannelId]*channelState, ev *SignallingEvent, outMsg chan<- *SignallingEvent) bool {
	query := ev.C2SEv.ChannelProfileQuery
	ch, ok := channels[query.ChannelId]
	if !ok {
		return p.send(ctx, outMsg, replyErr(ev, ErrorCodeChannelNotFound, "channel not found"))
	}
	return p.send(ctx, outMsg, reply(ev, &SSToClientEv{ChannelProfile: &ChannelProfile{
		ChannelId:   query.ChannelId,
		ChannelName: ch.name,
	}}))
}

// handlePing answers a client-to-SS ping with an SS-to-client pong,
// keeping the ping id and answering ack = seq + 1. The pong's own
// sequence number is left 0: the SS keeps no per-session sequence space.
// The ping is a liveness probe between the client and the SS itself, so
// the subscriber does not need to be registered first.
func (p *SimpleOnMemorySSProvider) handlePing(ctx context.Context, ev *SignallingEvent, outMsg chan<- *SignallingEvent) bool {
	ping := ev.C2SEv.Ping
	return p.send(ctx, outMsg, reply(ev, &SSToClientEv{Pong: &PingPongMsg{
		PingId:            ping.PingId,
		AckSequenceNumber: ping.SequenceNumber + 1,
	}}))
}

// channelMembersPageSize bounds the number of members carried by one
// channelMbsListResult message; a larger member list is paged over
// multiple messages.
const channelMembersPageSize = 64

// handleListChannelMembers answers a listChannelMembers request with one
// or more channelMbsListResult messages, paged by channelMembersPageSize
// and each carrying a fresh MsgId with InReplyTo pointing at the
// request. Even an empty channel yields one (empty, final) page, so the
// client always observes hasMore=false. Members are listed sorted by
// subscriber id, so pages are stable.
func (p *SimpleOnMemorySSProvider) handleListChannelMembers(ctx context.Context, channels map[ChannelId]*channelState, ev *SignallingEvent, outMsg chan<- *SignallingEvent) bool {
	req := ev.C2SEv.ListChannelMembers
	ch, ok := channels[req.ChannelId]
	if !ok {
		return p.send(ctx, outMsg, replyErr(ev, ErrorCodeChannelNotFound, "channel not found"))
	}
	ch.sweep(time.Now(), p.aging)
	members := make([]SubscriberId, 0, len(ch.subscribers))
	for id := range ch.subscribers {
		members = append(members, id)
	}
	sort.Slice(members, func(i, j int) bool { return members[i] < members[j] })
	for start := 0; ; start += channelMembersPageSize {
		end := min(start+channelMembersPageSize, len(members))
		hasMore := end < len(members)
		if !p.send(ctx, outMsg, reply(ev, &SSToClientEv{ChannelMbsListResult: &SSToClientChannelMbsListResult{
			ChannelId: req.ChannelId,
			Members:   members[start:end],
			HasMore:   hasMore,
		}})) {
			return false
		}
		if !hasMore {
			return true
		}
	}
}

// channelListPageSize bounds the number of channel ids carried by one
// channelListResult message; a larger channel list is paged over
// multiple messages.
const channelListPageSize = 64

// handleListChannels answers a listChannels request with one or more
// channelListResult messages, paged by channelListPageSize and each
// carrying a fresh MsgId with InReplyTo pointing at the request. Even an
// empty channel list yields one (empty, final) page, so the client
// always observes hasMore=false. Channels are listed sorted by channel
// id, so pages are stable. No registration is needed first.
func (p *SimpleOnMemorySSProvider) handleListChannels(ctx context.Context, channels map[ChannelId]*channelState, ev *SignallingEvent, outMsg chan<- *SignallingEvent) bool {
	ids := make([]ChannelId, 0, len(channels))
	for id := range channels {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for start := 0; ; start += channelListPageSize {
		end := min(start+channelListPageSize, len(ids))
		hasMore := end < len(ids)
		if !p.send(ctx, outMsg, reply(ev, &SSToClientEv{ChannelListResult: &SSToClientChannelListResult{
			Channels: ids[start:end],
			HasMore:  hasMore,
		}})) {
			return false
		}
		if !hasMore {
			return true
		}
	}
}

func (p *SimpleOnMemorySSProvider) handleRelay(ctx context.Context, channels map[ChannelId]*channelState, ev *SignallingEvent, outMsg chan<- *SignallingEvent) bool {
	c2c := ev.C2CEv
	// Both subscribers are resolved in the event's channel: a subscriber
	// id is only meaningful within its channel.
	ch, ok := channels[c2c.ChannelId]
	if !ok {
		return p.send(ctx, outMsg, replyErr(ev, ErrorCodeChannelNotFound,
			"channel not found; currently only the main channel is implemented, please specify the correct channel id"))
	}
	now := time.Now()
	if ch.live(c2c.FromSubscriber, now, p.aging) == nil {
		return p.send(ctx, outMsg, replyErr(ev, ErrorCodeSubscriberNotFound,
			"from subscriber not found in the given channel"))
	}
	to := ch.live(c2c.ToSubscriber, now, p.aging)
	if to == nil {
		return p.send(ctx, outMsg, replyErr(ev, ErrorCodeSubscriberNotFound,
			"to subscriber not found in the given channel"))
	}
	// The SS translates (channel, subscriber) addressing into an EPAddr:
	// rewrite the destination (addressed to the SS on the way in) to the
	// address ToSubscriber was learned with, so transports can route the
	// event without knowing subscriber addressing. From is left
	// untouched; the rest of the event passes through unchanged.
	ev.To = to.addr
	return p.send(ctx, outMsg, ev)
}

// send emits ev on outMsg, applying backpressure. It returns false when
// the provider is terminating (ctx done or shut down) before the event
// could be handed over.
func (p *SimpleOnMemorySSProvider) send(ctx context.Context, outMsg chan<- *SignallingEvent, ev *SignallingEvent) bool {
	select {
	case outMsg <- ev:
		return true
	case <-ctx.Done():
		return false
	case <-p.shutdownCh:
		return false
	}
}

// reply builds the SS's answer to ev: From the well-known SS service,
// To the event's origin, correlated via InReplyTo.
func reply(ev *SignallingEvent, s2c *SSToClientEv) *SignallingEvent {
	inReplyTo := ev.MsgId
	return &SignallingEvent{
		From:      EPAddr{ServiceId: WellKnownSvcIdSS},
		To:        ev.From,
		MsgId:     newMsgId(),
		InReplyTo: &inReplyTo,
		S2CEv:     s2c,
	}
}

func replyErr(ev *SignallingEvent, code ErrorCode, msg string) *SignallingEvent {
	return reply(ev, &SSToClientEv{Err: &SSToClientErrEv{ErrorCode: code, ErrorMsg: msg}})
}

// newMsgId generates a message id uniquely, statelessly, independently.
func newMsgId() MsgId {
	return MsgId(uuid.NewString())
}

// subscriberState is what the provider knows about one registered
// subscriber of a channel.
type subscriberState struct {
	id       SubscriberId
	username string

	// addr is the EPAddr the subscriber registered with, learned from
	// the From field of the registration event.
	addr EPAddr

	// lastActive is the last time the subscriber did anything — any
	// event from its address counts, including c2s liveness pings.
	lastActive time.Time
}

// expired reports whether the registration is too long ago to be valid.
func (s *subscriberState) expired(now time.Time, aging time.Duration) bool {
	return now.Sub(s.lastActive) > aging
}

// channelState holds what the provider knows about one channel: its
// profile name and its subscribers, indexed by id and (uniquely) by
// username.
type channelState struct {
	name          string
	subscribers   map[SubscriberId]*subscriberState
	idsByUsername map[string]SubscriberId

	// nextAutoId is where the next automatic subscriber id assignment
	// scan starts.
	nextAutoId int
}

func newChannelState(name string) *channelState {
	return &channelState{
		name:          name,
		subscribers:   make(map[SubscriberId]*subscriberState),
		idsByUsername: make(map[string]SubscriberId),
		nextAutoId:    autoSubscriberIdRangeStart,
	}
}

// assignSubscriberId picks the next free subscriber id in the automatic
// assignment range, scanning sequentially from where the last assignment
// left off and wrapping around at most once; expired ids on the way are
// freed by live. It returns "" when every id in the range is taken.
func (ch *channelState) assignSubscriberId(now time.Time, aging time.Duration) SubscriberId {
	for i := 0; i < autoSubscriberIdRangeEnd-autoSubscriberIdRangeStart; i++ {
		id := SubscriberId(strconv.Itoa(ch.nextAutoId))
		ch.nextAutoId++
		if ch.nextAutoId >= autoSubscriberIdRangeEnd {
			ch.nextAutoId = autoSubscriberIdRangeStart
		}
		if ch.live(id, now, aging) == nil {
			return id
		}
	}
	return ""
}

// sweep evicts every expired subscriber of the channel, freeing its
// subscriber id and username. Only the list-members operation does this
// all-at-once mark-and-sweep; other operations use live instead.
func (ch *channelState) sweep(now time.Time, aging time.Duration) {
	for id, sub := range ch.subscribers {
		if sub.expired(now, aging) {
			ch.evict(id)
		}
	}
}

// live returns the valid subscriber with the given id, or nil. An
// expired entry is not a live subscriber: it is evicted on the spot,
// freeing its subscriber id and username.
func (ch *channelState) live(id SubscriberId, now time.Time, aging time.Duration) *subscriberState {
	sub, ok := ch.subscribers[id]
	if !ok {
		return nil
	}
	if sub.expired(now, aging) {
		ch.evict(id)
		return nil
	}
	return sub
}

// evict removes a subscriber of the channel. The username is freed only
// while it still resolves to this subscriber: an incarnation replacement
// (see handleRegister) may have re-bound it to the successor's fresh
// registration, whose binding must survive this eviction.
func (ch *channelState) evict(id SubscriberId) {
	if sub, ok := ch.subscribers[id]; ok {
		if ch.idsByUsername[sub.username] == id {
			delete(ch.idsByUsername, sub.username)
		}
		delete(ch.subscribers, id)
	}
}

// touch refreshes the lastActive timestamp of every subscriber whose
// learned address matches the event's From, and of a c2c event's
// FromSubscriber in the event's own channel. Any activity counts —
// including c2s liveness pings — so a subscriber that sends something at
// least every aging interval keeps its registration valid. It is
// O(channels × subscribers) per event, which is fine at prototype scale.
func touch(channels map[ChannelId]*channelState, ev *SignallingEvent, now time.Time) {
	for _, ch := range channels {
		for _, sub := range ch.subscribers {
			if ev.From.UserId != "" && sub.addr.UserId == ev.From.UserId &&
				sub.addr.UserSessionId == ev.From.UserSessionId {
				sub.lastActive = now
			}
		}
	}
	if c2c := ev.C2CEv; c2c != nil {
		if ch, ok := channels[c2c.ChannelId]; ok {
			if sub, ok := ch.subscribers[c2c.FromSubscriber]; ok {
				sub.lastActive = now
			}
		}
	}
}

var _ SignallingServiceProvider = (*SimpleOnMemorySSProvider)(nil)
