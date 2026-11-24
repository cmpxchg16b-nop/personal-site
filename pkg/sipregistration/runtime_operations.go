package sipregistration

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/emiago/diago"
	"github.com/emiago/diago/media"
	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
)

const (
	maxSIPTargetBytes    = 1024
	maxOutboundTextBytes = 16 * 1024
	outboundContentType  = "text/plain; charset=utf-8"
)

// ErrOffline means a credential has no currently usable registered runtime.
var ErrOffline = errors.New("SIP credential is offline")

// operationRuntime is the outbound surface available to a Service while it
// holds a generation-checked manager lease.
type operationRuntime interface {
	OperationContext() context.Context
	MediaBindIP() net.IP
	Invite(context.Context, Credential, sip.Uri, *media.MediaSession, func(code int)) (*diago.DialogClientSession, error)
	SendMessage(context.Context, Credential, sip.Uri, string) error
	StopOperations()
}

var _ operationRuntime = (*diagoRuntime)(nil)

func (r *diagoRuntime) OperationContext() context.Context {
	return r.operationCtx
}

func (r *diagoRuntime) MediaBindIP() net.IP {
	return append(net.IP(nil), r.mediaBindIP...)
}

// StopOperations interrupts outbound transactions without closing the UA or
// its listener connection. Diago shares that connection with REGISTER, so the
// listener itself stays up until Close, after the worker's de-REGISTER.
func (r *diagoRuntime) StopOperations() {
	if r.stopOperations != nil {
		r.stopOperations()
	}
}

func (r *diagoRuntime) contextForOperation(ctx context.Context) (context.Context, context.CancelFunc, error) {
	if ctx == nil {
		return nil, nil, fmt.Errorf("%w: operation context is nil", ErrInvalidInput)
	}
	if r.operationCtx == nil {
		return nil, nil, ErrOffline
	}
	if err := r.operationCtx.Err(); err != nil {
		return nil, nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	operationCtx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(r.operationCtx, cancel)
	return operationCtx, func() {
		stop()
		cancel()
	}, nil
}

func (r *diagoRuntime) registrarDestination() (string, error) {
	host := strings.Trim(r.proxyHost, "[]")
	if host == "" || r.proxyPort <= 0 || r.proxyPort > 65535 {
		return "", fmt.Errorf("%w: runtime has no validated registrar destination", ErrOffline)
	}
	return net.JoinHostPort(host, strconv.Itoa(r.proxyPort)), nil
}

func operationScheme(credential Credential) string {
	if credential.Transport == TransportTLS {
		return "sips"
	}
	return "sip"
}

func ownedFrom(credential Credential) *sip.FromHeader {
	from := &sip.FromHeader{
		DisplayName: credential.Username,
		Address: sip.Uri{
			Scheme: operationScheme(credential),
			User:   credential.Username,
			Host:   credential.registrarHost,
		},
		Params: sip.NewParams(),
	}
	from.Params.Add("tag", sip.GenerateTagN(16))
	return from
}

const maxSIPInviteRedirects = 5

// Invite creates an outbound dialog and sends only its INVITE. The caller owns
// SDP/browser negotiation and must subsequently Ack, Hangup, and Close as
// appropriate. Redirects reuse the same offer and media socket; only the old
// SIP transaction is closed between attempts. Other failures close both.
func (r *diagoRuntime) Invite(ctx context.Context, credential Credential, target sip.Uri, sess *media.MediaSession, onProgress func(code int)) (*diago.DialogClientSession, error) {
	failBeforeDialog := func(err error) (*diago.DialogClientSession, error) {
		if sess == nil {
			return nil, err
		}
		return nil, errors.Join(err, sess.Close())
	}
	operationCtx, done, err := r.contextForOperation(ctx)
	if err != nil {
		return failBeforeDialog(err)
	}
	defer done()
	destination, err := r.registrarDestination()
	if err != nil {
		return failBeforeDialog(err)
	}
	if r.dg == nil {
		return failBeforeDialog(ErrOffline)
	}

	currentTarget := *target.Clone()
	visited := map[string]bool{currentTarget.String(): true}
	for redirects := 0; ; redirects++ {
		dialog, err := r.dg.NewDialog(currentTarget, diago.NewDialogOptions{Transport: string(credential.Transport)})
		if err != nil {
			return failBeforeDialog(err)
		}
		dialog.InitMediaSession(sess, nil, nil)
		dialog.InviteRequest.SetDestination(destination)
		err = dialog.Invite(operationCtx, diago.InviteClientOptions{
			Username: credential.Username,
			Password: credential.Password,
			Headers:  []sip.Header{ownedFrom(credential)},
			OnResponse: func(response *sip.Response) error {
				if onProgress != nil && response.StatusCode >= 100 && response.StatusCode < 200 {
					onProgress(response.StatusCode)
				}
				return nil
			},
		})
		if err == nil {
			return dialog, nil
		}

		var response *sip.Response
		var responseErr *sipgo.ErrDialogResponse
		if errors.As(err, &responseErr) && responseErr != nil {
			response = responseErr.Res
		} else {
			var valueErr sipgo.ErrDialogResponse
			if errors.As(err, &valueErr) {
				response = valueErr.Res
			}
		}
		if response == nil || (response.StatusCode != 300 && response.StatusCode != sip.StatusMovedPermanently && response.StatusCode != sip.StatusMovedTemporarily) {
			return nil, errors.Join(err, dialog.Close())
		}
		redirectTarget, redirectDestination, redirectErr := sipInviteRedirect(credential, response)
		if redirectErr != nil {
			return nil, errors.Join(err, redirectErr, dialog.Close())
		}
		if redirects >= maxSIPInviteRedirects {
			return nil, errors.Join(err, fmt.Errorf("SIP INVITE exceeded %d redirects", maxSIPInviteRedirects), dialog.Close())
		}
		key := redirectTarget.String()
		if visited[key] {
			return nil, errors.Join(err, fmt.Errorf("SIP INVITE redirect loop at %s", key), dialog.Close())
		}
		visited[key] = true

		// Dialog.Close would also close sess. The redirected INVITE needs the
		// identical offer and socket, so dispose only of the failed SIP leg.
		if closeErr := dialog.DialogClientSession.Close(); closeErr != nil {
			var mediaCloseErr error
			if sess != nil {
				mediaCloseErr = sess.Close()
			}
			return nil, errors.Join(err, closeErr, mediaCloseErr)
		}
		currentTarget = redirectTarget
		destination = redirectDestination
	}
}

func sipInviteRedirect(credential Credential, response *sip.Response) (sip.Uri, string, error) {
	if response == nil || (response.StatusCode != 300 && response.StatusCode != sip.StatusMovedPermanently && response.StatusCode != sip.StatusMovedTemporarily) {
		return sip.Uri{}, "", errors.New("SIP INVITE response is not a supported redirect")
	}
	contact := response.Contact()
	if contact == nil {
		return sip.Uri{}, "", fmt.Errorf("SIP INVITE %d redirect has no Contact", response.StatusCode)
	}
	target, _, err := parseSIPTarget(credential, contact.Address.String())
	if err != nil {
		return sip.Uri{}, "", fmt.Errorf("invalid SIP INVITE redirect Contact: %w", err)
	}
	port := target.Port
	if port == 0 {
		port = defaultRegistrarPort(credential.Transport)
	}
	return target, net.JoinHostPort(strings.Trim(target.Host, "[]"), strconv.Itoa(port)), nil
}

// SendMessage sends one pager-mode MESSAGE through the account's pinned
// registrar. A single 401/407 challenge receives one digest retry; every final
// non-2xx response is an error.
func (r *diagoRuntime) SendMessage(ctx context.Context, credential Credential, target sip.Uri, text string) error {
	if err := validateOutboundText(text); err != nil {
		return err
	}
	operationCtx, done, err := r.contextForOperation(ctx)
	if err != nil {
		return err
	}
	defer done()
	destination, err := r.registrarDestination()
	if err != nil {
		return err
	}
	if r.dg == nil {
		return ErrOffline
	}
	client, err := r.dg.Client(string(credential.Transport))
	if err != nil {
		return fmt.Errorf("SIP MESSAGE client: %w", err)
	}
	request := sip.NewRequest(sip.MESSAGE, target)
	request.SetTransport(sip.NetworkToUpper(string(credential.Transport)))
	request.SetDestination(destination)
	request.AppendHeader(ownedFrom(credential))
	request.AppendHeader(&sip.ToHeader{Address: *target.Clone()})
	contentType := sip.ContentTypeHeader(outboundContentType)
	request.AppendHeader(&contentType)
	request.SetBody([]byte(text))

	response, err := client.Do(operationCtx, request)
	if err != nil {
		return fmt.Errorf("SIP MESSAGE transaction: %w", err)
	}
	if response.StatusCode == sip.StatusUnauthorized || response.StatusCode == sip.StatusProxyAuthRequired {
		response, err = client.DoDigestAuth(operationCtx, request, response, sipgo.DigestAuth{
			Username: credential.Username,
			Password: credential.Password,
		})
		if err != nil {
			return fmt.Errorf("authenticated SIP MESSAGE transaction: %w", err)
		}
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("SIP MESSAGE rejected: %d %s", response.StatusCode, response.Reason)
	}
	return nil
}

func validateOutboundText(text string) error {
	if text == "" {
		return fmt.Errorf("%w: outbound text is empty", ErrInvalidInput)
	}
	if !utf8.ValidString(text) {
		return fmt.Errorf("%w: outbound text is not valid UTF-8", ErrInvalidInput)
	}
	if len(text) > maxOutboundTextBytes {
		return fmt.Errorf("%w: outbound text exceeds %d bytes", ErrInvalidInput, maxOutboundTextBytes)
	}
	return nil
}

// parseSIPTarget validates a browser-supplied target without resolving or
// routing to its host. Bare users are completed with the account's registrar
// host; the returned display is always the simple sip:user@host form.
func parseSIPTarget(credential Credential, raw string) (sip.Uri, string, error) {
	invalid := func(format string, args ...any) (sip.Uri, string, error) {
		return sip.Uri{}, "", fmt.Errorf("%w: %s", ErrInvalidInput, fmt.Sprintf(format, args...))
	}
	target := strings.TrimSpace(raw)
	if target == "" {
		return invalid("SIP target is empty")
	}
	if len(target) > maxSIPTargetBytes {
		return invalid("SIP target exceeds %d bytes", maxSIPTargetBytes)
	}
	if !utf8.ValidString(target) {
		return invalid("SIP target is not valid UTF-8")
	}
	if strings.IndexFunc(target, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0 {
		return invalid("SIP target contains whitespace or control characters")
	}
	if strings.Contains(target, "?") {
		return invalid("SIP target must not contain URI headers")
	}

	scheme := operationScheme(credential)
	hasScheme := (len(target) >= 4 && strings.EqualFold(target[:4], "sip:")) ||
		(len(target) >= 5 && strings.EqualFold(target[:5], "sips:"))
	if !hasScheme {
		if colon := strings.Index(target, ":"); colon >= 0 {
			if at := strings.Index(target, "@"); at < 0 || colon < at {
				return invalid("SIP target has an unsupported scheme")
			}
		}
		target = scheme + ":" + target
	}
	body := target[strings.Index(target, ":")+1:]
	if !strings.Contains(body, "@") {
		if credential.registrarHost == "" {
			return invalid("SIP target has no account registrar host")
		}
		target = scheme + ":" + body + "@" + credential.registrarHost
	}

	uri := sip.Uri{}
	if err := sip.ParseUri(target, &uri); err != nil {
		return invalid("malformed SIP target: %v", err)
	}
	if uri.HierarhicalSlashes || uri.Wildcard || uri.User == "" || uri.Host == "" {
		return invalid("SIP target must contain a user and host")
	}
	if !strings.EqualFold(uri.Scheme, scheme) {
		return invalid("SIP target scheme does not match account transport")
	}
	uri.Scheme = scheme
	if uri.Password != "" {
		return invalid("SIP target must not contain a password")
	}
	if uri.Headers.Length() > 0 {
		return invalid("SIP target must not contain URI headers")
	}
	if uri.Port < 0 || uri.Port > 65535 {
		return invalid("SIP target port is out of range")
	}
	transport := credential.Transport
	if transport == "" {
		transport = TransportUDP
	}
	if value, ok := uri.UriParams.Get("transport"); ok && !strings.EqualFold(value, string(transport)) {
		return invalid("SIP target transport does not match account transport")
	}
	displayHost := uri.Host
	if strings.Contains(displayHost, ":") && !strings.HasPrefix(displayHost, "[") {
		displayHost = "[" + displayHost + "]"
	}
	return uri, "sip:" + uri.User + "@" + displayHost, nil
}

func (m *Manager) acquireOperation(ownerID, credentialID string) (Credential, operationRuntime, uint64, error) {
	m.mu.RLock()
	current := m.entries[credentialKey{ownerID: ownerID, id: credentialID}]
	if current == nil {
		m.mu.RUnlock()
		return Credential{}, nil, 0, ErrNotFound
	}
	credential, operations, revision, ok := m.operationLeaseLocked(current, m.now())
	m.mu.RUnlock()
	if !ok {
		return Credential{}, nil, 0, ErrOffline
	}
	return credential, operations, revision, nil
}

// leaseForRuntime resolves the exact current runtime pointer for an inbound
// callback. A replaced runtime cannot acquire its old credential's new lease.
func (m *Manager) leaseForRuntime(rt *diagoRuntime) (Credential, operationRuntime, uint64, bool) {
	if rt == nil {
		return Credential{}, nil, 0, false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, current := range m.entries {
		if current.runtime != rt {
			continue
		}
		return m.operationLeaseLocked(current, m.now())
	}
	return Credential{}, nil, 0, false
}

// operationLeaseLocked checks desired state and the latest observed binding.
// Registering/failed refresh states remain usable until that binding expires.
func (m *Manager) operationLeaseLocked(current *entry, now time.Time) (Credential, operationRuntime, uint64, bool) {
	if current == nil || current.runtime == nil || !current.credential.Enabled {
		return Credential{}, nil, 0, false
	}
	expiresAt := current.credential.Registration.ExpiresAt
	if expiresAt == nil || !now.Before(*expiresAt) {
		return Credential{}, nil, 0, false
	}
	operations, ok := current.runtime.(operationRuntime)
	if !ok {
		return Credential{}, nil, 0, false
	}
	return cloneCredential(current.credential), operations, current.revision, true
}
