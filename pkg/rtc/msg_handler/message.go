package msg_handler

// The bot-message types: what a BotMessageHandler sees. They are the
// higher-level readings of the wire protocols whose source of truth is the
// browser application (web/site/src/api/ss/datachannel.tsx,
// binaryframes.ts) — one dcmsg frame becomes a ChatMessage, a
// FileAnnouncement, or a SipMessage; a run of accepted dcbin FILE frames
// becomes the FileChunks plus one Attachment.

import (
	"time"

	"personal-site/pkg/models/ss"
)

// Message is the envelope every chat-channel message shares: which pair it
// belongs to, who sent it, its wire identity, and its threading anchor.
type Message struct {
	ChannelId ss.ChannelId
	From      ss.SubscriberId
	To        ss.SubscriberId
	MsgId     ss.MsgId
	// InReplyTo is the msg id the message threads on; empty when it
	// threads on nothing.
	InReplyTo ss.MsgId
	// Timestamp is the sender-stamped creation time; zero when the frame
	// omitted it.
	Timestamp time.Time
}

// ChatMessage is a plain-text chat line (the dcmsg "text/plain" kind).
type ChatMessage struct {
	Message
	Text string
}

// AttachmentKind is a transferred file's render kind — the sender's
// explicit choice on the announcement, never derived from the file's MIME
// type: AttachmentFile renders the download card, AttachmentImage and
// AttachmentVideo the inline media cards. The transfer path is the same
// for all three.
type AttachmentKind string

const (
	AttachmentFile  AttachmentKind = "file"
	AttachmentImage AttachmentKind = "image"
	AttachmentVideo AttachmentKind = "video"
)

// FileAnnouncement is the chat-channel message that announced a file
// transfer (the dcmsg "application/x-file-transfer-status" kind): its
// MsgId is the threading anchor a report about the transfer refers back
// to, and its metadata describes the file the binary channel then carries.
type FileAnnouncement struct {
	Message
	// FileId is the announced file's id in canonical form (lowercase
	// dashed UUID when parseable) — the form FileChunk and Attachment
	// also carry, so the two sides correlate directly.
	FileId         string
	Kind           AttachmentKind
	Filename       string
	MIMEType       string
	SizeTotalBytes uint64
}

// FileChunk is one accepted chunk of an inbound file transfer: its payload
// continues the file's contiguous prefix exactly, and the Server has
// already acknowledged it. Payload is valid until the handler returns —
// retain a copy, not the slice.
type FileChunk struct {
	ChannelId ss.ChannelId
	Peer      ss.SubscriberId
	// FileId is the transfer's id in canonical form (see
	// FileAnnouncement.FileId).
	FileId  string
	Seq     uint32
	Offset  uint64
	Total   uint64 // the whole file's size on the wire
	Payload []byte
	// Last is true when this chunk completes the file; HandleAttachment
	// follows it.
	Last bool
}

// Attachment is a completely transferred file: every chunk of its stream
// accepted and concatenated. Data is freshly assembled by the Server; the
// handler may retain it. Kind, Filename, and MIMEType come from the
// transfer's announcement and are zero when none was seen.
type Attachment struct {
	ChannelId ss.ChannelId
	Peer      ss.SubscriberId
	FileId    string
	Kind      AttachmentKind
	Filename  string
	MIMEType  string
	Data      []byte
}

// SipMethod is a SIP-subset request's start line (the method field of a
// dcmsg "application/x-sip" body).
type SipMethod string

const (
	SipMethodInvite SipMethod = "INVITE"
	SipMethodCancel SipMethod = "CANCEL"
	SipMethodBye    SipMethod = "BYE"
)

// The INVITE's well-known final responses (a SIP-subset status line).
const (
	SipCodeOK        = 200
	SipPhraseOK      = "OK"
	SipCodeDecline   = 603
	SipPhraseDecline = "Decline"
)

// SipResponse is a SIP-subset status line: the response code and its
// reason phrase, one of the well-known pairs above.
type SipResponse struct {
	Code   int
	Phrase string
}

// MediaKind is what a call carries (the INVITE's X-Media header): voice
// attaches the microphones only, video additionally the cameras.
type MediaKind string

const (
	MediaVoice MediaKind = "voice"
	MediaVideo MediaKind = "video"
)

// SipMessage is one message of a call's SIP-subset dialog: exactly one of
// Method (a request: INVITE, CANCEL, or BYE) and Response (200 OK or 603
// Decline) is set. Media says what an INVITE's call carries — MediaVoice
// when the wire omitted X-Media (voice is the default); it is meaningless
// on the dialog's other messages and left empty there.
type SipMessage struct {
	Message
	CallId   string
	Method   SipMethod
	Response *SipResponse
	Media    MediaKind
}
