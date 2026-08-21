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

// Message for listing the members of a channel, send from client to SS.
// The SS answers with one or more s2c channelMbsListResult messages, all
// inReplyTo the request's msg id.
export interface ClientToSSListChannelMembers {
  channelId: ChannelId;
}

// Client to signalling server event
export interface ClientToSSEv {
  register?: ClientToSSRegEv;

  userProfileQuery?: ClientToSSUserProfileQuery;

  /** ping the signalling server itself (out of band liveness/keepalive, e.g. from browsers, which cannot send protocol-level WebSocket pings); the SS answers with a s2c pong */
  ping?: PingPongMsg;

  /** list the members of a channel; the SS answers with one or more channelMbsListResult messages */
  listChannelMembers?: ClientToSSListChannelMembers;
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

// Signalling server to Client event
export interface SSToClientEv {
  // err is present only when there IS an error occurred, null or undefined otherwise.
  err?: SSToClientErrEv;

  // carries the reply payload of a user profile query; present only on profile query replies.
  profile?: UserProfile;

  // pong answering a c2s ping: keeps the ping id, ack = ping's seq + 1
  pong?: PingPongMsg;

  // one page of the answer to a listChannelMembers request
  channelMbsListResult?: SSToClientChannelMbsListResult;
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

  /** A WebRTC/SIP-flavor session description object, for example { type: 'answer', sdp: SDP } */
  sessionDesc?: RTCSessionDescription;

  /** Trickle ICE candidate */
  rtcICECandidate?: RTCIceCandidate;

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
