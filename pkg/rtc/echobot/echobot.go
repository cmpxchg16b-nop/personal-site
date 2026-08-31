// Package echobot implements the reference bot of the site's data-channel
// protocols — the Go counterpart of the browser application's
// useDataChannel + useBinaryDataChannel (web/site/src/api/ss/
// datachannel.tsx, binarydatachannel.tsx), specialized to "bounce
// everything back and prove it arrived". A bot is three layers:
//
//   - pkg/rtc's HeadlessRTCClient — the bot client: signalling, peer
//     connections, sessions, and data-channel dispatch by label.
//   - pkg/rtc/msg_handler's Server — the data-channel layer: it serves
//     the two well-known labels and distills their raw frames into bot
//     messages, owning the protocol mechanics (the echo discipline,
//     validation, reassembly, acknowledgement).
//   - this package's echoHandler — the message policy, a
//     msg_handler.BotMessageHandler.
//
// The policy, such as it is: a plain-text chat line gets a reply with
// identical content; an incoming call — voice or video — is declined with
// 603 Decline; and an inbound file transfer's running sha256 is reported
// over the session's messaging channel — a "sha256:<hex>" chat message
// opened by the first accepted chunk (referring back to the transfer's
// announcement when one was seen), amended via chat control every
// hashReportChunkInterval chunks with the final chunk always amending, so
// the report ends on the file's complete digest. The file's bytes are
// never retained: the running hash is the deliverable, complete the
// moment the last chunk lands.
package echobot

import (
	"log/slog"

	"personal-site/pkg/rtc"
	"personal-site/pkg/rtc/msg_handler"
)

const (
	// DataChannelLabelMessages is the well-known label of the messaging
	// data channel every pair of peers brings up ("dcmsg"); the JSON
	// DCMsg protocol rides it.
	DataChannelLabelMessages = msg_handler.DataChannelLabelMessages

	// DataChannelLabelBinary is the well-known label of the binary data
	// channel brought up alongside the messaging one ("dcbin"); the
	// compact binary file-transfer frames ride it.
	DataChannelLabelBinary = msg_handler.DataChannelLabelBinary
)

// Configuration configures the echo bot.
type Configuration struct {
	// Logger receives the bot's diagnostics; nil selects slog's default
	// logger.
	Logger *slog.Logger
}

// New wires the echo bot onto client: a msg_handler.Server serving the
// client's two well-known data channels with the echo-purpose
// BotMessageHandler as their message policy. It panics when a label is
// already taken, mirroring the client's HandleDataChannel. The bot needs
// no further driving.
func New(client *rtc.HeadlessRTCClient, config Configuration) {
	logger := config.Logger
	if logger == nil {
		logger = slog.Default()
	}
	msg_handler.NewServer(client, newEchoHandler(logger), msg_handler.Configuration{Logger: logger})
}
