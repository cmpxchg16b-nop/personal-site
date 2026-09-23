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
// The sip-leg (toward the SIP network) is a diago
// (github.com/emiago/diago on sipgo) transaction user per REGISTERED
// chat user — each /register opens a dedicated SIP client: its own UA,
// named for the user's SIP username, and its own UDP socket. Every
// registration and every outbound call is a SIP transaction on the
// account's own client, stamped with that user's identity alone — the
// From is the user's AOR, the Contact's user part is the username, the
// digest credential is the user's own: the user brings their own SIP
// credential with /register <user@host> <password> (kept in the
// injected UserSessionStorage), phones a callee with /call <user@host>,
// and ends the call with /hangup or the browser's hangup button. The
// client dies with the account; the bot's own identity never appears on
// the wire.
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
	"fmt"
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

	// The sip-leg's transport: the template every account's SIP client
	// is opened from. Transport is the SIP network transport ("udp",
	// "tcp"); empty selects "udp". BindHost/BindPort say where each
	// account's socket listens; zero values select all interfaces and an
	// ephemeral port per account — a fixed BindPort admits one
	// registered user at a time. An empty bind host MUST NOT reach
	// diago's transport as-is: it never resolves an advertised address
	// then, and the Contact goes out host-less (a registrar answers
	// "400 Bad Contact Header"). ExternalHost, when set, is the address
	// the bot advertises in its SIP Contact and SDP — for hosts where
	// the bind address is not the address the SIP network should dial
	// back (the simple NAT case).
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
// NewOnMemoryUserSessionStorage()); the SIP clients the registrations
// and calls run on are opened per account (see sipStack). It panics
// when a label is already taken, mirroring the client's
// HandleDataChannel. The bot needs no further driving.
func New(client *rtc.HeadlessRTCClient, storage UserSessionStorage, config Configuration) {
	logger := config.Logger
	if logger == nil {
		logger = slog.Default()
	}
	transport := config.Transport
	if transport == "" {
		transport = "udp"
	}
	// An empty bind host means all interfaces; diago resolves the
	// advertised address from the routing table then (see the
	// Configuration doc).
	bindHost := config.BindHost
	if bindHost == "" {
		bindHost = "0.0.0.0"
	}
	expiry := config.RegisterExpiry
	if expiry == 0 {
		expiry = 300
	}

	stack := sipStack{
		logger:       logger,
		transport:    transport,
		bindHost:     bindHost,
		bindPort:     config.BindPort,
		externalHost: config.ExternalHost,
	}
	msg_handler.NewServer(client, newSipHandler(logger, storage, stack, time.Duration(expiry)*time.Second, config.TestSIPContact), msg_handler.Configuration{Logger: logger})
}

// sipStack is the per-account SIP client factory: the bot's sip-leg
// configuration, materialized by open into one diago per registered
// user. The accounts share the configuration, never a client — a shared
// client would mix every user's credentials and identity onto one
// socket's Contact.
type sipStack struct {
	logger       *slog.Logger
	transport    string
	bindHost     string
	bindPort     int
	externalHost string
}

// open materializes one user's SIP client: a diago whose UA is named
// for the user's SIP username — the Contact's user part and the
// INVITE's callback address then carry the user's name, never the
// bot's — with a transport socket of the account's own. The client is
// already serving when open returns (responses to its transactions
// arrive on its socket; inbound INVITEs — someone calling a registered
// AOR — have no routing policy in this outbound-only SBC and are
// declined). The client dies with ctx: the account stops it once the
// de-REGISTER left.
func (s sipStack) open(ctx context.Context, username string) (*diago.Diago, error) {
	ua, err := sipgo.NewUA(sipgo.WithUserAgent(username))
	if err != nil {
		return nil, err
	}
	dg := diago.NewDiago(ua,
		diago.WithTransport(diago.Transport{
			Transport:    s.transport,
			BindHost:     s.bindHost,
			BindPort:     s.bindPort,
			ExternalHost: s.externalHost,
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
	if err := dg.ServeBackground(ctx, func(inDialog *diago.DialogServerSession) {
		s.logger.Info("sipbot: declining an inbound SIP call",
			"from", inDialog.FromUser(), "to", inDialog.ToUser())
		if err := inDialog.Respond(603, "Decline", nil); err != nil {
			s.logger.Warn("sipbot: inbound-call decline not sent", "err", err)
		}
	}); err != nil {
		return nil, fmt.Errorf("the SIP client failed to start: %w", err)
	}
	return dg, nil
}
