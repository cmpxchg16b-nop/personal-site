package sipregistration

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"time"
)

const registrarResolveTimeout = 3 * time.Second

var nonPublicRegistrarPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),       // this network
	netip.MustParsePrefix("192.0.0.0/24"),    // IETF protocol assignments
	netip.MustParsePrefix("192.0.2.0/24"),    // documentation
	netip.MustParsePrefix("192.88.99.0/24"),  // deprecated 6to4 relay anycast
	netip.MustParsePrefix("198.18.0.0/15"),   // benchmarking
	netip.MustParsePrefix("198.51.100.0/24"), // documentation
	netip.MustParsePrefix("203.0.113.0/24"),  // documentation
	netip.MustParsePrefix("240.0.0.0/4"),     // reserved
	netip.MustParsePrefix("64:ff9b:1::/48"),  // local-use NAT64
	netip.MustParsePrefix("100::/64"),        // discard-only
	netip.MustParsePrefix("2001:2::/48"),     // benchmarking
	netip.MustParsePrefix("2001:db8::/32"),   // documentation
	netip.MustParsePrefix("fec0::/10"),       // deprecated site-local
}

// validateRegistrarTarget applies the anonymous-visitor egress policy. Normal
// authenticated subjects may use private PBXs; visitors cannot use the server
// as an unsolicited private-network SIP probe unless the operator explicitly
// enables that behavior. All resolved addresses must be acceptable, so a
// hostname cannot mix a public answer with a private fallback.
func (m *Manager) validateRegistrarTarget(credential Credential) error {
	_, err := m.registrarDialHost(credential)
	return err
}

// registrarDialHost pins anonymous visitors to a validated public IP for the
// actual socket destination. The original domain remains in the SIP AOR and TLS
// ServerName, but a later DNS rebinding cannot redirect this attempt elsewhere.
func (m *Manager) registrarDialHost(credential Credential) (string, error) {
	host := strings.Trim(credential.registrarHost, "[]")
	if m.allowPrivateRegistrars || !strings.HasPrefix(credential.OwnerID, "visitor:") {
		return host, nil
	}
	if ip := net.ParseIP(host); ip != nil {
		if !publicRegistrarIP(ip) {
			return "", fmt.Errorf("%w: visitor SIP registrar must be a public IP address", ErrInvalidInput)
		}
		return ip.String(), nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), registrarResolveTimeout)
	defer cancel()
	addresses, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
	if err != nil {
		return "", fmt.Errorf("%w: resolve SIP registrar: %v", ErrInvalidInput, err)
	}
	if len(addresses) == 0 {
		return "", fmt.Errorf("%w: SIP registrar resolved to no addresses", ErrInvalidInput)
	}
	for _, ip := range addresses {
		if !publicRegistrarIP(ip) {
			return "", fmt.Errorf("%w: visitor SIP registrar resolves to a private or special-use address", ErrInvalidInput)
		}
	}
	return addresses[0].String(), nil
}

func publicRegistrarIP(ip net.IP) bool {
	if ipv4 := ip.To4(); ipv4 != nil {
		ip = ipv4
	}
	if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return false
	}
	// Shared/CGNAT space is not public Registrar address space either.
	if ipv4 := ip.To4(); ipv4 != nil && ipv4[0] == 100 && ipv4[1] >= 64 && ipv4[1] <= 127 {
		return false
	}
	address, ok := netip.AddrFromSlice(ip)
	if !ok {
		return false
	}
	address = address.Unmap()
	for _, prefix := range nonPublicRegistrarPrefixes {
		if prefix.Contains(address) {
			return false
		}
	}
	return true
}
