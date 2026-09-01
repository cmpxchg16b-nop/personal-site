// Package musicbot implements the music bot of the site's data-channel
// protocols — a pkg/rtc/msg_handler.BotMessageHandler of the music kind,
// on the three-layer bot stack:
//
//   - pkg/rtc's HeadlessRTCClient — the bot client: signalling, peer
//     connections, sessions, data-channel dispatch by label, and the
//     media plumbing (AddTrack/RemoveTrack/HandleTrack).
//   - pkg/rtc/msg_handler's Server — the data-channel layer: it serves
//     the two well-known labels and distills their raw frames into bot
//     messages, owning the protocol mechanics (the echo discipline, the
//     transfer reassembly, the call log's status amends).
//   - this package's musicHandler — the message policy.
//
// The policy, such as it is: text chat is the bot's CLI — /help shows the
// commands, /list-songs the songbook, /play <song> phones the user (a
// voice INVITE) and plays the song, or switches it mid-call. An incoming
// voice call is accepted and answered with music; a video call is
// declined on the spot. An attachment of any kind is refused with a chat
// reply (the bytes themselves flow through the Server's transfer
// mechanics and are discarded).
//
// The music is programmatically generated (see song.go) and encoded
// G.711 μ-law (PCMU) — the WebRTC audio codec every browser offers and a
// pure-Go encoder can produce — and rides the pair's existing peer
// connection as one local media track per call, fed frame by frame by
// the call's player (see player.go).
package musicbot

import (
	"log/slog"

	"personal-site/pkg/rtc"
	"personal-site/pkg/rtc/msg_handler"
)

// Configuration configures the music bot.
type Configuration struct {
	// Logger receives the bot's diagnostics; nil selects slog's default
	// logger.
	Logger *slog.Logger
}

// New wires the music bot onto client: a msg_handler.Server serving the
// client's two well-known data channels with the music-purpose
// BotMessageHandler as their message policy. It panics when a label is
// already taken, mirroring the client's HandleDataChannel. The bot needs
// no further driving.
func New(client *rtc.HeadlessRTCClient, config Configuration) {
	logger := config.Logger
	if logger == nil {
		logger = slog.Default()
	}
	msg_handler.NewServer(client, newMusicHandler(logger), msg_handler.Configuration{Logger: logger})
}
