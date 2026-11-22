package sipregistration

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"math/big"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/emiago/diago"
	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
)

// diagoRuntime owns one account's UA, listener, and REGISTER transaction. The
// listener uses an ephemeral port: credentials are independent UAs and no
// account monopolizes a host-wide SIP port.
type diagoRuntime struct {
	transaction *diago.RegisterTransaction
	ua          *sipgo.UserAgent
	stopServing context.CancelFunc
	closeOnce   sync.Once
	closeErr    error
}

func newDiagoRuntime(credential Credential, manager *Manager) (runtime, error) {
	// Revalidate on every attempt and pin the actual destination to the
	// validated address. The SIP URI still carries the configured domain.
	dialHost, err := manager.registrarDialHost(credential)
	if err != nil {
		return nil, err
	}
	port := credential.registrarPort
	if port == 0 {
		port = defaultRegistrarPort(credential.Transport)
	}
	uaOptions := []sipgo.UserAgentOption{sipgo.WithUserAgent(credential.Username)}
	var serverTLS *tls.Config
	if credential.Transport == TransportTLS {
		// Outbound registrar certificates are verified against the system roots.
		uaOptions = append(uaOptions, sipgo.WithUserAgenTLSConfig(&tls.Config{MinVersion: tls.VersionTLS12, ServerName: strings.Trim(credential.registrarHost, "[]")}))
		certificate, err := ephemeralServerCertificate(credential.registrarHost)
		if err != nil {
			return nil, fmt.Errorf("create TLS listener certificate: %w", err)
		}
		// A TLS listener also needs a certificate. This ephemeral certificate is
		// only for inbound connections to this UA; it is not used to trust the
		// registrar. Deployments requiring mutual TLS or a stable inbound identity
		// need explicit certificate configuration (see docs/user-sip-subsystem.md).
		serverTLS = &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12}
	}
	ua, err := sipgo.NewUA(uaOptions...)
	if err != nil {
		return nil, err
	}
	stackCtx, stopServing := context.WithCancel(context.Background())
	closeStack := func() {
		stopServing()
		_ = ua.Close()
	}
	bindHost := sourceAddressFor(dialHost, port)
	dg := diago.NewDiago(ua,
		diago.WithLogger(manager.logger),
		diago.WithTransport(diago.Transport{
			ID:        string(credential.Transport),
			Transport: string(credential.Transport),
			BindHost:  bindHost,
			BindPort:  0,
			TLSConf:   serverTLS,
		}),
		diago.WithMessageHandler(manager.handleInboundMessage),
	)
	if err := dg.ServeBackground(stackCtx, manager.handleInboundInvite); err != nil {
		closeStack()
		return nil, fmt.Errorf("start SIP listener: %w", err)
	}

	scheme := "sip"
	if credential.Transport == TransportTLS {
		scheme = "sips"
	}
	params := sip.NewParams()
	params.Add("transport", string(credential.Transport))
	recipient := sip.Uri{
		Scheme:    scheme,
		User:      credential.Username,
		Host:      credential.registrarHost,
		Port:      port,
		UriParams: params,
	}
	transaction, err := dg.RegisterTransaction(context.Background(), recipient, diago.RegisterOptions{
		Username:  credential.Username,
		Password:  credential.Password,
		ProxyHost: net.JoinHostPort(dialHost, strconv.Itoa(port)),
		Expiry:    manager.registrationExpiry,
	})
	if err != nil {
		closeStack()
		return nil, err
	}
	from := &sip.FromHeader{
		Address: sip.Uri{Scheme: "sip", User: credential.Username, Host: credential.registrarHost},
		Params:  sip.NewParams(),
	}
	from.Params.Add("tag", sip.GenerateTagN(16))
	transaction.Origin.AppendHeader(from)
	// Keep the Contact user equal to the AOR user even if the UA default changes.
	transaction.Origin.Contact().Address.User = credential.Username
	return &diagoRuntime{transaction: transaction, ua: ua, stopServing: stopServing}, nil
}

func (r *diagoRuntime) Register(ctx context.Context) error {
	return r.transaction.Register(ctx)
}

func (r *diagoRuntime) Refresh(ctx context.Context) error {
	return r.transaction.Qualify(ctx)
}

func (r *diagoRuntime) Unregister(ctx context.Context) error {
	return r.transaction.Unregister(ctx)
}

func (r *diagoRuntime) Expiry() time.Duration {
	return r.transaction.Expiry()
}

func (r *diagoRuntime) Close() error {
	r.closeOnce.Do(func() {
		r.stopServing()
		r.closeErr = r.ua.Close()
	})
	return r.closeErr
}

func defaultRegistrarPort(transport Transport) int {
	if transport == TransportTLS {
		return 5061
	}
	return 5060
}

// sourceAddressFor chooses the same local address the kernel would use toward
// the registrar. A UDP dial performs route selection without sending traffic;
// that answer is also appropriate for TCP/TLS/WS listener binding.
func sourceAddressFor(host string, port int) string {
	bareHost := strings.Trim(host, "[]")
	fallback := "0.0.0.0"
	if ip := net.ParseIP(bareHost); ip != nil && ip.To4() == nil {
		fallback = "::"
	}
	address, err := net.ResolveUDPAddr("udp", net.JoinHostPort(bareHost, strconv.Itoa(port)))
	if err != nil || address.IP == nil {
		return fallback
	}
	connection, err := net.DialUDP("udp", nil, address)
	if err != nil {
		if address.IP.To4() == nil {
			return "::"
		}
		return fallback
	}
	defer connection.Close()
	local, ok := connection.LocalAddr().(*net.UDPAddr)
	if !ok {
		return fallback
	}
	return local.IP.String()
}

// ephemeralServerCertificate creates a short-lived, process-local certificate
// for the TLS listener. Outbound trust remains independent and system-rooted.
func ephemeralServerCertificate(host string) (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, err
	}
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "personal-site SIP UA"},
		NotBefore:    now.Add(-time.Minute),
		NotAfter:     now.Add(30 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	bareHost := strings.Trim(host, "[]")
	if ip := net.ParseIP(bareHost); ip != nil {
		template.IPAddresses = []net.IP{ip}
	} else if bareHost != "" {
		template.DNSNames = []string{bareHost}
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, nil
}
