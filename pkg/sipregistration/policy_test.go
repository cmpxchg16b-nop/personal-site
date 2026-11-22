package sipregistration

import (
	"net"
	"testing"
)

func TestPublicRegistrarIP(t *testing.T) {
	for _, test := range []struct {
		address string
		want    bool
	}{
		{address: "8.8.8.8", want: true},
		{address: "2606:4700:4700::1111", want: true},
		{address: "0.0.0.1"},
		{address: "127.0.0.1"},
		{address: "100.64.0.1"},
		{address: "192.0.2.1"},
		{address: "198.18.0.1"},
		{address: "198.19.255.255"},
		{address: "203.0.113.1"},
		{address: "2001:db8::1"},
		{address: "fec0::1"},
	} {
		t.Run(test.address, func(t *testing.T) {
			if got := publicRegistrarIP(net.ParseIP(test.address)); got != test.want {
				t.Fatalf("publicRegistrarIP(%s) = %v, want %v", test.address, got, test.want)
			}
		})
	}
}
