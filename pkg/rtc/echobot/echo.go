package echobot

// This file is the echo bot's message policy — a
// msg_handler.BotMessageHandler. Everything protocol-shaped (the echo
// discipline, validation, reassembly, acknowledgement) lives a layer
// down, in msg_handler's Server; what lives here is only what the
// messages MEAN to an echo bot. The handler is a plain struct delegating
// to plain helpers, like any bot framework's handler: the file-hash
// feature keeps its own state (hashreport.go); there is nothing to
// synchronize here.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"

	"personal-site/pkg/rtc/msg_handler"
)

// echoHandler is the echo-purpose msg_handler.BotMessageHandler.
type echoHandler struct {
	logger *slog.Logger
	hashes *hashReporter
}

var _ msg_handler.BotMessageHandler = (*echoHandler)(nil)

func newEchoHandler(logger *slog.Logger) *echoHandler {
	return &echoHandler{
		logger: logger,
		hashes: &hashReporter{logger: logger},
	}
}

// HandleChatMessage answers a chat line with an identical one — the echo
// purpose.
func (h *echoHandler) HandleChatMessage(_ context.Context, msg *msg_handler.ChatMessage, w msg_handler.ResponseWriter) {
	if _, err := w.Reply(msg.Text); err != nil {
		h.logger.Warn("echobot: reply not sent", "peer", msg.From, "err", err)
	}
}

// HandleFileChunk folds the chunk into its transfer's running hash and
// reports it — the hash feature is the hashReporter's; see hashreport.go.
// The file's bytes themselves are never retained: the Server owns
// reassembly; only the running hash accumulates.
func (h *echoHandler) HandleFileChunk(_ context.Context, _ *msg_handler.FileAnnouncement, chunk *msg_handler.FileChunk, w msg_handler.ResponseWriter) {
	h.hashes.chunk(chunk, w)
}

// HandleAttachment logs the finished transfer. The bot retains nothing
// per file: the final hash was already reported by the last chunk's
// HandleFileChunk.
func (h *echoHandler) HandleAttachment(_ context.Context, _ *msg_handler.FileAnnouncement, attachment *msg_handler.Attachment, _ msg_handler.ResponseWriter) {
	sum := sha256.Sum256(attachment.Data)
	h.logger.Info("echobot: file received",
		"peer", attachment.Peer, "fileId", attachment.FileId,
		"size", len(attachment.Data), "sha256", hex.EncodeToString(sum[:]))
}

// HandleCalling answers an incoming call: a call is no use to a bot, so
// an INVITE — voice or video — is declined. The rest of the dialog (the
// caller's CANCEL, a BYE, the responses' folds) is the caller's business.
func (h *echoHandler) HandleCalling(_ context.Context, sip *msg_handler.SipMessage, w msg_handler.ResponseWriter) {
	if sip.Method != msg_handler.SipMethodInvite {
		return
	}
	h.logger.Info("echobot: declining call",
		"peer", sip.From, "callId", sip.CallId, "media", sip.Media)
	if err := w.Reject(msg_handler.SipCodeDecline, msg_handler.SipPhraseDecline); err != nil {
		h.logger.Warn("echobot: call decline not sent", "peer", sip.From, "err", err)
	}
}
