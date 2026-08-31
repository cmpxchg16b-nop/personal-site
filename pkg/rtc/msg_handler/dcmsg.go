package msg_handler

// This file is the Go mirror of the browser's messaging data channel
// ("dcmsg") codec — web/site/src/api/ss/datachannel.tsx is the source of
// truth. Every frame is one JSON DCMsg; the decode rules (malformed frames
// are dropped silently) and the echo discipline (a recipient bounces every
// accepted message back with the echo flag set — except the call protocol,
// application/x-sip, which is never bounced) live here; what a bot does
// beyond that lives behind the BotMessageHandler interface (server.go
// dispatches it).
//
// The decode side works on the raw JSON fields, so a bounce re-encodes the
// frame it actually received — unknown fields and exact number literals
// ride along untouched, like the frontend's {...msg, echo: true}.

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"

	"personal-site/pkg/models/ss"
)

// The DCMsg format version and the body kinds (the mimeType field).
const (
	dcMsgMimeVersion            = "1.0"
	dcMsgMimePlaintext          = "text/plain"
	dcMsgMimeFileTransferStatus = "application/x-file-transfer-status"
	dcMsgMimeChatControl        = "application/x-chat-control"
	dcMsgMimeSip                = "application/x-sip"
)

// The SIP-subset start lines (see dcMsgMimeSip): the request methods and
// the INVITE's final responses.
const (
	sipMethodInvite = "INVITE"
	sipMethodCancel = "CANCEL"
	sipMethodBye    = "BYE"

	sipResponseOKCode        = 200
	sipResponseOKPhrase      = "OK"
	sipResponseDeclineCode   = 603
	sipResponseDeclinePhrase = "Decline"
)

// The phone call's logged UI states (the INVITE's X-Call-Status header)
// and kinds (the X-Media header).
const (
	callStatusInviting  = "inviting"
	callStatusAccepted  = "accepted"
	callStatusRejected  = "rejected"
	callStatusCancelled = "cancelled"
	callStatusEnded     = "ended"

	callKindVoice = "voice"
	callKindVideo = "video"
)

// The file transfer's render kinds (the sender's explicit choice) and
// lifecycle states.
const (
	fileKindFile  = "file"
	fileKindImage = "image"
	fileKindVideo = "video"

	fileStatusPending = "pending"
	fileStatusRunning = "running"
	fileStatusDone    = "done"
)

// dcSipResponse is a SIP status line: the response code and its reason
// phrase, one of the well-known pairs above.
type dcSipResponse struct {
	Code   int    `json:"code"`
	Phrase string `json:"phrase"`
}

// dcSipBody is the wire form of a SIP-subset DCMsg body (the frontend's
// DCSip): one message of a call's dialog. Exactly one of Method / Response
// is set; the X-* extension headers belong to the INVITE alone.
type dcSipBody struct {
	CallId      string         `json:"callId"`
	Method      string         `json:"method,omitempty"`
	Response    *dcSipResponse `json:"response,omitempty"`
	XMedia      string         `json:"X-Media,omitempty"`
	XCallStatus string         `json:"X-Call-Status,omitempty"`
}

// dcChatControlBody is the wire form of a chat-control DCMsg body (the
// frontend's DCChatControl) — the subset the echo bot sends: an "amend"
// rewriting a text message's plaintext.
type dcChatControlBody struct {
	Subtype         string   `json:"subtype"`
	TargetMessageId ss.MsgId `json:"targetMessageId"`
	Text            string   `json:"text,omitempty"`
}

// dcMsgOut is the wire form of an outbound DCMsg — the fields the echo bot
// ever sends (plaintext replies, SIP dialog messages, chat-control
// amends), in the field order of the frontend's builders.
type dcMsgOut struct {
	MimeVersion       string             `json:"mimeVersion"`
	ChannelId         ss.ChannelId       `json:"channelId"`
	FromSubscriberId  ss.SubscriberId    `json:"fromSubscriberId"`
	ToSubscriberId    ss.SubscriberId    `json:"toSubscriberId"`
	CreationTimestamp float64            `json:"creationTimestamp"` // Unix seconds
	MsgId             ss.MsgId           `json:"msgId"`
	InReplyTo         ss.MsgId           `json:"inReplyTo,omitempty"`
	MimeType          string             `json:"mimeType"`
	Plaintext         string             `json:"plaintext"`
	ChatControl       *dcChatControlBody `json:"chatControl,omitempty"`
	Sip               *dcSipBody         `json:"sip,omitempty"`
}

// newDCMsgOut starts an outbound DCMsg to toSubscriberId, generating a fresh
// msg id and stamping the creation time — the shared base of the
// frontend's newDCMsg / newChatControlDCMsg / newSipDCMsg builders.
func newDCMsgOut(channelId ss.ChannelId, from, to ss.SubscriberId) *dcMsgOut {
	return &dcMsgOut{
		MimeVersion:       dcMsgMimeVersion,
		ChannelId:         channelId,
		FromSubscriberId:  from,
		ToSubscriberId:    to,
		CreationTimestamp: float64(time.Now().UnixMilli()) / 1000,
		MsgId:             ss.MsgId(uuid.NewString()),
	}
}

// encode serializes the message for the wire. A nil return means the
// (fixed-shape) message could not be encoded — in practice unreachable.
func (m *dcMsgOut) encode() []byte {
	data, err := json.Marshal(m)
	if err != nil {
		return nil
	}
	return data
}

// dcMsgIn is one decoded, validated inbound DCMsg. Raw holds the frame's
// top-level fields untouched, so echoFrame can bounce the frame exactly as
// received.
type dcMsgIn struct {
	raw       map[string]json.RawMessage
	channelId ss.ChannelId
	from      ss.SubscriberId
	to        ss.SubscriberId
	msgId     ss.MsgId
	inReplyTo ss.MsgId  // optional; not validated (the frontend never checks it)
	timestamp time.Time // the sender-stamped creation time; zero when omitted
	mimeType  string
	plaintext string
	echo      bool

	// The validated bodies the Server acts on; nil unless the mimeType says so.
	sip          *dcSipIn
	fileTransfer *fileTransferBody
}

// dcSipIn is the validated, normalized form of an inbound SIP body: a
// request (Method set) XOR a response (Response set) — never both.
type dcSipIn struct {
	callId   string
	method   string
	response *dcSipResponse
	xMedia   string // the INVITE's X-Media, when present
}

// echoFrame is the bounce: the received frame re-encoded with the echo
// flag set — the mirror of the frontend's encodeDCMsg({...msg, echo: true}),
// preserving every field (known or not) byte for byte.
func (m *dcMsgIn) echoFrame() []byte {
	m.raw["echo"] = json.RawMessage("true")
	data, err := json.Marshal(m.raw)
	if err != nil { // the raw fields decoded once; they encode back
		return nil
	}
	return data
}

// decodeDCMsg parses one dcmsg frame, returning nil for malformed frames
// (dropped silently, mirroring the frontend's decodeDCMsg and the SS's
// rule for malformed events).
func decodeDCMsg(data []byte) *dcMsgIn {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return nil
	}
	channelId, ok := requiredStringField(fields, "channelId")
	if !ok {
		return nil
	}
	from, ok := requiredStringField(fields, "fromSubscriberId")
	if !ok {
		return nil
	}
	to, ok := requiredStringField(fields, "toSubscriberId")
	if !ok {
		return nil
	}
	msgId, ok := requiredStringField(fields, "msgId")
	if !ok {
		return nil
	}
	mimeType, ok := requiredStringField(fields, "mimeType")
	if !ok {
		return nil
	}
	plaintext, ok := requiredStringField(fields, "plaintext")
	if !ok {
		return nil
	}
	echo, valid := optionalBoolField(fields, "echo")
	if !valid {
		return nil
	}
	msg := &dcMsgIn{
		raw:       fields,
		channelId: ss.ChannelId(channelId),
		from:      ss.SubscriberId(from),
		to:        ss.SubscriberId(to),
		msgId:     ss.MsgId(msgId),
		mimeType:  mimeType,
		plaintext: plaintext,
		echo:      echo,
	}
	if inReplyTo, present, valid := optionalStringField(fields, "inReplyTo"); valid && present {
		msg.inReplyTo = ss.MsgId(inReplyTo)
	}
	if ts, present, valid := optionalNumberField(fields, "creationTimestamp"); valid && present {
		// Unix seconds, fractional (the frontend's Date.now()/1000).
		msg.timestamp = time.UnixMilli(int64(ts * 1000))
	}
	// The body must be present and well-formed for the kinds that carry
	// one; any other mimeType is body-less and accepted as-is.
	switch mimeType {
	case dcMsgMimeFileTransferStatus:
		raw := fields["fileTransfer"]
		if raw == nil {
			return nil
		}
		body, ok := decodeFileTransferBody(raw)
		if !ok {
			return nil
		}
		msg.fileTransfer = body
	case dcMsgMimeChatControl:
		raw := fields["chatControl"]
		if raw == nil || !isWellFormedChatControlBody(raw) {
			return nil
		}
	case dcMsgMimeSip:
		raw := fields["sip"]
		if raw == nil {
			return nil
		}
		sip := decodeSipBody(raw)
		if sip == nil {
			return nil
		}
		msg.sip = sip
	}
	return msg
}

// requiredStringField reads a mandatory string field, mirroring the
// frontend's typeof checks: absent, null, and non-string all fail.
func requiredStringField(fields map[string]json.RawMessage, key string) (string, bool) {
	raw := fields[key]
	if raw == nil || string(raw) == "null" {
		return "", false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", false
	}
	return s, true
}

// optionalStringField reads an optional string field: absent is valid and
// not present; present-but-null or present-but-not-a-string is invalid —
// the frontend's `x !== undefined && typeof x !== "string"` rule.
func optionalStringField(fields map[string]json.RawMessage, key string) (value string, present, valid bool) {
	raw := fields[key]
	if raw == nil {
		return "", false, true
	}
	if string(raw) == "null" {
		return "", false, false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", false, false
	}
	return s, true, true
}

// optionalBoolField reads an optional boolean field with the same
// absent/null/typed discipline as optionalStringField.
func optionalBoolField(fields map[string]json.RawMessage, key string) (value, valid bool) {
	raw := fields[key]
	if raw == nil {
		return false, true
	}
	if string(raw) == "null" {
		return false, false
	}
	var b bool
	if err := json.Unmarshal(raw, &b); err != nil {
		return false, false
	}
	return b, true
}

// optionalNumberField reads an optional JSON number (Unix seconds for a
// timestamp) with the same absent/null/typed discipline as
// optionalStringField.
func optionalNumberField(fields map[string]json.RawMessage, key string) (value float64, present, valid bool) {
	raw := fields[key]
	if raw == nil {
		return 0, false, true
	}
	if string(raw) == "null" {
		return 0, false, false
	}
	var n json.Number
	if err := json.Unmarshal(raw, &n); err != nil {
		return 0, false, false
	}
	f, err := n.Float64()
	if err != nil {
		return 0, false, false
	}
	return f, true, true
}

// requiredUintField reads a mandatory non-negative integer field (a byte
// count). JSON numbers are floats; the wire's byte counts are integral and
// far below 2^53, so the conversion is exact.
func requiredUintField(fields map[string]json.RawMessage, key string) (uint64, bool) {
	f, present, valid := optionalNumberField(fields, key)
	if !present || !valid || f < 0 {
		return 0, false
	}
	return uint64(f), true
}

// requiredIntField reads a mandatory integer field.
func requiredIntField(fields map[string]json.RawMessage, key string) (int, bool) {
	raw := fields[key]
	if raw == nil || string(raw) == "null" {
		return 0, false
	}
	var n int
	if err := json.Unmarshal(raw, &n); err != nil {
		return 0, false
	}
	return n, true
}

// fileTransferBody is a validated DCFileTransfer body (the frontend's
// isWellFormedFileTransfer): the announced file's id and metadata.
type fileTransferBody struct {
	fileId      string
	kind        string // file / image / video
	filename    string
	mimeType    string
	totalBytes  uint64
	transferred uint64
}

// decodeFileTransferBody validates a DCFileTransfer body, returning nil-ok
// when malformed.
func decodeFileTransferBody(raw json.RawMessage) (*fileTransferBody, bool) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, false
	}
	fileId, ok := requiredStringField(fields, "fileId")
	if !ok {
		return nil, false
	}
	kind, ok := requiredStringField(fields, "kind")
	if !ok || (kind != fileKindFile && kind != fileKindImage && kind != fileKindVideo) {
		return nil, false
	}
	filename, ok := requiredStringField(fields, "filename")
	if !ok {
		return nil, false
	}
	mimeType, ok := requiredStringField(fields, "fileMIMEType")
	if !ok {
		return nil, false
	}
	totalBytes, ok := requiredUintField(fields, "fileSizeTotalBytes")
	if !ok {
		return nil, false
	}
	transferred, ok := requiredUintField(fields, "fileSizeTransferred")
	if !ok {
		return nil, false
	}
	status, ok := requiredStringField(fields, "fileTransferStatus")
	if !ok || (status != fileStatusPending && status != fileStatusRunning && status != fileStatusDone) {
		return nil, false
	}
	return &fileTransferBody{
		fileId:      fileId,
		kind:        kind,
		filename:    filename,
		mimeType:    mimeType,
		totalBytes:  totalBytes,
		transferred: transferred,
	}, true
}

// isWellFormedChatControlBody validates a DCChatControl body (the
// frontend's decode-time chat-control checks).
func isWellFormedChatControlBody(raw json.RawMessage) bool {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return false
	}
	subtype, ok := requiredStringField(fields, "subtype")
	if !ok || (subtype != "delete" && subtype != "amend") {
		return false
	}
	if _, ok := requiredStringField(fields, "targetMessageId"); !ok {
		return false
	}
	if _, _, valid := optionalStringField(fields, "text"); !valid {
		return false
	}
	if raw := fields["fileTransfer"]; raw != nil {
		if body, ok := decodeFileTransferBody(raw); !ok || body == nil {
			return false
		}
	}
	if raw := fields["sip"]; raw != nil {
		if decodeSipBody(raw) == nil {
			return false
		}
	}
	return true
}

// decodeSipBody validates a DCSip body (the frontend's isWellFormedSip): a
// callId, a start line that is a request line XOR a status line, and the
// X-* extension headers on the INVITE alone (X-Call-Status required
// there). Returns nil when malformed.
func decodeSipBody(raw json.RawMessage) *dcSipIn {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil
	}
	callId, ok := requiredStringField(fields, "callId")
	if !ok {
		return nil
	}
	method, hasMethod, valid := optionalStringField(fields, "method")
	if !valid {
		return nil
	}
	var response *dcSipResponse
	if raw := fields["response"]; raw != nil {
		r, ok := decodeSipResponseBody(raw)
		if !ok {
			return nil
		}
		response = r
	}
	// A SIP start line is a request line XOR a status line.
	if hasMethod == (response != nil) {
		return nil
	}
	xMedia, hasXMedia, valid := optionalStringField(fields, "X-Media")
	if !valid {
		return nil
	}
	xCallStatus, hasXCallStatus, valid := optionalStringField(fields, "X-Call-Status")
	if !valid {
		return nil
	}
	// Extension headers belong to the INVITE alone.
	if (!hasMethod || method != sipMethodInvite) && (hasXMedia || hasXCallStatus) {
		return nil
	}
	if response != nil {
		if (response.Code == sipResponseOKCode && response.Phrase == sipResponseOKPhrase) ||
			(response.Code == sipResponseDeclineCode && response.Phrase == sipResponseDeclinePhrase) {
			return &dcSipIn{callId: callId, response: response}
		}
		return nil
	}
	switch method {
	case sipMethodInvite:
		if !hasXCallStatus || !isCallStatus(xCallStatus) {
			return nil
		}
		if hasXMedia && xMedia != callKindVoice && xMedia != callKindVideo {
			return nil
		}
	case sipMethodCancel, sipMethodBye:
	default:
		return nil
	}
	return &dcSipIn{callId: callId, method: method, xMedia: xMedia}
}

// decodeSipResponseBody validates a SIP status line body.
func decodeSipResponseBody(raw json.RawMessage) (*dcSipResponse, bool) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return nil, false
	}
	code, ok := requiredIntField(fields, "code")
	if !ok {
		return nil, false
	}
	phrase, ok := requiredStringField(fields, "phrase")
	if !ok {
		return nil, false
	}
	return &dcSipResponse{Code: code, Phrase: phrase}, true
}

// isCallStatus reports whether s is one of the phone call's UI states.
func isCallStatus(s string) bool {
	switch s {
	case callStatusInviting, callStatusAccepted, callStatusRejected, callStatusCancelled, callStatusEnded:
		return true
	}
	return false
}
