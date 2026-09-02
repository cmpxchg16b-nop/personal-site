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
// The songbook is injected — the Configuration's AudioSources
// (pkg/models/audiosource's AudioSourceData, authored as the
// configuration document's audioSource entries): sample data in the
// declared format, inline or at a url, optionally flac compressed,
// loaded and decoded lazily as a stream when a call first plays the
// song, and looped by rewinding the stream for as long as the call
// lasts. Two format combinations are accepted, and the track's codec
// follows the song: a μ-law source (8000 Hz, mono) rides the pair's
// peer connection as a PCMU track, its bytes becoming the RTP payload
// as they are; a linear PCM source (48000 Hz, stereo) rides an opus
// track (48000 Hz, stereo — among the audio codecs pion's default
// media engine registers and every browser offers), encoded frame by
// frame from the source's samples (see player.go and convert.go). A
// mid-call song switch within a codec family is a stream swap; across
// families the whole track is replaced on the wire.
package musicbot

import (
	"log/slog"

	"personal-site/pkg/models/audiosource"
	"personal-site/pkg/rtc"
	"personal-site/pkg/rtc/msg_handler"
)

// Configuration configures the music bot.
type Configuration struct {
	// Logger receives the bot's diagnostics; nil selects slog's default
	// logger.
	Logger *slog.Logger

	// AudioSources is the bot's songbook, keyed by each source's Name
	// (the CLI's song names). The sources are validated at wiring — an
	// invalid source panics, a wiring-time error — and their sample
	// data is loaded lazily, when a call first plays them.
	AudioSources []*audiosource.AudioSourceData
}

// New wires the music bot onto client: a msg_handler.Server serving the
// client's two well-known data channels with the music-purpose
// BotMessageHandler as their message policy. It panics when a label is
// already taken, mirroring the client's HandleDataChannel, and when an
// audio source is invalid or two share a name. The bot needs no
// further driving.
func New(client *rtc.HeadlessRTCClient, config Configuration) {
	logger := config.Logger
	if logger == nil {
		logger = slog.Default()
	}
	for _, s := range config.AudioSources {
		if err := s.Validate(); err != nil {
			panic(err)
		}
	}
	msg_handler.NewServer(client, newMusicHandler(logger, config.AudioSources), msg_handler.Configuration{Logger: logger})
}
