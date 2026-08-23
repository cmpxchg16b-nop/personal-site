"use client";

// globally and uniquely identifies a user
// implementation must treat it opaquely and must not guess or assume its formation
export type UserId = string;

// globally and uniquely identifies a user session, a user can have multiple sessions to the signalling server
// implementation must treat it opaquely and must not guess or assume its formation
export type UserSessionId = string;

// A entity that is neither a user nor does it has a 'session' to signalling server
// implementation should treat it opaquely too.
export type ServiceId = string;

// A subscriber identifier, local to a channel of a signalling server, opque. uniquely identifies an entity in a channel.
export type SubscriberId = string;

// A globally unique channel id
export type ChannelId = string;

// A msg id uniquely identify a message in global, and also for correlate request and reply. must be treated opaquely.
export type MsgId = string;

// Wellknown service id for signalling server
export const WELLKNOWN_SVC_ID_SS: ServiceId =
  "253dc952-a55b-4d01-a885-c5e240e95fdb";

// Wellknown channel id for main channel (a SS implementation should at least implement main channel)
export const WELLKNOWN_CH_ID_MAIN: ChannelId =
  "f887f5b0-7b78-4ceb-a051-f42879f9d98e";

// A packed address struct type
export interface EPAddr {
  /** in case the message is constructed at browser, the client can leave it empty, and the http/ws server handler populate it from cookie or http header or jwt. however the browser client code can still poplate it at will (for testing purpose). */
  userId?: UserId;

  /** in case the message is constructed at browser, the client can leave it empty, and the http/ws server handler populate it from cookie or http header or jwt. however the browser client code can still poplate it at will (for testing purpose). */
  userSessionId?: UserSessionId;

  /** in case the client is talking to a service */
  serviceId?: ServiceId;
}

// Message for a client register itself to the server, send from client to SS.
// A subscriber id is bound to zero or one (user id, user session id, channel id)
// tuple: re-registration of a live id by the same tuple is allowed and acts as a
// refresh (it may update the username); registration of an id bound to another
// tuple is rejected with ErrorCode.SubscriberIdIsRegistered.
export interface ClientToSSRegEv {
  /** if empty, the SS assigns a subscriber id sequentially from the
   * automatic assignment range 1000-1999 and echoes it in the
   * registerResult reply; an empty id always mints a fresh subscriber,
   * even from an already-registered tuple. Clients picking their own
   * subscriber ids are recommended to preserve that range for automatic
   * registration. */
  subscriberId: SubscriberId;
  channelId: ChannelId;
  // descriptive, lowercase, no-space, valid dns label for displaying the subscriber in UI
  username: string;
}

// Message for a subscriber query the user profile of another subscriber, send from client to SS
export interface ClientToSSUserProfileQuery {
  subscriberId: SubscriberId;
  channelId: ChannelId;
}

// Message for querying the profile of a channel, send from client to SS.
// The SS answers with a s2c channelProfile reply.
export interface ClientToSSChannelProfileQuery {
  channelId: ChannelId;
}

// Message for listing the members of a channel, send from client to SS.
// The SS answers with one or more s2c channelMbsListResult messages, all
// inReplyTo the request's msg id.
export interface ClientToSSListChannelMembers {
  channelId: ChannelId;
}

// Message for listing the channels of the signalling server, send from
// client to SS. It carries no fields. The SS answers with one or more s2c
// channelListResult messages, all inReplyTo the request's msg id.
export type ClientToSSListChannels = Record<string, never>;

// Client to signalling server event
export interface ClientToSSEv {
  register?: ClientToSSRegEv;

  userProfileQuery?: ClientToSSUserProfileQuery;

  /** query the profile of a channel; the SS answers with a s2c channelProfile */
  channelProfileQuery?: ClientToSSChannelProfileQuery;

  /** ping the signalling server itself (out of band liveness/keepalive, e.g. from browsers, which cannot send protocol-level WebSocket pings); the SS answers with a s2c pong */
  ping?: PingPongMsg;

  /** list the members of a channel; the SS answers with one or more channelMbsListResult messages */
  listChannelMembers?: ClientToSSListChannelMembers;

  /** list the channels of the server; the SS answers with one or more channelListResult messages */
  listChannels?: ClientToSSListChannels;
}

export enum ErrorCode {
  // subscriber id has been registered
  SubscriberIdIsRegistered = 1,

  // subscriber not found in the given channel
  SubscriberNotFound,

  // channel not found, or only main channel is implemented, please specify the correct channel id
  ChannelNotFound,

  // the username you requested in the registration has already been taken by someone else.
  UsernameTaken,

  // no subscriber id is free in the automatic assignment range 1000-1999 (all are registered)
  NoSubscriberIdAvailable,
}

export interface SSToClientErrEv {
  // error code must be well-defined
  errorCode: ErrorCode;

  // while error message is descriptive.
  errorMsg: string;
}

// The profile data the SS holds about a registered subscriber.
export interface UserProfile {
  subscriberId: SubscriberId;
  channelId: ChannelId;
  username: string;
}

// The profile data the SS holds about a channel.
export interface ChannelProfile {
  channelId: ChannelId;
  channelName: string;
}

// One page of a channel members list result. Multiple result messages can be
// sent per one listChannelMembers request, all correlated via inReplyTo; the
// client might decide a timeout on its own, or wait until hasMore is false.
export interface SSToClientChannelMbsListResult {
  channelId: ChannelId;
  // the subscriber ids of (a page of) the members of the channel; use
  // userProfileQuery to resolve a member's profile
  members: SubscriberId[];
  // true when more result messages follow for the same request
  hasMore: boolean;
}

// One page of a channel list result. Multiple result messages can be
// sent per one listChannels request, all correlated via inReplyTo; the
// client might decide a timeout on its own, or wait until hasMore is false.
export interface SSToClientChannelListResult {
  // (a page of) the channel ids of the server; use channelProfileQuery to
  // resolve a channel's profile
  channels: ChannelId[];
  // true when more result messages follow for the same request
  hasMore: boolean;
}

// The result of a successful registration, echoed back by the SS: the
// channel and the subscriber id the registration is bound to. The
// subscriber id is assigned by the SS when the request left it empty.
export interface RegisterResult {
  channelId: ChannelId;
  subscriberId: SubscriberId;
}

// Signalling server to Client event
export interface SSToClientEv {
  // err is present only when there IS an error occurred, null or undefined otherwise.
  err?: SSToClientErrEv;

  // carries the reply payload of a successful registration.
  registerResult?: RegisterResult;

  // carries the reply payload of a user profile query; present only on profile query replies.
  profile?: UserProfile;

  // carries the reply payload of a channel profile query; present only on channel profile query replies.
  channelProfile?: ChannelProfile;

  // pong answering a c2s ping: keeps the ping id, ack = ping's seq + 1
  pong?: PingPongMsg;

  // one page of the answer to a listChannelMembers request
  channelMbsListResult?: SSToClientChannelMbsListResult;

  // one page of the answer to a listChannels request
  channelListResult?: SSToClientChannelListResult;
}

// A ping or ping-reply message: between two clients (relayed by the SS), or
// between a client and the SS itself (the SS answers a c2s ping with a s2c pong).
export interface PingPongMsg {
  // ping id for distinguish multiple simultaneous ping sessions
  pingId: string;
  sequenceNumber: number;
  // send in pong message, should be seq + 1
  ackSequenceNumber: number;
}

// Client to Client signalling event, a signalling server will just simply pass it.
export interface ClientToClientEv {
  fromSubscriber: SubscriberId;

  toSubscriber: SubscriberId;

  /**
   * The channel scoping the two subscriber ids — think of a subscriber id as
   * an IP address and the channel as the VLAN it lives in: a subscriber id
   * alone does not determine an endpoint, only (channelId, subscriberId)
   * does. There is deliberately a single channel id, no from/to pair: caller
   * and callee are expected to be registered in the same channel.
   */
  channelId: ChannelId;

  /**
   * A WebRTC/SIP-flavor session description object, for example
   * { type: 'answer', sdp: SDP }. This is the JSON wire shape shared by the
   * browser's RTCSessionDescription (what JSON.stringify produces from it via
   * toJSON()) and pion's webrtc.SessionDescription on the server side; it can
   * be passed straight to RTCPeerConnection.setRemoteDescription().
   */
  sessionDesc?: RTCSessionDescriptionInit;

  /**
   * Trickle ICE candidate, in the JSON wire shape shared by the browser's
   * RTCIceCandidate (via toJSON()) and pion's webrtc.ICECandidateInit on the
   * server side; it can be passed straight to
   * RTCPeerConnection.addIceCandidate(). An empty candidate string signals
   * end-of-candidates.
   */
  rtcICECandidate?: RTCIceCandidateInit;

  /** out of band ping message */
  ping?: PingPongMsg;

  /** out of band ping reply message */
  pong?: PingPongMsg;
}

export interface SignallingEvent {
  from: EPAddr;
  to: EPAddr;

  /** generated uniquely, statelessly, independently per message. */
  msgId: MsgId;

  /** holds the message id of the origin message being answered. */
  inReplyTo?: MsgId;

  c2SEv?: ClientToSSEv;
  s2CEv?: SSToClientEv;
  c2CEv?: ClientToClientEv;
}

// ChatUser is the chat UI's view of a user: the app-level identity (id,
// display name, presence) plus the user's signalling subscription
// (channelId + subscriberId), when known.
export type ChatUser = {
  // The id is currently the user's signalling subscriber id: it is
  // guaranteed unique within its channel (see channelId), but NOT
  // globally — the same subscriber id can identify different users in
  // different channels, so a ChatUser is only addressable as
  // (channelId, id).
  id: string;
  name: string;
  online: boolean;
  // The signalling channel the user's subscription lives in — the scope
  // of subscriberId — when known.
  channelId?: ChannelId;
  // The user's signalling subscriber id (channel-local, assigned by the
  // SS on registration), when known.
  subscriberId?: SubscriberId;
};

// ChatChannel is the chat UI's view of a channel: its members form the
// sidebar tree's second level (excludes the current user — you don't
// open a direct message with yourself).
export type ChatChannel = {
  id: string;
  name: string;
  members: ChatUser[];
};
