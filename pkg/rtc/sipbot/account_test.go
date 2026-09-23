package sipbot

import (
	"net"
	"strings"
	"testing"
)

// TestParseAddressOfRecord covers the /register argument's parsing: the
// plain AOR, scheme toleration, and the failures (no host, no user,
// garbage).
func TestParseAddressOfRecord(t *testing.T) {
	sess, err := parseAddressOfRecord("2001@sip.example.com", "passW_0rd")
	if err != nil {
		t.Fatalf("parseAddressOfRecord: %v", err)
	}
	want := UserSession{AddressOfRecord: "2001@sip.example.com", Username: "2001", Host: "sip.example.com", Password: "passW_0rd"}
	if sess != want {
		t.Fatalf("session = %+v, want %+v", sess, want)
	}

	sess, err = parseAddressOfRecord("sip:2002@192.168.1.2:5060", "p")
	if err != nil {
		t.Fatalf("parseAddressOfRecord with scheme and port: %v", err)
	}
	if sess.Username != "2002" || sess.Host != "192.168.1.2" || sess.Port != 5060 || sess.Password != "p" || sess.AddressOfRecord != "2002@192.168.1.2:5060" {
		t.Fatalf("session = %+v", sess)
	}

	// An IPv6 literal is bracketed (RFC 3261); the Host keeps the
	// brackets, the AOR renders back into a valid URI.
	sess, err = parseAddressOfRecord("1005@[2a0a:4cc0::1]:5060", "p")
	if err != nil {
		t.Fatalf("parseAddressOfRecord with an IPv6 host: %v", err)
	}
	if sess.Username != "1005" || sess.Host != "[2a0a:4cc0::1]" || sess.Port != 5060 || sess.AddressOfRecord != "1005@[2a0a:4cc0::1]:5060" {
		t.Fatalf("session = %+v", sess)
	}

	// …and an unbracketed one is rejected with the bracket hint.
	if _, err := parseAddressOfRecord("1005@2a0a:4cc0::1", "p"); err == nil || !strings.Contains(err.Error(), "brackets") {
		t.Fatalf("parseAddressOfRecord of an unbracketed IPv6 = %v, want the bracket hint", err)
	}

	for _, bad := range []string{"2001", "@sip.example.com", "sip:", "a b@c"} {
		if _, err := parseAddressOfRecord(bad, "p"); err == nil {
			t.Fatalf("parseAddressOfRecord(%q) succeeded, want an error", bad)
		}
	}
}

// TestBindHostFor covers the account socket's address pick: an empty
// configured BindHost selects the route's source address toward the
// registrar — an IPv6 address for an IPv6 registrar, IPv4 for IPv4 (the
// exact address is the host's routing table's, so the assertions check
// the family), an unresolvable registrar gets the IPv4 wildcard — and an
// explicit BindHost wins verbatim.
func TestBindHostFor(t *testing.T) {
	auto := sipStack{}
	for _, tc := range []struct {
		host string
		v6   bool
	}{
		{"192.168.1.2", false},
		{"127.0.0.1", false},
		{"[2a0a:4cc0::1]", true},
		{"[::1]", true},
	} {
		got := auto.bindHostFor(UserSession{Host: tc.host})
		ip := net.ParseIP(got)
		if ip == nil {
			t.Errorf("bindHostFor(%q) = %q, not an IP", tc.host, got)
			continue
		}
		if v6 := ip.To4() == nil; v6 != tc.v6 {
			t.Errorf("bindHostFor(%q) = %q, wrong address family (want v6=%v)", tc.host, got, tc.v6)
		}
	}
	// Unresolvable: the IPv4 wildcard default.
	if got := auto.bindHostFor(UserSession{Host: "not a valid host!"}); got != "0.0.0.0" {
		t.Errorf("bindHostFor(unresolvable) = %q, want 0.0.0.0", got)
	}
	// A pinned BindHost wins regardless of the registrar's family.
	pinned := sipStack{bindHost: "127.0.0.1"}
	if got := pinned.bindHostFor(UserSession{Host: "[2a0a:4cc0::1]"}); got != "127.0.0.1" {
		t.Errorf("pinned bindHostFor = %q, want the pin", got)
	}
}
