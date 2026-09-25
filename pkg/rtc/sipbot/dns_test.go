package sipbot

// The ipPreference tests: the parse/normalize tables, and the filtering
// DNS proxy driven end to end — a fake upstream resolver (a table-driven
// miekg/dns server on the loopback) behind the bot's proxy, queried
// through the real net.Resolver machinery, exactly the way sipgo's
// transport layer drives it.

import (
	"context"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/miekg/dns"
)

// TestParseIPPreference covers the attribute's validation: the values,
// and the upstream pairing — required with a non-default preference,
// rejected with the default, shaped host[:port].
func TestParseIPPreference(t *testing.T) {
	for _, tc := range []struct {
		s, upstream string
		want        IPPreference
	}{
		{"", "", IPPreferenceDefault},
		{"default", "", IPPreferenceDefault},
		{"v4Only", "192.168.1.1", IPPreferenceV4Only},
		{"v4Only", "192.168.1.1:5353", IPPreferenceV4Only},
		{"v6Only", "2606:4700:4700::1111", IPPreferenceV6Only},
		{"v6Only", "[2606:4700:4700::1111]:53", IPPreferenceV6Only},
		{"v6Only", "dns.example.com:53", IPPreferenceV6Only},
	} {
		got, err := ParseIPPreference(tc.s, tc.upstream)
		if err != nil {
			t.Errorf("ParseIPPreference(%q, %q): %v", tc.s, tc.upstream, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseIPPreference(%q, %q) = %q, want %q", tc.s, tc.upstream, got, tc.want)
		}
	}
	for _, tc := range []struct {
		s, upstream string
	}{
		{"v6", "192.168.1.1"},              // a bad value
		{"IPV4ONLY", "192.168.1.1"},        // case matters
		{"v4Only", ""},                     // the upstream is required
		{"v6Only", ""},                     // likewise
		{"default", "192.168.1.1"},         // the default takes no upstream
		{"", "192.168.1.1"},                // likewise (the zero value)
		{"v4Only", "not a resolver addr!"}, // a bad upstream shape
	} {
		if got, err := ParseIPPreference(tc.s, tc.upstream); err == nil {
			t.Errorf("ParseIPPreference(%q, %q) = %q, want an error", tc.s, tc.upstream, got)
		}
	}
}

// TestNormalizeDNSResAddr covers the upstream address's canonicalization:
// the port defaulting to 53 and the IPv6 brackets.
func TestNormalizeDNSResAddr(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"192.168.1.1", "192.168.1.1:53"},
		{"192.168.1.1:5353", "192.168.1.1:5353"},
		{"2606:4700:4700::1111", "[2606:4700:4700::1111]:53"},
		{"[2606:4700:4700::1111]:5353", "[2606:4700:4700::1111]:5353"},
		{"dns.example.com", "dns.example.com:53"},
		{"dns.example.com:5353", "dns.example.com:5353"},
	} {
		got, err := normalizeDNSResAddr(tc.in)
		if err != nil {
			t.Errorf("normalizeDNSResAddr(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("normalizeDNSResAddr(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	for _, bad := range []string{"", "not a resolver!"} {
		if _, err := normalizeDNSResAddr(bad); err == nil {
			t.Errorf("normalizeDNSResAddr(%q) succeeded, want an error", bad)
		}
	}
}

// fakeDNS is a test DNS resolver: a miekg/dns server on the loopback
// (UDP and TCP on one port, like the proxy under test) answering from
// the given tables — aNames maps a lowercased FQDN to its addresses
// (both families mixed), srvNames to its SRV records. A name in no
// table answers NXDOMAIN; a name in aNames but with no record of the
// asked qtype answers NODATA. A name in truncate answers UDP queries
// with the TC bit and no answers, forcing the resolver's TCP retry.
type fakeDNS struct {
	addr string

	mu      sync.Mutex
	queries int

	aNames   map[string][]string
	srvNames map[string][]*net.SRV
	truncate map[string]bool

	udp *dns.Server
	tcp *dns.Server
}

func newFakeDNS(t *testing.T, aNames map[string][]string, srvNames map[string][]*net.SRV, truncate map[string]bool) *fakeDNS {
	t.Helper()
	f := &fakeDNS{aNames: aNames, srvNames: srvNames, truncate: truncate}
	mux := dns.NewServeMux()
	mux.HandleFunc(".", f.serve)
	udpConn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("the fake DNS's UDP socket: %v", err)
	}
	port := udpConn.LocalAddr().(*net.UDPAddr).Port
	tcpLn, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		t.Fatalf("the fake DNS's TCP socket: %v", err)
	}
	f.addr = udpConn.LocalAddr().String()
	f.udp = &dns.Server{PacketConn: udpConn, Net: "udp", Handler: mux}
	f.tcp = &dns.Server{Listener: tcpLn, Net: "tcp", Handler: mux}
	go func() { _ = f.udp.ActivateAndServe() }()
	go func() { _ = f.tcp.ActivateAndServe() }()
	t.Cleanup(func() {
		_ = f.udp.Shutdown()
		_ = f.tcp.Shutdown()
	})
	return f
}

// serve answers one query from the tables.
func (f *fakeDNS) serve(w dns.ResponseWriter, req *dns.Msg) {
	f.mu.Lock()
	f.queries++
	f.mu.Unlock()
	m := new(dns.Msg)
	if len(req.Question) != 1 {
		m.SetRcode(req, dns.RcodeFormatError)
		_ = w.WriteMsg(m)
		return
	}
	q := req.Question[0]
	name := strings.ToLower(q.Name)
	m.SetReply(req)
	m.RecursionAvailable = true // a recursive resolver's posture (the Go resolver's lame-referral check needs it on empty NOERRORs)
	if f.truncate[name] && w.LocalAddr().Network() == "udp" {
		m.Truncated = true
		_ = w.WriteMsg(m)
		return
	}
	ips, known := f.aNames[name]
	if _, ok := f.srvNames[name]; ok {
		known = true
	}
	if !known {
		m.SetRcode(req, dns.RcodeNameError)
		_ = w.WriteMsg(m)
		return
	}
	switch q.Qtype {
	case dns.TypeA:
		for _, s := range ips {
			if ip := net.ParseIP(s); ip != nil && ip.To4() != nil {
				m.Answer = append(m.Answer, &dns.A{Hdr: dns.RR_Header{Name: q.Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60}, A: ip.To4()})
			}
		}
	case dns.TypeAAAA:
		for _, s := range ips {
			if ip := net.ParseIP(s); ip != nil && ip.To4() == nil {
				m.Answer = append(m.Answer, &dns.AAAA{Hdr: dns.RR_Header{Name: q.Name, Rrtype: dns.TypeAAAA, Class: dns.ClassINET, Ttl: 60}, AAAA: ip})
			}
		}
	case dns.TypeSRV:
		for _, srv := range f.srvNames[name] {
			m.Answer = append(m.Answer, &dns.SRV{Hdr: dns.RR_Header{Name: q.Name, Rrtype: dns.TypeSRV, Class: dns.ClassINET, Ttl: 60}, Priority: srv.Priority, Weight: srv.Weight, Port: srv.Port, Target: srv.Target})
		}
	}
	_ = w.WriteMsg(m)
}

// queryCount is the number of queries the fake has answered — the
// literal-bypass assertion.
func (f *fakeDNS) queryCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.queries
}

// lookupStrings resolves host through r and returns the string form of
// the answers, sorted for comparison.
func lookupStrings(t *testing.T, r *net.Resolver, host string) ([]string, error) {
	t.Helper()
	ips, err := r.LookupIPAddr(context.Background(), host)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(ips))
	for _, ip := range ips {
		out = append(out, ip.String())
	}
	slices.Sort(out)
	return out, nil
}

// TestFilteringResolver drives the bot's filtering DNS proxy through the
// real net.Resolver: the family filtering both ways, the NODATA of a
// name without the allowed family's records, the upstream's NXDOMAIN
// propagating, the SRV relay, the truncated answer's TCP retry, and the
// IP-literal bypass.
func TestFilteringResolver(t *testing.T) {
	upstream := newFakeDNS(t,
		map[string][]string{
			"dual.test.":   {"192.0.2.1", "2001:db8::1"},
			"v4host.test.": {"192.0.2.2"},
			"tcp.test.":    {"192.0.2.3"},
		},
		map[string][]*net.SRV{
			"_sip._udp.dual.test.": {{Priority: 10, Weight: 20, Port: 5060, Target: "dual.test."}},
		},
		map[string]bool{"tcp.test.": true},
	)

	t.Run("v4Only", func(t *testing.T) {
		proxy, err := startFilteringResolver(IPPreferenceV4Only, upstream.addr)
		if err != nil {
			t.Fatalf("startFilteringResolver: %v", err)
		}
		t.Cleanup(proxy.Close)
		got, err := lookupStrings(t, proxy.resolver, "dual.test")
		if err != nil {
			t.Fatalf("LookupIPAddr(dual.test): %v", err)
		}
		if want := []string{"192.0.2.1"}; !slices.Equal(got, want) {
			t.Errorf("v4Only dual.test = %v, want %v", got, want)
		}
		// A name without an AAAA record is unaffected.
		got, err = lookupStrings(t, proxy.resolver, "v4host.test")
		if err != nil || !slices.Equal(got, []string{"192.0.2.2"}) {
			t.Errorf("v4Only v4host.test = %v, %v", got, err)
		}
		// The truncated UDP answer retries over TCP.
		got, err = lookupStrings(t, proxy.resolver, "tcp.test")
		if err != nil || !slices.Equal(got, []string{"192.0.2.3"}) {
			t.Errorf("v4Only tcp.test = %v, %v (the TCP retry)", got, err)
		}
	})

	t.Run("v6Only", func(t *testing.T) {
		proxy, err := startFilteringResolver(IPPreferenceV6Only, upstream.addr)
		if err != nil {
			t.Fatalf("startFilteringResolver: %v", err)
		}
		t.Cleanup(proxy.Close)
		got, err := lookupStrings(t, proxy.resolver, "dual.test")
		if err != nil {
			t.Fatalf("LookupIPAddr(dual.test): %v", err)
		}
		if want := []string{"2001:db8::1"}; !slices.Equal(got, want) {
			t.Errorf("v6Only dual.test = %v, want %v", got, want)
		}
		// A name with no AAAA record resolves empty under v6Only.
		got, err = lookupStrings(t, proxy.resolver, "v4host.test")
		if err != nil {
			t.Logf("v6Only v4host.test: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("v6Only v4host.test = %v, want empty", got)
		}
	})

	t.Run("shared", func(t *testing.T) {
		proxy, err := startFilteringResolver(IPPreferenceV4Only, upstream.addr)
		if err != nil {
			t.Fatalf("startFilteringResolver: %v", err)
		}
		t.Cleanup(proxy.Close)

		// The upstream's NXDOMAIN propagates.
		if _, err := lookupStrings(t, proxy.resolver, "absent.test"); err == nil {
			t.Error("LookupIPAddr(absent.test) succeeded, want the NXDOMAIN")
		}

		// The SRV relay crosses the proxy untouched.
		_, srvs, err := proxy.resolver.LookupSRV(context.Background(), "sip", "udp", "dual.test")
		if err != nil || len(srvs) != 1 {
			t.Fatalf("LookupSRV: %v, %v", srvs, err)
		}
		if srvs[0].Port != 5060 || srvs[0].Target != "dual.test." {
			t.Errorf("LookupSRV = %+v", srvs[0])
		}

		// An IP literal never reaches the upstream.
		before := upstream.queryCount()
		got, err := lookupStrings(t, proxy.resolver, "192.0.2.9")
		if err != nil || !slices.Equal(got, []string{"192.0.2.9"}) {
			t.Errorf("LookupIPAddr(192.0.2.9) = %v, %v", got, err)
		}
		if n := upstream.queryCount(); n != before {
			t.Errorf("the literal query reached the upstream (%d queries, want %d)", n, before)
		}
	})
}
