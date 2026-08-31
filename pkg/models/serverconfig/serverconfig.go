// Package serverconfig parses serverConfig.xml, the global server
// configuration document validated against serverConfig.xsd in the project
// root.
package serverconfig

import (
	"encoding/xml"
	"fmt"
	"os"
	"time"
)

// ServerConfigXML mirrors the structure of serverConfig.xml (validated
// against serverConfig.xsd in the project root), the global server
// configuration document.
type ServerConfigXML struct {
	XMLName          xml.Name            `xml:"serverConfig"`
	OIDCLoginOptions OIDCLoginOptionsXML `xml:"oidcLoginOptions"`
	// GithubOAuthLogin is nil when the document has no <githubOAuthLogin/>
	// element; the Github OAuth login handler is wired only when the element
	// is present.
	GithubOAuthLogin *GithubOAuthLoginXML `xml:"githubOAuthLogin"`
	LoginOptions     LoginOptionsXML      `xml:"loginOptions"`
	// AllowedOrigins holds every <allowedOrigin/> entry of the document: the
	// request origins trusted by the OAuth2/OIDC login handlers when their
	// configured redirect URL is relative (see pkg/api/common
	// ResolveRedirectURL).
	AllowedOrigins []string `xml:"allowedOrigin"`
	// IceServers holds every <iceServer/> entry of the document: the ICE
	// server sets served by GET /api/iceServers (see pkg/api/iceservers).
	IceServers []IceServerXML `xml:"iceServer"`
	// EchoBot is nil when the document has no <echoBot/> element; the
	// built-in echo bot is wired only when the element is present and
	// carries a url and a jwt (see cmd/server).
	EchoBot *EchoBotXML `xml:"echoBot"`
}

// OIDCLoginOptionsXML mirrors the <oidcLoginOptions/> section of
// serverConfig.xml. Each <oidcLoginOption/> maps onto the configuration
// fields of GenericOIDCLoginHandler.
type OIDCLoginOptionsXML struct {
	Options []OIDCLoginOptionXML `xml:"oidcLoginOption"`
}

// OIDCLoginOptionXML mirrors a single <oidcLoginOption/> entry of the
// <oidcLoginOptions/> section of serverConfig.xml.
type OIDCLoginOptionXML struct {
	ProviderName            string `xml:"providerName,attr"`
	IssuerURL               string `xml:"issuerURL,attr"`
	ClientId                string `xml:"clientId,attr"`
	ClientSecret            string `xml:"clientSecret,attr"`
	RedirectURL             string `xml:"redirectURL,attr"`
	Scope                   string `xml:"scope,attr"`
	SessionLifespan         string `xml:"sessionLifespan,attr"`
	LoginSuccessRedirectURL string `xml:"loginSuccessRedirectURL,attr"`
}

// GithubOAuthLoginXML mirrors the <githubOAuthLogin/> section of
// serverConfig.xml: the single Github OAuth login provider. Attribute names
// mirror the fields of GithubOAuthLoginHandler (pkg/api/login/oauth2/github)
// with the redundant GithubOAuth prefix dropped, and map directly onto the
// constructor parameters of NewGithubOAuthLoginHandler.
type GithubOAuthLoginXML struct {
	ClientId                string `xml:"clientId,attr"`
	AppSecret               string `xml:"appSecret,attr"`
	RedirectURL             string `xml:"redirectURL,attr"`
	LoginPage               string `xml:"loginPage,attr"`
	Scope                   string `xml:"scope,attr"`
	TokenEndpoint           string `xml:"tokenEndpoint,attr"`
	SessionLifespan         string `xml:"sessionLifespan,attr"`
	LoginSuccessRedirectURL string `xml:"loginSuccessRedirectURL,attr"`
}

// LoginOptionsXML mirrors the <loginOptions/> section of serverConfig.xml:
// the login page's configurable IdP list, served to the frontend as JSON by
// the login options API handler (pkg/api/loginoptions).
type LoginOptionsXML struct {
	Options []LoginOptionXML `xml:"loginOption"`
}

// LoginOptionXML mirrors a single <loginOption/> entry of the
// <loginOptions/> section of serverConfig.xml.
type LoginOptionXML struct {
	Kind        string `xml:"kind,attr"`
	Name        string `xml:"name,attr"`
	DisplayName string `xml:"displayName,attr"`
	Label       string `xml:"label,attr"`
	LoginURL    string `xml:"loginURL,attr"`
	// AllowedOrigins is the raw comma-separated allowedOrigins attribute;
	// an empty string means no origin restriction. Split it with
	// loginoptions.ParseAllowedOrigins.
	AllowedOrigins string `xml:"allowedOrigins,attr"`
}

// EchoBotXML mirrors the <echoBot/> section of serverConfig.xml: the
// built-in echo-purpose bot (pkg/rtc/echobot on a pkg/rtc
// HeadlessRTCClient), which lives in the server process as a plain
// signalling client of the WebSocket endpoint URL points at — typically
// this server's own /api/ss/ws. JWT is the bot's session token, sent as a
// bearer token on the WebSocket handshake (the endpoint is not on the JWT
// whitelist); the token is the bot's whole identity — the endpoint stamps
// its subject/session onto the bot's events, and its username claim is
// the registration's display name, like every client's. The remaining
// attribute names mirror the fields of rtc.RTCClientConfiguration with a
// lowercased first letter.
type EchoBotXML struct {
	URL string `xml:"url,attr"`
	JWT string `xml:"jwt,attr"`
	// ChannelId is the channel the bot registers in; empty selects the
	// well-known main channel.
	ChannelId string `xml:"channelId,attr"`
	// SubscriberId is the bot's subscriber id; empty asks the SS to
	// assign one from the automatic assignment range.
	SubscriberId string `xml:"subscriberId,attr"`
	// IceServers is the raw comma-separated iceServers attribute; split
	// it with iceservers.ParseURLs. Empty means no ICE servers: the
	// bot's peer connections run on host candidates alone.
	IceServers string `xml:"iceServers,attr"`
	// The duration attributes are Go time.Duration strings, parsed with
	// ParseDuration; empty selects the wiring's default.
	KeepAliveInterval  string `xml:"keepAliveInterval,attr"`
	MemberListInterval string `xml:"memberListInterval,attr"`
	ReplyTimeout       string `xml:"replyTimeout,attr"`
	ReconnectInterval  string `xml:"reconnectInterval,attr"`
}

// IceServerXML mirrors a single <iceServer/> entry of serverConfig.xml.
// URLs is the raw comma-separated urls attribute; split it with
// iceservers.ParseURLs. AllowedOrigin restricts the entry to requests
// whose origin matches it exactly; an empty string applies the entry to
// every origin.
type IceServerXML struct {
	URLs          string `xml:"urls,attr"`
	AllowedOrigin string `xml:"allowedOrigin,attr"`
}

// LoadServerConfig parses the global server configuration XML document.
func LoadServerConfig(path string) (*ServerConfigXML, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read server config file %s: %w", path, err)
	}
	var cfg ServerConfigXML
	if err := xml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse server config file %s: %w", path, err)
	}
	return &cfg, nil
}

// ParseSessionLifespan parses a Go time.Duration string, falling back to the
// given default when the input is empty.
func ParseSessionLifespan(s string, fallback time.Duration) (time.Duration, error) {
	return ParseDuration(s, fallback)
}

// ParseDuration parses a Go time.Duration string, falling back to the
// given default when the input is empty.
func ParseDuration(s string, fallback time.Duration) (time.Duration, error) {
	if s == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("invalid duration %q: %w", s, err)
	}
	return d, nil
}
