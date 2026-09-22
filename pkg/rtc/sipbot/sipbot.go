// Package sipbot implements the sip bot of the site's data-channel
// protocols — a session border controller between the chat subsystem's
// WebRTC phone sessions and an external SIP voice network, in SIP terms
// a back-to-back user agent (B2BUA), and the third message policy on the
// three-layer bot stack:
//
//   - pkg/rtc's HeadlessRTCClient — the bot client: signalling, peer
//     connections, sessions, data-channel dispatch by label, and the
//     media plumbing (AddTrack/RemoveTrack/HandleTrack).
//   - pkg/rtc/msg_handler's Server — the data-channel layer: it serves
//     the two well-known labels and distills their raw frames into bot
//     messages, owning the protocol mechanics (the echo discipline, the
//     transfer reassembly, the call log's status amends).
//   - this package's sipHandler — the message policy.
//
// The webrtc-leg (toward the browser) is the music bot's: a call is an
// in-band SIP-subset dialog over the pair's dcmsg channel plus an opus
// audio m-line renegotiated onto the pair's existing peer connection.
// The sip-leg (toward the SIP network) is one shared diago
// (github.com/emiago/diago on sipgo) transaction user per bot process —
// one UDP transport on which every chat user's registration and every
// outbound call is a separate SIP transaction stamped with that user's
// own identity: the user brings their own SIP credential with
// /register <user@host> <password> (kept in the injected
// UserSessionStorage), phones a callee with /call <user@host>, and ends
// the call with /hangup or the browser's hangup button.
//
// The bot terminates both signalling planes and both media planes and
// relays each into the other: dialog events cross as the handler's
// Invite/Cancel/Bye on one side and diago's Invite/Hangup on the other,
// and media crosses as the call's relay — the codec toward the browser
// is always opus, the codec toward the SIP network is negotiated (opus
// preferred, then PCMU, PCMA), and when both legs settled on opus the
// relay is a pure passthrough: the codec's own packets cross the SBC
// byte for byte, with transcoding (G.711 ↔ PCM ↔ opus) only when the
// two legs differ (see transcode.go).
package sipbot

import (
	"context"
	"log/slog"
	"time"

	"github.com/emiago/diago"
	"github.com/emiago/diago/media"
	"github.com/emiago/sipgo"

	"personal-site/pkg/rtc"
	"personal-site/pkg/rtc/msg_handler"
)

// Configuration configures the sip bot.
type Configuration struct {
	// Logger receives the bot's diagnostics; nil selects slog's default
	// logger.
	Logger *slog.Logger

	// The sip-leg's transport: one shared SIP socket for the whole bot.
	// Transport is the SIP network transport ("udp", "tcp"); empty
	// selects "udp". BindHost/BindPort say where the socket listens;
	// zero values select all interfaces and an ephemeral port.
	// ExternalHost, when set, is the address the bot advertises in its
	// SIP Contact and SDP — for hosts where the bind address is not the
	// address the SIP network should dial back (the simple NAT case).
	Transport    string
	BindHost     string
	BindPort     int
	ExternalHost string

	// RegisterExpiry is the registration's Expires; zero selects 300 s.
	RegisterExpiry int

	// TestSIPContact is the SIP address the CLI's /test-call command
	// dials (e.g. "9664@192.168.1.2") — a known-good callee on the SIP
	// network the deployment tests against. Empty disables /test-call.
	TestSIPContact string
}

// New wires the sip bot onto client: a msg_handler.Server serving the
// client's two well-known data channels with the sip-purpose
// BotMessageHandler as their message policy. storage is the bot's
// user-session store (the shipped wiring passes
// NewOnMemoryUserSessionStorage()); the SIP stack it dials out on lives
// for the process. It panics when a label is already taken, mirroring
// the client's HandleDataChannel, and when the SIP stack cannot be
// created. The bot needs no further driving.
func New(client *rtc.HeadlessRTCClient, storage UserSessionStorage, config Configuration) {
	logger := config.Logger
	if logger == nil {
		logger = slog.Default()
	}
	transport := config.Transport
	if transport == "" {
		transport = "udp"
	}
	expiry := config.RegisterExpiry
	if expiry == 0 {
		expiry = 300
	}

	ua, err := sipgo.NewUA(sipgo.WithUserAgent("sipbot"))
	if err != nil {
		panic(err)
	}
	dg := diago.NewDiago(ua,
		diago.WithTransport(diago.Transport{
			Transport:    transport,
			BindHost:     config.BindHost,
			BindPort:     config.BindPort,
			ExternalHost: config.ExternalHost,
		}),
		// The offer's preference order: opus first — when the far end
		// takes it, the relay is a passthrough; the G.711 twins are the
		// transcoding fallbacks. telephone-event rides along for the
		// PBXs that insist on negotiating it; the relay drops it.
		diago.WithMediaConfig(diago.MediaConfig{
			Codecs: []media.Codec{
				media.CodecAudioOpus,
				media.CodecAudioUlaw,
				media.CodecAudioAlaw,
				media.CodecTelephoneEvent8000,
			},
		}),
	)

	msg_handler.NewServer(client, newSipHandler(logger, storage, dg, time.Duration(expiry)*time.Second, config.TestSIPContact), msg_handler.Configuration{Logger: logger})

	// The SIP stack listens for the process's lifetime: client
	// transactions (REGISTER, INVITE) receive their responses through it,
	// and in-dialog requests (the callee's BYE, re-INVITEs) are served by
	// diago's own handlers. An inbound INVITE — someone calling one of
	// the registered AORs — has no routing policy in this outbound-only
	// SBC, so it is declined.
	go func() {
		err := dg.ServeBackground(context.Background(), func(inDialog *diago.DialogServerSession) {
			logger.Info("sipbot: declining an inbound SIP call",
				"from", inDialog.FromUser(), "to", inDialog.ToUser())
			if err := inDialog.Respond(603, "Decline", nil); err != nil {
				logger.Warn("sipbot: inbound-call decline not sent", "err", err)
			}
		})
		if err != nil {
			logger.Error("sipbot: the SIP stack stopped", "err", err)
		}
	}()
}
