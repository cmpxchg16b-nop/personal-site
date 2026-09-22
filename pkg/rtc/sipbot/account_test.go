package sipbot

import "testing"

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

	for _, bad := range []string{"2001", "@sip.example.com", "sip:", "a b@c"} {
		if _, err := parseAddressOfRecord(bad, "p"); err == nil {
			t.Fatalf("parseAddressOfRecord(%q) succeeded, want an error", bad)
		}
	}
}
