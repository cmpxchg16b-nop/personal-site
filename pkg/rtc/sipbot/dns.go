package sipbot

// This file is the ipPreference machinery: the address-family selection
// for every DNS hostname resolution in the sip leg.
//
// Two places in the bot resolve a hostname to an IP: bindHostFor, which
// resolves the registrar to pick the account socket's bind address, and
// sipgo's transport layer, which resolves the request-URI host of every
// outbound SIP message (the REGISTERs, the INVITEs, their BYEs, and the
// SRV fallback). sipgo resolves through its UA's dnsResolver — a
// *net.Resolver, replaceable via sipgo.WithUserAgentDNSResolver; the
// library's own family-preference knob is unexported as of v1.6.0 — so a
// non-default preference installs a custom resolver on every account's
// UA, and bindHostFor resolves through the same one.
//
// The custom resolver is the pure-Go resolver (PreferGo) pointed at the
// bot's one filtering DNS proxy: a reverse proxy on an ephemeral
// loopback address (127.0.0.1:0, shared by every account — see
// filteringResolver) that relays queries to the configured upstream
// resolver (miekg/dns's ExchangeContext) and answers the suppressed
// family's qtype itself — AAAA under v4Only, A under v6Only — with an
// empty NOERROR (NODATA — never NXDOMAIN, which could poison the allowed
// family's parallel lookup). Every other query — the allowed family,
// SRV, anything else — crosses to the upstream and back untouched, so
// TTLs, CNAME chains, and NXDOMAINs keep their real meaning, and sipgo's
// SRV fallback keeps working (the SRV target's chase re-enters the
// filter as ordinary A/AAAA queries). The proxy speaks UDP and TCP on
// one port, so a truncated upstream answer's TCP retry works too.
//
// The SIP messages on the wire are untouched: the hostnames stay
// hostnames in every URI and header; only the routing changes. Two
// deliberate edges, both the pure-Go resolver's own short-circuits
// (design-doc caveat 20): IP literals never reach the proxy (a literal
// is not a resolution — a wrong-family literal registrar fails at send),
// and names answered from /etc/hosts never reach it either (a
// dual-family hosts entry escapes the filter, and sipgo's own
// prefer-IPv4 then applies; bindHostFor re-filters its own answers, so
// at least the socket's family stays the preference's).

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/miekg/dns"
)

// IPPreference is the <sipBot/> element's ipPreference attribute: the
// address family every DNS resolution in the sip leg honors. The zero
// value is the default.
type IPPreference string

const (
	// IPPreferenceDefault keeps sipgo's own resolution behavior: IPv4
	// preferred, IPv6 used when the hostname has no A record. The system
	// resolver is used untouched.
	IPPreferenceDefault IPPreference = "default"
	// IPPreferenceV4Only resolves A records only: a hostname without one
	// fails its registration with the DNS error.
	IPPreferenceV4Only IPPreference = "v4Only"
	// IPPreferenceV6Only resolves AAAA records only, likewise.
	IPPreferenceV6Only IPPreference = "v6Only"
)

// ParseIPPreference validates the raw ipPreference attribute and its
// required pairing with the raw upstreamDNSResolver one: a non-default
// preference resolves through the bot's filtering DNS proxy, which needs
// an explicit upstream resolver to relay to (the system resolver is left
// to the rest of the process); the default takes no upstream. An invalid
// pairing fails the startup, the credential pool's discipline.
func ParseIPPreference(s, upstreamResAddr string) (IPPreference, error) {
	p := IPPreference(s).normalize()
	switch p {
	case IPPreferenceDefault, IPPreferenceV4Only, IPPreferenceV6Only:
	default:
		return "", fmt.Errorf("bad ipPreference %q: want %q, %q or %q", s, IPPreferenceV6Only, IPPreferenceV4Only, IPPreferenceDefault)
	}
	if p == IPPreferenceDefault {
		if upstreamResAddr != "" {
			return "", fmt.Errorf("upstreamDNSResolver %q is set but ipPreference is %q — the resolver would go unused; remove one of them", upstreamResAddr, IPPreferenceDefault)
		}
		return p, nil
	}
	if upstreamResAddr == "" {
		return "", fmt.Errorf("ipPreference %q needs an explicit upstream DNS resolver — set upstreamDNSResolver (host[:port], the port defaulting to 53)", p)
	}
	if _, err := normalizeDNSResAddr(upstreamResAddr); err != nil {
		return "", fmt.Errorf("bad upstreamDNSResolver: %w", err)
	}
	return p, nil
}

// normalize maps the zero value to the default.
func (p IPPreference) normalize() IPPreference {
	if p == "" {
		return IPPreferenceDefault
	}
	return p
}

// allows reports whether an IP's family survives the preference.
func (p IPPreference) allows(ip net.IP) bool {
	switch p.normalize() {
	case IPPreferenceV4Only:
		return ip.To4() != nil
	case IPPreferenceV6Only:
		return ip.To4() == nil
	}
	return true
}

// wildcard is the bind fallback of the preference's family, for a
// registrar that does not resolve within it (the registration then fails
// with its own error, like an unresolvable one).
func (p IPPreference) wildcard() string {
	if p.normalize() == IPPreferenceV6Only {
		return "::"
	}
	return "0.0.0.0"
}

// normalizeDNSResAddr canonicalizes an upstream resolver address to
// host:port: the port defaults to 53, and a bare IPv6 literal gains its
// brackets.
func normalizeDNSResAddr(s string) (string, error) {
	if s == "" {
		return "", fmt.Errorf("empty upstream DNS resolver address")
	}
	if _, err := netip.ParseAddrPort(s); err == nil {
		return s, nil
	}
	if _, _, err := net.SplitHostPort(s); err == nil {
		return s, nil // a hostname with a port
	}
	if strings.ContainsAny(s, " \t/") {
		return "", fmt.Errorf("bad upstream DNS resolver address %q (want host[:port])", s)
	}
	if a, err := netip.ParseAddr(s); err == nil && a.Is6() {
		return "[" + s + "]:53", nil
	}
	return s + ":53", nil
}

// upstreamExchangeTimeout bounds one relayed query's round trip to the
// upstream resolver; the caller (the Go resolver) retries per its own
// attempts budget.
const upstreamExchangeTimeout = 5 * time.Second

// filteringResolver is the bot's family-filtering DNS reverse proxy: one
// per bot (shared by every account — the filter is stateless), bound to
// an ephemeral loopback port, relaying to the configured upstream
// resolver. The account UAs' *net.Resolver dials it.
type filteringResolver struct {
	pref     IPPreference
	upstream string

	udp *dns.Server
	tcp *dns.Server

	resolver *net.Resolver
}

// startFilteringResolver starts the shared proxy on 127.0.0.1:0 (the
// kernel picks the port; UDP and TCP share it). The caller keeps the
// returned proxy for the bot's lifetime; Close stops it.
func startFilteringResolver(pref IPPreference, upstreamResAddr string) (*filteringResolver, error) {
	upstream, err := normalizeDNSResAddr(upstreamResAddr)
	if err != nil {
		return nil, err
	}
	r := &filteringResolver{pref: pref, upstream: upstream}
	mux := dns.NewServeMux()
	mux.HandleFunc(".", r.serveDNS)

	udpConn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("the filtering DNS proxy's UDP socket: %w", err)
	}
	port := udpConn.LocalAddr().(*net.UDPAddr).Port
	// The Go resolver retries a truncated UDP answer over TCP, on the
	// same port.
	tcpLn, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		_ = udpConn.Close()
		return nil, fmt.Errorf("the filtering DNS proxy's TCP socket: %w", err)
	}
	r.udp = &dns.Server{PacketConn: udpConn, Net: "udp", Handler: mux}
	r.tcp = &dns.Server{Listener: tcpLn, Net: "tcp", Handler: mux}
	go func() { _ = r.udp.ActivateAndServe() }()
	go func() { _ = r.tcp.ActivateAndServe() }()

	addr := udpConn.LocalAddr().String()
	r.resolver = &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			// The resolv.conf server address goes unused: every query
			// goes to the proxy.
			var d net.Dialer
			return d.DialContext(ctx, network, addr)
		},
	}
	return r, nil
}

// serveDNS is the reverse proxy: the suppressed family's qtype answers
// NODATA from the filter; everything else relays to the upstream and
// back, over the transport the query arrived on (a truncated UDP answer
// makes the resolver retry over TCP, which must ask the upstream over
// TCP in turn).
func (r *filteringResolver) serveDNS(w dns.ResponseWriter, req *dns.Msg) {
	if len(req.Question) == 1 {
		qtype := req.Question[0].Qtype
		suppress := (qtype == dns.TypeAAAA && r.pref == IPPreferenceV4Only) ||
			(qtype == dns.TypeA && r.pref == IPPreferenceV6Only)
		if suppress {
			m := new(dns.Msg)
			m.SetReply(req) // NOERROR, the question echoed, no answers
			// A NODATA without the RA bit trips the Go resolver's
			// lame-referral check (an empty NOERROR that is neither
			// authoritative nor recursive) — answer as a recursive
			// resolver would.
			m.RecursionAvailable = true
			_ = w.WriteMsg(m)
			return
		}
	}
	network := "udp"
	if w.LocalAddr().Network() == "tcp" {
		network = "tcp"
	}
	c := &dns.Client{Net: network, Timeout: upstreamExchangeTimeout}
	resp, _, err := c.ExchangeContext(context.Background(), req, r.upstream)
	if err != nil {
		m := new(dns.Msg)
		m.SetRcode(req, dns.RcodeServerFailure)
		_ = w.WriteMsg(m)
		return
	}
	_ = w.WriteMsg(resp)
}

// Close stops both servers; the bot's proxy otherwise lives for the
// process (the bot has no shutdown of its own).
func (r *filteringResolver) Close() {
	_ = r.udp.Shutdown()
	_ = r.tcp.Shutdown()
}
