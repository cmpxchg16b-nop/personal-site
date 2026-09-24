package sipbot

// The yellow page's rendering tests: the /yellow-page listing's shape —
// sections in order, a line per contact, the description only when the
// entry carries one — and the empty page's answer.

import (
	"strings"
	"testing"
)

func TestYellowPageTextEmpty(t *testing.T) {
	for _, page := range [][]YellowPageSection{nil, {}} {
		if got := yellowPageText(page); got != yellowPageEmpty {
			t.Fatalf("yellowPageText(%v) = %q, want %q", page, got, yellowPageEmpty)
		}
	}
}

func TestYellowPageText(t *testing.T) {
	page := []YellowPageSection{
		{ID: "sec-local", Name: "local", Contacts: []YellowPageContact{
			{ID: "echo", Name: "echo test", AOR: "9196"},
			{ID: "echo-delayed", Name: "delayed echo test", AOR: "9195", Description: "echoes back after 250ms"},
			{ID: "moh", Name: "music on hold", AOR: "9664"},
		}},
		{ID: "sec-pbx", Name: "the PBX", Contacts: []YellowPageContact{
			{ID: "alice", Name: "Alice", AOR: "sip:alice@pbx.example.com"},
		}},
	}
	want := "Yellow page — dial with /call <aor>:\n" +
		"local:\n" +
		"- echo test: 9196\n" +
		"- delayed echo test: 9195 — echoes back after 250ms\n" +
		"- music on hold: 9664\n" +
		"the PBX:\n" +
		"- Alice: sip:alice@pbx.example.com"
	if got := yellowPageText(page); got != want {
		t.Fatalf("yellowPageText = %q, want %q", got, want)
	}
	// The contacts' and sections' opaque ids stay out of the listing.
	for _, id := range []string{"sec-local", "sec-pbx", "echo-delayed"} {
		if strings.Contains(want, id) {
			t.Fatalf("the listing leaks the opaque id %q: %q", id, want)
		}
	}
}
