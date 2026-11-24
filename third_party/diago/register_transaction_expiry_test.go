package diago

import (
	"testing"
	"time"

	"github.com/emiago/sipgo/sip"
)

func TestRegisterResponseExpiry(t *testing.T) {
	response := func() *sip.Response {
		return sip.NewResponse(200, "OK")
	}
	t.Run("requested lease is the fallback", func(t *testing.T) {
		got, err := registerResponseExpiry(response(), 5*time.Minute, nil)
		if err != nil || got != 5*time.Minute {
			t.Fatalf("expiry = %v, err = %v", got, err)
		}
	})
	t.Run("response Expires header", func(t *testing.T) {
		res := response()
		res.AppendHeader(sip.NewHeader("Expires", "120"))
		got, err := registerResponseExpiry(res, 5*time.Minute, nil)
		if err != nil || got != 2*time.Minute {
			t.Fatalf("expiry = %v, err = %v", got, err)
		}
	})
	t.Run("Contact binding expiry takes precedence", func(t *testing.T) {
		res := response()
		res.AppendHeader(sip.NewHeader("Expires", "120"))
		params := sip.NewParams()
		params.Add("expires", "45")
		res.AppendHeader(&sip.ContactHeader{Address: sip.Uri{Scheme: "sip", User: "alice", Host: "192.0.2.1"}, Params: params})
		got, err := registerResponseExpiry(res, 5*time.Minute, nil)
		if err != nil || got != 45*time.Second {
			t.Fatalf("expiry = %v, err = %v", got, err)
		}
	})
	t.Run("multi Contact selects our binding and parameter name is case insensitive", func(t *testing.T) {
		res := response()
		res.AppendHeader(sip.NewHeader("Contact", "<sip:other@192.0.2.2:5060>;expires=3600, <sip:alice@192.0.2.1:5060>;Expires=60"))
		own := &sip.ContactHeader{Address: sip.Uri{Scheme: "sip", User: "alice", Host: "192.0.2.1", Port: 5060}}
		got, err := registerResponseExpiry(res, 5*time.Minute, own)
		if err != nil || got != time.Minute {
			t.Fatalf("expiry = %v, err = %v", got, err)
		}
	})
	t.Run("multi Contact transport distinguishes otherwise identical bindings", func(t *testing.T) {
		res := response()
		res.AppendHeader(sip.NewHeader("Contact", "<sip:alice@192.0.2.1:5060;transport=tcp>;expires=3600, <sip:alice@192.0.2.1:5060;transport=uDp>;Expires=60"))
		params := sip.NewParams()
		params.Add("transport", "UDP")
		own := &sip.ContactHeader{Address: sip.Uri{Scheme: "sip", User: "alice", Host: "192.0.2.1", Port: 5060, UriParams: params}}
		got, err := registerResponseExpiry(res, 5*time.Minute, own)
		if err != nil || got != time.Minute {
			t.Fatalf("expiry = %v, err = %v", got, err)
		}
	})
	t.Run("separate Contact headers select our binding", func(t *testing.T) {
		res := response()
		res.AppendHeader(sip.NewHeader("Contact", "<sip:other@192.0.2.2:5060>;expires=3600"))
		res.AppendHeader(sip.NewHeader("Contact", "<sip:alice@192.0.2.1:5060>;expires=30"))
		own := &sip.ContactHeader{Address: sip.Uri{Scheme: "sip", User: "alice", Host: "192.0.2.1", Port: 5060}}
		got, err := registerResponseExpiry(res, 5*time.Minute, own)
		if err != nil || got != 30*time.Second {
			t.Fatalf("expiry = %v, err = %v", got, err)
		}
	})
	t.Run("malformed Contact expiry is an error", func(t *testing.T) {
		res := response()
		params := sip.NewParams()
		params.Add("expires", "soon")
		res.AppendHeader(&sip.ContactHeader{Address: sip.Uri{Scheme: "sip", User: "alice", Host: "192.0.2.1"}, Params: params})
		if _, err := registerResponseExpiry(res, time.Minute, nil); err == nil {
			t.Fatal("expected a parse error")
		}
	})
}

func TestRewriteRegisterContactFromViaTransport(t *testing.T) {
	tests := []struct {
		network  string
		wantHost string
		wantPort int
	}{
		{network: "UDP", wantHost: "198.51.100.20", wantPort: 24000},
		{network: "TCP", wantHost: "192.0.2.10", wantPort: 15060},
		{network: "TLS", wantHost: "192.0.2.10", wantPort: 15060},
		{network: "WS", wantHost: "192.0.2.10", wantPort: 15060},
		{network: "WSS", wantHost: "192.0.2.10", wantPort: 15060},
	}
	for _, test := range tests {
		t.Run(test.network, func(t *testing.T) {
			request := sip.NewRequest(sip.REGISTER, sip.Uri{Scheme: "sip", User: "alice", Host: "pbx.example"})
			request.SetTransport(test.network)
			contact := &sip.ContactHeader{Address: sip.Uri{Scheme: "sip", User: "alice", Host: "192.0.2.10", Port: 15060}}
			request.AppendHeader(contact)
			params := sip.NewParams()
			params.Add("rport", "24000")
			params.Add("received", "198.51.100.20")
			via := &sip.ViaHeader{Transport: test.network, Host: "192.0.2.10", Port: 15060, Params: params}

			rewriteRegisterContactFromVia(request, via, contact.Clone())

			got := request.Contact()
			if got.Address.Host != test.wantHost || got.Address.Port != test.wantPort {
				t.Fatalf("%s Contact = %s:%d, want %s:%d", test.network, got.Address.Host, got.Address.Port, test.wantHost, test.wantPort)
			}
		})
	}
}

func TestBuildUnregisterRequestExpiresOnlyOurContact(t *testing.T) {
	params := sip.NewParams()
	params.Add("Expires", "300")
	request := sip.NewRequest(sip.REGISTER, sip.Uri{Scheme: "sip", User: "alice", Host: "pbx.example"})
	request.AppendHeader(&sip.ContactHeader{Address: sip.Uri{Scheme: "sip", User: "alice", Host: "192.0.2.1", Port: 5060}, Params: params})
	if err := buildUnregisterRequest(request); err != nil {
		t.Fatal(err)
	}
	contact := request.Contact()
	if contact == nil || contact.Address.Wildcard || contact.Address.User != "alice" {
		t.Fatalf("Contact = %+v", contact)
	}
	if raw, ok := contactParam(contact.Params, "expires"); !ok || raw != "0" {
		t.Fatalf("Contact expires = %q, %v; params = %+v", raw, ok, contact.Params)
	}
	if expires := request.GetHeader("Expires"); expires == nil || expires.Value() != "0" {
		t.Fatalf("Expires = %+v", expires)
	}
}
