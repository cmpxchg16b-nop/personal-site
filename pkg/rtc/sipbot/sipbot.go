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
// injected UserSessionStorage) — or is loaned an account from the
// injected SIPCredentialPool — phones a callee with /call <user@host>,
// learns their callable address with /my-number, and ends the call with
// /hangup or the browser's hangup button. The same client is the UAS of
// the reverse direction: a SIP subscriber's INVITE to the registered
// AOR rings the user's browser, and the user's accept joins the legs
// (inbound.go). The client dies with the account; the bot's own
// identity never appears on the wire.
//
// The bot terminates both signalling planes and both media planes and
// relays each into the other: dialog events cross as the handler's
// Invite/Cancel/Bye on one side and diago's Invite/Hangup on the other,
// and media crosses as the call's relay — the codec toward the browser
// is always opus, the codec toward the SIP network is negotiated (opus
// preferred, then PCMU, PCMA), and when both legs settled on opus the
// relay is a pure passthrough: the codec's own packets cross the SBC
// byte for byte, with transcoding (G.711 ↔ PCM ↔ opus) only when the
// two legs differ (see transcode.go). DTMF crosses too, as RFC 4733
// telephone-event, WebRTC-leg → SIP-leg: the browser user's key presses
// reach the SIP network; the reverse direction is deliberately not
// transported (see dtmf.go).
package sipbot

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"

	"github.com/emiago/diago"
	"github.com/emiago/diago/media"
	"github.com/emiago/sipgo"

	"personal-site/pkg/rtc"
	"personal-site/pkg/rtc/msg_handler"
)

// codecAudioOpusStereo is the sip-leg's opus: diago's constant annotated
// with the RFC 7587 fmtp parameters announcing in-band FEC and the stereo
// receive preference (stereo=1) — the webrtc-leg's stereo opus
// (stereoOpusPCFactory) mirrored onto the SIP side. It rides both the
// outbound INVITE's offer and the inbound call's answer (diago's
// negotiation keeps the local codecs, Fmtp included — see
// third_party/diago/PATCHES.md), so when the far end settles on opus the
// relay is a stereo passthrough end to end; a mono opus peer is unaffected
// (stereo is a preference, never a requirement).
var codecAudioOpusStereo = func() media.Codec {
	c := media.CodecAudioOpus
	c.Fmtp = "useinbandfec=1;stereo=1"
	return c
}()

// Configuration configures the sip bot.
type Configuration struct {
	// Logger receives the bot's diagnostics; nil selects slog's default
	// logger.
	Logger *slog.Logger

	// The sip-leg's transport: the template every account's SIP client
	// is opened from. Transport is the SIP network transport ("udp",
	// "tcp"); empty selects "udp". BindHost/BindPort say where each
	// account's socket listens. An empty BindHost — the default — selects
	// the per-account source address of the route to the account's
	// registrar, so the Contact and SDP advertise the address the
	// registrar already sees the bot's packets come from, in the
	// registrar's address family — an IPv4 socket cannot write to an
	// IPv6 registrar. An explicit BindHost is used verbatim for every
	// account, its address family then constraining which registrars are
	// reachable. A zero BindPort selects an ephemeral port per account —
	// a fixed one admits one registered user at a time. ExternalHost,
	// when set, is the address the bot advertises in its SIP Contact and
	// SDP — for hosts where the bind address is not the address the SIP
	// network should dial back (the simple NAT case); being one value,
	// it fits deployments whose accounts share one address family.
	Transport    string
	BindHost     string
	BindPort     int
	ExternalHost string

	// IPPreference selects the address family of every DNS hostname
	// resolution in the sip leg — the registrar's and the callees':
	// IPPreferenceV4Only resolves A records only, IPPreferenceV6Only AAAA
	// records only (a hostname without one fails its registration with
	// the DNS error), and IPPreferenceDefault (the zero value) keeps
	// sipgo's own behavior (IPv4 preferred, IPv6 used when no A record
	// exists). The account sockets' bind-address selection (an empty
	// BindHost) follows the same family. IP literals are not resolutions
	// and pass unaffected. A non-default value resolves through the bot's
	// own filtering DNS proxy, so it requires UpstreamDNSResolver; see
	// dns.go.
	IPPreference IPPreference

	// UpstreamDNSResolver is the upstream DNS resolver (host[:port], the
	// port defaulting to 53) the bot's filtering DNS proxy relays to —
	// required with a non-default IPPreference, rejected with the default
	// (which uses the system resolver untouched). Validate the pairing
	// with ParseIPPreference; New panics on an invalid one.
	UpstreamDNSResolver string

	// TestSIPContact is the SIP address the CLI's /test-call command
	// dials (e.g. "9664@192.168.1.2") — a known-good callee on the SIP
	// network the deployment tests against. Empty disables /test-call.
	TestSIPContact string

	// YellowPage is the bot's phone book: the deployment's example
	// callable numbers (the <yellowPage/> element of serverConfig.xml),
	// listed by the CLI's /yellow-page command grouped by section. Empty
	// answers that the page is empty.
	YellowPage []YellowPageSection
}

// New wires the sip bot onto client: a msg_handler.Server serving the
// client's two well-known data channels with the sip-purpose
// BotMessageHandler as their message policy. storage is the bot's
// user-session store (the shipped wiring passes
// NewOnMemoryUserSessionStorage()); pool is the shared SIP credential
// pool the bot loans accounts from (nil: no pool — a /call without a
// registration always answers with the /register hint); the SIP clients
// the registrations and calls run on are opened per account (see
// sipStack). It panics when a label is already taken, mirroring the
// client's HandleDataChannel — and likewise when a non-default
// IPPreference's pairing with UpstreamDNSResolver is invalid (validate
// it with ParseIPPreference) or its filtering DNS proxy cannot start.
// The bot needs no further driving.
func New(client *rtc.HeadlessRTCClient, storage UserSessionStorage, pool *SIPCredentialPool, config Configuration) {
	logger := config.Logger
	if logger == nil {
		logger = slog.Default()
	}
	transport := config.Transport
	if transport == "" {
		transport = "udp"
	}

	stack := sipStack{
		logger:       logger,
		transport:    transport,
		bindHost:     config.BindHost,
		bindPort:     config.BindPort,
		externalHost: config.ExternalHost,
		ipPreference: config.IPPreference.normalize(),
	}
	// A non-default ipPreference needs the bot's filtering DNS proxy
	// (dns.go) on its explicit upstream resolver. The proxy is shared
	// by every account and lives for the bot's lifetime (the process,
	// in the shipped wiring). A bad pairing is a misconfiguration:
	// cmd/server validates with ParseIPPreference at startup, so a
	// panic here means a programmatic caller skipped it.
	if _, err := ParseIPPreference(string(stack.ipPreference), config.UpstreamDNSResolver); err != nil {
		panic(fmt.Sprintf("sipbot: %v", err))
	}
	if stack.ipPreference != IPPreferenceDefault {
		proxy, err := startFilteringResolver(stack.ipPreference, config.UpstreamDNSResolver)
		if err != nil {
			panic(fmt.Sprintf("sipbot: the filtering DNS proxy: %v", err))
		}
		stack.dnsProxy = proxy
	}
	h := newSipHandler(logger, storage, pool, stack, config.TestSIPContact, config.YellowPage)
	// The unsolicited-send path (the inbound call's ring) is the Server's
	// WriterFor — a wiring-time assignment: no handler invocation can
	// precede the client's Run, which the caller starts after New returns.
	h.writers = msg_handler.NewServer(client, h, msg_handler.Configuration{Logger: logger})
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
	// ipPreference selects the address family of every DNS resolution
	// (dns.go); dnsProxy is the bot's filtering DNS reverse proxy — nil
	// under the default preference, where the system resolver is used
	// untouched.
	ipPreference IPPreference
	dnsProxy     *filteringResolver
}

// open materializes one account's SIP client: a diago whose UA is named
// for the account's SIP username — the Contact's user part and the
// INVITE's callback address then carry the user's name, never the
// bot's — with a transport socket of the account's own, bound to the
// route's source address toward the registrar (see bindHostFor — the
// address-family answer too). The client is already serving when open
// returns (responses to its transactions arrive on its socket); an
// inbound INVITE — someone calling the registered AOR — is handed to
// onInbound, which owns the dialog from then on and blocks until it
// ends (diago's serve-handler lifetime discipline). The client dies
// with ctx: the account stops it once the de-REGISTER left.
func (s sipStack) open(ctx context.Context, session UserSession, onInbound func(inDialog *diago.DialogServerSession)) (*diago.Diago, error) {
	uaOpts := []sipgo.UserAgentOption{sipgo.WithUserAgent(session.Username)}
	if s.dnsProxy != nil {
		// A non-default ipPreference resolves every hostname the account's
		// client touches — the registrar's, the callees' — through the
		// bot's filtering DNS proxy (dns.go): the answers come back in the
		// chosen family only.
		uaOpts = append(uaOpts, sipgo.WithUserAgentDNSResolver(s.dnsProxy.resolver))
	}
	ua, err := sipgo.NewUA(uaOpts...)
	if err != nil {
		return nil, err
	}
	dg := diago.NewDiago(ua,
		diago.WithTransport(diago.Transport{
			Transport:    s.transport,
			BindHost:     s.bindHostFor(ctx, session),
			BindPort:     s.bindPort,
			ExternalHost: s.externalHost,
		}),
		// The offer's preference order: opus first — when the far end
		// takes it, the relay is a passthrough; the G.711 twins are the
		// transcoding fallbacks. telephone-event rides along always —
		// the browser user's key presses cross to the SIP network on it
		// (call.go's DTMF relay); inbound answers echo it only when the
		// caller's offer announced it (diago's negotiation intersects).
		diago.WithMediaConfig(diago.MediaConfig{
			Codecs: []media.Codec{
				codecAudioOpusStereo,
				media.CodecAudioUlaw,
				media.CodecAudioAlaw,
				media.CodecTelephoneEvent8000,
			},
		}),
	)
	if err := dg.ServeBackground(ctx, onInbound); err != nil {
		return nil, fmt.Errorf("the SIP client failed to start: %w", err)
	}
	return dg, nil
}

// bindHostFor picks the account socket's bind address: the configured
// BindHost verbatim when one is set (the operator's choice, its address
// family included); otherwise the source address the route to the
// registrar would use — so the account's socket binds, and the Contact
// and SDP advertise, exactly the address the registrar already sees the
// bot's packets come from. A UDP dial sends nothing; it just answers the
// routing question. Under a non-default ipPreference the registrar
// resolves through the bot's filtering DNS proxy (dns.go) — the same
// view of the namespace the wire resolutions get — so the socket's
// family always matches the one the account's resolver picks for the
// wire. This is also what makes IPv6 work: an IPv4 socket cannot write
// to an IPv6 registrar (the kernel's "non-IPv4 address" write error),
// and diago's own interface resolution picks a link-local IPv6 on hosts
// without a global one — the route lookup gets the right family and the
// right address in one step.
func (s sipStack) bindHostFor(ctx context.Context, session UserSession) string {
	if s.bindHost != "" {
		return s.bindHost
	}
	// sipgo's URI parser keeps a bracketed IPv6 literal's brackets in the
	// Host; the net package wants the bare address.
	host := strings.TrimPrefix(strings.TrimSuffix(session.Host, "]"), "[")
	port := session.Port
	if port == 0 {
		port = 5060
	}
	if s.dnsProxy != nil {
		// A non-default ipPreference resolves through the bot's filtering
		// DNS proxy — the same view of the namespace the wire resolutions
		// get, upstream included.
		ips, err := s.dnsProxy.resolver.LookupIP(ctx, "ip", host)
		if err != nil {
			return s.ipPreference.wildcard() // unresolvable — the registration will say why
		}
		// The proxy filters DNS-sourced answers, but a static-source
		// answer (/etc/hosts) escapes it — re-filter so the socket's
		// family stays the preference's.
		for _, ip := range ips {
			if !s.ipPreference.allows(ip) {
				continue
			}
			if conn, err := net.DialUDP("udp", nil, &net.UDPAddr{IP: ip, Port: port}); err == nil {
				source := conn.LocalAddr().(*net.UDPAddr).IP.String()
				_ = conn.Close()
				return source
			}
			return s.ipPreference.wildcard() // resolved but unrouted
		}
		return s.ipPreference.wildcard()
	}
	raddr, err := net.ResolveUDPAddr("udp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil || raddr.IP == nil {
		return "0.0.0.0" // unresolvable — the registration will say why
	}
	if conn, err := net.DialUDP("udp", nil, raddr); err == nil {
		source := conn.LocalAddr().(*net.UDPAddr).IP.String()
		_ = conn.Close()
		return source
	}
	// No route (yet): at least match the family, so a resolvable but
	// unrouted IPv6 registrar fails with a routing error rather than the
	// misleading "non-IPv4 address".
	if raddr.IP.To4() == nil {
		return "::"
	}
	return "0.0.0.0"
}
