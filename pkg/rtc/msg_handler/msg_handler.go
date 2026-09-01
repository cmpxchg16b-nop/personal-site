// Package msg_handler is the data-channel layer of a headless bot,
// between the bot client (pkg/rtc's HeadlessRTCClient, which brokers the
// peer sessions and dispatches their data channels by label) and the
// bot's own message policy. Its Server serves a pair's two well-known
// data channels and distills their raw frames into bot messages for a
// BotMessageHandler — the way an http.Server distills a connection's
// bytes into requests for an http.Handler.
//
// A "message" at this layer is a higher-level construct than a raw
// data-channel frame:
//
//   - a plain-text chat line (ChatMessage),
//   - one accepted chunk of an inbound file transfer (FileChunk),
//   - a completely transferred file attachment (Attachment — a file,
//     photo, or video attachment),
//   - one message of a call's SIP-subset dialog (SipMessage) — an
//     incoming INVITE answered with the ResponseWriter's Accept or
//     Reject, an outgoing call opened with its Invite, the call's media
//     offered as webrtc TrackLocals and the peer's inbound media arriving
//     at the registered OnTrack callback.
//
// The protocol mechanics stay with the Server, below the interface, and
// are no concern of a BotMessageHandler: malformed frames are dropped
// silently, every accepted message is bounced back to its sender with the
// echo flag set (echoes and the call protocol excepted — the classical
// conditioning of the messaging channel), inbound file streams are
// reassembled strictly and every accepted chunk is acknowledged. What the
// bot DOES with a message is a BotMessageHandler's alone; it answers
// through the ResponseWriter it is handed.
package msg_handler

import "context"

// BotMessageHandler handles a bot's messages — the counterpart of
// http.Handler for the messages a Server distills from a pair's data
// channels.
//
// The Server invokes the methods synchronously on the data channel's own
// goroutine as messages arrive: invocations on one channel are serialized
// and in order — a file's chunks arrive in order, and its HandleAttachment
// follows the final HandleFileChunk — but different channels of one peer,
// and different peers, run concurrently. State an implementation keeps
// across invocations is its own concern, the way it is in any bot
// framework; one guarantee helps: one transfer's chunks never overlap, so
// per-transfer state keyed by file id needs no synchronization beyond a
// concurrent map (sync.Map). A slow handler stalls its channel, like a
// slow http.Handler stalls its connection.
//
// ctx is the peer session's context: it is canceled when the session ends
// (the peer dropped out, or the client's Run ended).
//
// Like http.Handler's ServeHTTP, the methods return nothing: a handler
// answers the message — a reply, an amendment, a call's acceptance or
// rejection — through the ResponseWriter, or not at all.
type BotMessageHandler interface {
	// HandleChatMessage handles a plain-text chat line.
	HandleChatMessage(ctx context.Context, msg *ChatMessage, w ResponseWriter)

	// HandleFileChunk handles one accepted chunk of an inbound file
	// transfer: the chunk continues the file's contiguous prefix exactly
	// (a gap, an overlap, or a mismatched total has already dropped the
	// stream) and has already been acknowledged. announcement is the
	// transfer's announcement message when one was seen — cross-channel
	// ordering is not guaranteed, so it is nil when the chunk arrived
	// first (or none was sent).
	HandleFileChunk(ctx context.Context, announcement *FileAnnouncement, chunk *FileChunk, w ResponseWriter)

	// HandleAttachment handles a completely transferred file: every chunk
	// of its stream accepted and concatenated into attachment.Data.
	// announcement is as in HandleFileChunk.
	HandleAttachment(ctx context.Context, announcement *FileAnnouncement, attachment *Attachment, w ResponseWriter)

	// HandleCalling handles one message of a call's SIP-subset dialog: an
	// INVITE (which rings the bot), its CANCEL, a BYE, or a response to
	// the bot's own INVITE. Dialog messages are never bounced, so the
	// handler sees them exactly once.
	HandleCalling(ctx context.Context, sip *SipMessage, w ResponseWriter)
}
