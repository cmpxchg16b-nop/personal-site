package msg_handler

// This file is the Server: the data-channel layer of a bot's stack. It
// registers for the two well-known labels on a HeadlessRTCClient and owns
// every protocol mechanic below the BotMessageHandler interface:
//
//   - dcmsg (the messaging channel): decode and validation, the echo
//     discipline (every accepted message is bounced back verbatim with
//     echo set — echoes and the call protocol excepted), and the
//     file-transfer announcements the binary side correlates with.
//   - dcbin (the binary channel): strict reassembly of inbound file
//     streams (a gap, an overlap, or a mismatched total drops the
//     transfer) and the acknowledgement of every accepted frame, so the
//     sender's sliding window keeps advancing.
//
// What remains — what a message MEANS to the bot — is dispatched to the
// BotMessageHandler.
//
// Like the client and the bots before it, the Server follows the client's
// hub-and-notes model: a single hub goroutine owns the peer registry (the
// per-session messaging channel and the file-transfer announcements), fed
// by notes through serviceChan; the binary side queries it per chunk —
// per-report lookup is load-bearing, because a glare rebuild swaps the
// messaging channel mid-session and a cached one would go stale. The
// Server's hub lives for the Server's lifetime; there is no Close.

import (
	"context"
	"log/slog"

	"github.com/google/uuid"
	"github.com/pion/webrtc/v4"

	"personal-site/pkg/models/ss"
	"personal-site/pkg/rtc"
)

const (
	// DataChannelLabelMessages is the well-known label of the messaging
	// data channel every pair of peers brings up ("dcmsg"); the JSON
	// DCMsg protocol rides it.
	DataChannelLabelMessages = "dcmsg"

	// DataChannelLabelBinary is the well-known label of the binary data
	// channel brought up alongside the messaging one ("dcbin"); the
	// compact binary file-transfer frames ride it.
	DataChannelLabelBinary = "dcbin"
)

// Configuration configures a Server.
type Configuration struct {
	// Logger receives the Server's diagnostics; nil selects slog's
	// default logger.
	Logger *slog.Logger
}

// Server is the data-channel layer of a bot: constructed with NewServer,
// it serves a HeadlessRTCClient's two well-known data channels and hands
// the distilled bot messages to its BotMessageHandler. It is safe for
// concurrent use; the client invokes it once per (session, label) on its
// own goroutines, and the cross-channel state (the announcements ride
// dcmsg but describe the files arriving on dcbin) is funneled through the
// hub goroutine.
type Server struct {
	client  *rtc.HeadlessRTCClient
	handler BotMessageHandler
	logger  *slog.Logger

	// serviceChan carries every note to the hub goroutine — the only
	// goroutine touching the peer registry.
	serviceChan chan serverNote
}

// peerRecord is the hub's record of a live session: the session's
// messaging channel (replaced when a glare rebuild re-invokes the
// handler) and the peer's file-transfer announcements, keyed by canonical
// file id — the metadata a file's chunks and attachment are correlated
// with. Owned by the hub goroutine; never touched elsewhere.
type peerRecord struct {
	dcmsg     *webrtc.DataChannel
	announced map[string]*FileAnnouncement
}

// serverNote is one note for the Server's hub goroutine — the counterpart
// of the client's clientNote.
type serverNote interface{ isServerNote() }

// peerUpNote registers a session's messaging channel under its peer,
// replacing a previous invocation's (a glare rebuild re-invokes the
// handler with a fresh channel on the same session).
type peerUpNote struct {
	peer  ss.SubscriberId
	dcmsg *webrtc.DataChannel
}

// peerDownNote drops a session's registration as the session ends —
// unless a rebuilt session's has already replaced it (the messaging
// channel's identity tells the two apart).
type peerDownNote struct {
	peer  ss.SubscriberId
	dcmsg *webrtc.DataChannel
}

// announceNote records a peer's file-transfer announcement, keyed by
// canonical file id. The channel it arrived on guards the record against
// a stale invocation's late announcement.
type announceNote struct {
	peer         ss.SubscriberId
	dcmsg        *webrtc.DataChannel
	fileId       string
	announcement *FileAnnouncement
}

// messagingNote asks the hub for a peer's current messaging channel and
// the announcement of a file — everything a chunk's or an attachment's
// dispatch needs.
type messagingNote struct {
	peer   ss.SubscriberId
	fileId string
	reply  chan messagingReply
}

// messagingReply answers a messagingNote; a nil dcmsg means the peer has
// no live session whose messaging channel came up.
type messagingReply struct {
	dcmsg        *webrtc.DataChannel
	announcement *FileAnnouncement
}

func (peerUpNote) isServerNote()    {}
func (peerDownNote) isServerNote()  {}
func (announceNote) isServerNote()  {}
func (messagingNote) isServerNote() {}

// NewServer constructs a Server and registers it for the well-known
// messaging and binary data-channel labels on client; every distilled
// bot message goes to handler. It panics when a label is already taken,
// mirroring the client's HandleDataChannel. The Server's hub goroutine
// starts here.
func NewServer(client *rtc.HeadlessRTCClient, handler BotMessageHandler, config Configuration) *Server {
	if client == nil {
		panic("msg_handler: nil client")
	}
	if handler == nil {
		panic("msg_handler: nil BotMessageHandler")
	}
	logger := config.Logger
	if logger == nil {
		logger = slog.Default()
	}
	s := &Server{
		client:      client,
		handler:     handler,
		logger:      logger,
		serviceChan: make(chan serverNote),
	}
	go s.hub()
	client.HandleDataChannel(DataChannelLabelMessages, rtc.DataChannelHandlerFunc(s.serveMessages))
	client.HandleDataChannel(DataChannelLabelBinary, rtc.DataChannelHandlerFunc(s.serveBinary))
	return s
}

// hub is the Server's single stateful goroutine: it owns the peer
// registry — every handler goroutine reaches it through serviceChan
// notes.
func (s *Server) hub() {
	peers := make(map[ss.SubscriberId]*peerRecord)
	for note := range s.serviceChan {
		switch n := note.(type) {
		case peerUpNote:
			peers[n.peer] = &peerRecord{dcmsg: n.dcmsg, announced: make(map[string]*FileAnnouncement)}
		case peerDownNote:
			if entry, ok := peers[n.peer]; ok && entry.dcmsg == n.dcmsg {
				delete(peers, n.peer)
			}
		case announceNote:
			if entry, ok := peers[n.peer]; ok && entry.dcmsg == n.dcmsg {
				entry.announced[n.fileId] = n.announcement
			}
		case messagingNote:
			var reply messagingReply
			if entry, ok := peers[n.peer]; ok {
				reply = messagingReply{dcmsg: entry.dcmsg, announcement: entry.announced[n.fileId]}
			}
			n.reply <- reply
		}
	}
}

// messagingChannel asks the hub for the peer's current messaging channel
// and the announcement of fileId (nil when none was seen) — the channel a
// chunk's or an attachment's ResponseWriter answers on, and the message
// it threads on.
func (s *Server) messagingChannel(peer ss.SubscriberId, fileId string) (*webrtc.DataChannel, *FileAnnouncement) {
	reply := make(chan messagingReply, 1)
	s.serviceChan <- messagingNote{peer: peer, fileId: fileId, reply: reply}
	r := <-reply
	return r.dcmsg, r.announcement
}

// canonicalFileId normalizes a file id the way the wire codec does
// (lowercase, dashed); an id that is not a UUID is used as-is.
func canonicalFileId(fileId string) string {
	if id, err := uuid.Parse(fileId); err == nil {
		return id.String()
	}
	return fileId
}

// envelopeOf lifts a decoded frame's envelope into a Message.
func envelopeOf(msg *dcMsgIn) Message {
	return Message{
		ChannelId: msg.channelId,
		From:      msg.from,
		To:        msg.to,
		MsgId:     msg.msgId,
		InReplyTo: msg.inReplyTo,
		Timestamp: msg.timestamp,
	}
}

// sipMessageOf lifts a decoded frame's SIP body into a SipMessage.
func sipMessageOf(msg *dcMsgIn) *SipMessage {
	sip := &SipMessage{
		Message: envelopeOf(msg),
		CallId:  msg.sip.callId,
		Method:  SipMethod(msg.sip.method),
	}
	if msg.sip.response != nil {
		sip.Method = ""
		sip.Response = &SipResponse{Code: msg.sip.response.Code, Phrase: msg.sip.response.Phrase}
	}
	if sip.Method == SipMethodInvite {
		// X-Media says what the call carries; voice is the default when
		// the wire omits it.
		sip.Media = MediaKind(msg.sip.xMedia)
		if sip.Media == "" {
			sip.Media = MediaVoice
		}
	}
	return sip
}

// serveMessages is the dcmsg handler: decode and validation, the echo
// discipline (bounce every accepted message back with echo set, except
// echoes and the call protocol), the file-transfer announcement
// bookkeeping the binary side correlates with — and the dispatch of the
// distilled messages to the BotMessageHandler.
func (s *Server) serveMessages(ctx context.Context, channelId ss.ChannelId, peer ss.SubscriberId, dc *webrtc.DataChannel) {
	self := s.client.SubscriberId()
	s.serviceChan <- peerUpNote{peer: peer, dcmsg: dc}
	// The session ends with ctx (a glare rebuild replaces the registration
	// first): drop it unless it has already been replaced.
	go func() {
		<-ctx.Done()
		s.serviceChan <- peerDownNote{peer: peer, dcmsg: dc}
	}()
	dc.OnMessage(func(raw webrtc.DataChannelMessage) {
		if !raw.IsString {
			return // dcmsg carries JSON text; anything else is dropped
		}
		msg := decodeDCMsg(raw.Data)
		// The data channel is bound to this pair by construction; a
		// message claiming otherwise is dropped.
		if msg == nil || msg.channelId != channelId {
			return
		}
		if msg.echo {
			// One of our own messages bounced back: never echoed again.
			// The Server keeps no message store, so there is nothing to
			// apply.
			return
		}
		if msg.from != peer {
			return
		}
		if msg.mimeType == dcMsgMimeSip {
			// The call protocol is never bounced; the dialog message goes
			// straight to the handler.
			s.handler.HandleCalling(ctx, sipMessageOf(msg), &responseWriter{
				server:    s,
				dc:        dc,
				channelId: channelId,
				self:      self,
				peer:      peer,
				inReplyTo: msg.msgId,
				callId:    msg.sip.callId,
			})
			return
		}
		// The protocol's echo rule: bounce the message back verbatim, so
		// the sender sees its own message.
		if err := s.sendText(dc, msg.echoFrame()); err != nil {
			s.logger.Warn("msg_handler: echo not sent", "peer", peer, "err", err)
			return // the channel is gone; any answer would fail the same way
		}
		switch msg.mimeType {
		case dcMsgMimePlaintext:
			s.handler.HandleChatMessage(ctx, &ChatMessage{
				Message: envelopeOf(msg),
				Text:    msg.plaintext,
			}, &responseWriter{
				server:    s,
				dc:        dc,
				channelId: channelId,
				self:      self,
				peer:      peer,
				inReplyTo: msg.msgId,
			})
		case dcMsgMimeFileTransferStatus:
			// Record the announcement, so the file's chunks and its
			// attachment can be correlated back to it once the bytes
			// arrive on dcbin (cross-channel ordering is not guaranteed:
			// a chunk that arrives first simply sees no announcement).
			body := msg.fileTransfer
			s.serviceChan <- announceNote{
				peer:   peer,
				dcmsg:  dc,
				fileId: canonicalFileId(body.fileId),
				announcement: &FileAnnouncement{
					Message:        envelopeOf(msg),
					FileId:         canonicalFileId(body.fileId),
					Kind:           AttachmentKind(body.kind),
					Filename:       body.filename,
					MIMEType:       body.mimeType,
					SizeTotalBytes: body.totalBytes,
				},
			}
		}
	})
}

// serveBinary is the dcbin handler: reassembly and acknowledgement of
// inbound file transfers, with each accepted chunk and each completed
// file dispatched to the BotMessageHandler.
func (s *Server) serveBinary(ctx context.Context, channelId ss.ChannelId, peer ss.SubscriberId, dc *webrtc.DataChannel) {
	sess := &binarySession{
		server:    s,
		dc:        dc,
		channelId: channelId,
		peer:      peer,
		self:      s.client.SubscriberId(),
		transfers: make(map[uuid.UUID]*inboundTransfer),
	}
	dc.OnMessage(func(raw webrtc.DataChannelMessage) {
		if raw.IsString {
			return // dcbin carries binary frames; anything else is dropped
		}
		frame := decodeBinaryFrame(raw.Data)
		file, ok := frame.(*fileFrame)
		if !ok {
			// A FACK advances a send; the Server sends no files, so there
			// is nothing to advance.
			return
		}
		sess.accept(ctx, file)
	})
}

// binarySession is one session's dcbin state: the reassembly of every
// in-flight inbound transfer, keyed by file id (files multiplex over the
// channel). It lives exactly as long as its session: the partial transfers
// of a torn-down session die with it — mirroring the frontend's reset
// handling.
type binarySession struct {
	server    *Server
	dc        *webrtc.DataChannel
	channelId ss.ChannelId
	peer      ss.SubscriberId
	self      ss.SubscriberId
	transfers map[uuid.UUID]*inboundTransfer
}

// inboundTransfer is the reassembly state of one incoming file. SCTP's
// ordered, reliable delivery means the next frame must continue the
// contiguous prefix exactly.
type inboundTransfer struct {
	total    uint64 // the whole file's size on the wire
	received uint64 // contiguously received bytes (the next expected offset)
	nextSeq  uint32 // the next expected seq
	data     []byte // the reassembled prefix — the attachment in the making
}

// accept folds one FILE frame into its stream's reassembly: the frame must
// continue the contiguous prefix exactly — anything else marks the stream
// corrupt and the transfer is dropped. Every accepted frame is dispatched
// to the handler as a FileChunk and acknowledged; a completing frame's
// reassembled file follows as an Attachment.
func (sess *binarySession) accept(ctx context.Context, f *fileFrame) {
	t, ok := sess.transfers[f.fileId]
	if !ok {
		// The first frame of a stream must start it: seq 0, offset 0.
		if f.seq != 0 || f.offset != 0 {
			sess.server.logger.Warn("msg_handler: first frame of a file does not start the stream; frame dropped",
				"peer", sess.peer, "fileId", f.fileId)
			return
		}
		t = &inboundTransfer{total: f.total}
		sess.transfers[f.fileId] = t
	}
	if f.total != t.total ||
		f.seq != t.nextSeq ||
		f.offset != t.received ||
		f.offset+uint64(len(f.payload)) > f.total {
		delete(sess.transfers, f.fileId)
		sess.server.logger.Warn("msg_handler: corrupt frame stream; transfer dropped",
			"peer", sess.peer, "fileId", f.fileId)
		return
	}
	t.data = append(t.data, f.payload...)
	t.received += uint64(len(f.payload))
	t.nextSeq++
	last := t.received == t.total
	fileId := canonicalFileId(f.fileId.String())
	// The messaging channel and the transfer's announcement, fresh from
	// the hub per chunk: a glare rebuild swaps the channel mid-session,
	// and a late announcement is picked up whenever it arrives.
	dcmsg, announcement := sess.server.messagingChannel(sess.peer, fileId)
	w := &responseWriter{
		server:    sess.server,
		dc:        dcmsg,
		channelId: sess.channelId,
		self:      sess.self,
		peer:      sess.peer,
	}
	if announcement != nil {
		w.inReplyTo = announcement.MsgId
	}
	sess.server.handler.HandleFileChunk(ctx, announcement, &FileChunk{
		ChannelId: sess.channelId,
		Peer:      sess.peer,
		FileId:    fileId,
		Seq:       f.seq,
		Offset:    f.offset,
		Total:     f.total,
		Payload:   f.payload,
		Last:      last,
	}, w)
	// Every accepted frame is acknowledged; a failed acknowledgement only
	// stalls the sender's window, so it is logged and swallowed, mirroring
	// the browser.
	ack := ackFrame{fileId: f.fileId, ackSeq: t.nextSeq, ackedBytes: t.received}
	if err := sess.dc.Send(ack.encode()); err != nil {
		sess.server.logger.Warn("msg_handler: acknowledgement not sent", "peer", sess.peer, "err", err)
	}
	if last {
		delete(sess.transfers, f.fileId)
		attachment := &Attachment{
			ChannelId: sess.channelId,
			Peer:      sess.peer,
			FileId:    fileId,
			Data:      t.data,
		}
		if announcement != nil {
			attachment.Kind = announcement.Kind
			attachment.Filename = announcement.Filename
			attachment.MIMEType = announcement.MIMEType
		}
		sess.server.handler.HandleAttachment(ctx, announcement, attachment, w)
	}
}

// sendText sends one encoded DCMsg on the messaging channel.
func (s *Server) sendText(dc *webrtc.DataChannel, data []byte) error {
	if data == nil {
		return errEncode
	}
	return dc.SendText(string(data))
}
