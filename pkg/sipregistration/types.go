// Package sipregistration owns users' SIP credentials and the server-side
// REGISTER lifecycle. Browsers manage credentials through the site's own API;
// they never exchange SIP directly with a registrar.
package sipregistration

import (
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/emiago/sipgo/sip"
)

// Transport is the SIP transport used by the server-side user agent.
type Transport string

const (
	TransportUDP Transport = "udp"
	TransportTCP Transport = "tcp"
	TransportTLS Transport = "tls"
	TransportWS  Transport = "ws"
)

// RegistrationState is the observed state of a credential's server-side
// REGISTER lifecycle. Enabled is the desired state; RegistrationState is never
// accepted from a client.
type RegistrationState string

const (
	RegistrationDisabled    RegistrationState = "disabled"
	RegistrationRegistering RegistrationState = "registering"
	RegistrationRegistered  RegistrationState = "registered"
	RegistrationFailed      RegistrationState = "failed"
)

// Registration describes the latest observed registration attempt. Attempts is
// the number of consecutive failures; it resets after a successful REGISTER or
// refresh. Times are UTC when serialized by the API.
type Registration struct {
	State        RegistrationState `json:"state"`
	LastError    string            `json:"last_error,omitempty"`
	Attempts     int               `json:"attempts"`
	RegisteredAt *time.Time        `json:"registered_at,omitempty"`
	ExpiresAt    *time.Time        `json:"expires_at,omitempty"`
	RetryAt      *time.Time        `json:"retry_at,omitempty"`
}

// Credential is one user's SIP account. Password is runtime-only data and must
// never be serialized or logged. OwnerID is the authenticated JWT subject.
type Credential struct {
	ID           string
	OwnerID      string
	SIPURI       string
	Server       string
	Username     string
	Password     string
	Transport    Transport
	Enabled      bool
	Registration Registration

	registrarHost string
	registrarPort int
}

// Input is the desired credential configuration. On update, an empty Password
// preserves the stored password; registration fields are deliberately absent.
type Input struct {
	Server    string
	Username  string
	Password  string
	Transport Transport
	Enabled   bool
	// PreserveEnabled is update-only: the manager reads Enabled from the
	// current credential while holding that entry's lifecycle lock. It avoids a
	// stale API GET or edit dialog accidentally reversing a concurrent toggle.
	PreserveEnabled bool
}

var (
	ErrInvalidInput = errors.New("invalid SIP credential")
	ErrNotFound     = errors.New("SIP credential not found")
	ErrConflict     = errors.New("SIP URI is already configured")
	ErrQuota        = errors.New("SIP credential limit reached")
	ErrClosed       = errors.New("SIP registration manager is closed")
)

func normalizeInput(input Input) (Credential, error) {
	server := strings.TrimSpace(input.Server)
	username := strings.TrimSpace(input.Username)
	transport := Transport(strings.ToLower(strings.TrimSpace(string(input.Transport))))
	if transport == "websocket" {
		transport = TransportWS
	}
	if transport == "" {
		transport = TransportUDP
	}
	switch transport {
	case TransportUDP, TransportTCP, TransportTLS, TransportWS:
	default:
		return Credential{}, fmt.Errorf("%w: unsupported transport %q", ErrInvalidInput, input.Transport)
	}
	if username == "" {
		return Credential{}, fmt.Errorf("%w: username is required", ErrInvalidInput)
	}
	if strings.Contains(username, "@") || strings.IndexFunc(username, unicode.IsSpace) >= 0 {
		return Credential{}, fmt.Errorf("%w: username must be a SIP user part, not a URI", ErrInvalidInput)
	}

	host, port, normalizedServer, err := parseServer(server)
	if err != nil {
		return Credential{}, err
	}
	// Parse the complete AOR as well as its parts. This rejects user characters
	// that would make the identity ambiguous on the wire without imposing a
	// phone-number-only policy on valid SIP usernames.
	aor := sip.Uri{}
	if err := sip.ParseUri("sip:"+username+"@"+host, &aor); err != nil || aor.User != username {
		if err == nil {
			err = errors.New("user part was not preserved")
		}
		return Credential{}, fmt.Errorf("%w: invalid SIP username: %v", ErrInvalidInput, err)
	}

	return Credential{
		SIPURI:        canonicalAOR(username, host),
		Server:        normalizedServer,
		Username:      username,
		Password:      input.Password,
		Transport:     transport,
		Enabled:       input.Enabled,
		registrarHost: host,
		registrarPort: port,
	}, nil
}

// parseServer validates host[:port], with an optional sip: scheme for API
// convenience. The AOR domain is the host; the port belongs only to the
// registrar destination and is not part of the AOR routing key.
func parseServer(server string) (host string, port int, normalized string, err error) {
	if server == "" {
		return "", 0, "", fmt.Errorf("%w: SIP server is required", ErrInvalidInput)
	}
	if strings.IndexFunc(server, unicode.IsSpace) >= 0 {
		return "", 0, "", fmt.Errorf("%w: SIP server contains whitespace", ErrInvalidInput)
	}
	if strings.HasPrefix(strings.ToLower(server), "sips:") {
		return "", 0, "", fmt.Errorf("%w: use the TLS transport instead of a sips: server", ErrInvalidInput)
	}
	if len(server) >= 4 && strings.EqualFold(server[:4], "sip:") {
		server = server[4:]
	}
	uri := sip.Uri{}
	if parseErr := sip.ParseUri("sip:"+server, &uri); parseErr != nil {
		return "", 0, "", fmt.Errorf("%w: invalid SIP server: %v", ErrInvalidInput, parseErr)
	}
	if uri.User != "" || uri.Host == "" {
		return "", 0, "", fmt.Errorf("%w: SIP server must be host[:port], without a user part", ErrInvalidInput)
	}
	if uri.Port < 0 || uri.Port > 65535 {
		return "", 0, "", fmt.Errorf("%w: SIP server port is out of range", ErrInvalidInput)
	}
	if uri.UriParams.Length() > 0 || uri.Headers.Length() > 0 {
		return "", 0, "", fmt.Errorf("%w: SIP server must not contain URI parameters or headers", ErrInvalidInput)
	}
	host = strings.ToLower(uri.Host)
	port = uri.Port
	normalized = host
	if port > 0 {
		normalized = fmt.Sprintf("%s:%d", host, port)
	}
	return host, port, normalized, nil
}

// canonicalAOR keeps the SIP user part case-sensitive while normalizing the
// case-insensitive host. Registrar ports, To tags, and transport parameters do
// not form part of the identity used for inbound user lookup.
func canonicalAOR(username, host string) string {
	return "sip:" + username + "@" + strings.ToLower(host)
}

func sipUriForLookup(raw string) (sip.Uri, error) {
	raw = strings.TrimSpace(raw)
	hasScheme := (len(raw) >= 4 && strings.EqualFold(raw[:4], "sip:")) ||
		(len(raw) >= 5 && strings.EqualFold(raw[:5], "sips:"))
	if !hasScheme {
		raw = "sip:" + raw
	}
	uri := sip.Uri{}
	if err := sip.ParseUri(raw, &uri); err != nil {
		return sip.Uri{}, err
	}
	return uri, nil
}

func cloneTime(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func cloneRegistration(status Registration) Registration {
	status.RegisteredAt = cloneTime(status.RegisteredAt)
	status.ExpiresAt = cloneTime(status.ExpiresAt)
	status.RetryAt = cloneTime(status.RetryAt)
	return status
}

func cloneCredential(credential Credential) Credential {
	credential.Registration = cloneRegistration(credential.Registration)
	return credential
}
