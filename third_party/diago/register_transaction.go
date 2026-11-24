// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
)

type RegisterResponseError struct {
	RegisterReq *sip.Request
	RegisterRes *sip.Response

	Msg string
}

func (e *RegisterResponseError) StatusCode() int {
	return e.RegisterRes.StatusCode
}

func (e RegisterResponseError) Error() string {
	return e.Msg
}

type RegisterOptions struct {
	// Digest auth
	Username  string
	Password  string
	ProxyHost string

	// Expiry is for Expire header
	Expiry time.Duration
	// Retry interval is interval before next Register is sent
	RetryInterval time.Duration
	AllowHeaders  []string

	OnRegistered func()

	// Useragent default will be used on what is provided as NewUA()
	// UserAgent         string
	// UserAgentHostname string
}

type RegisterTransaction struct {
	opts   RegisterOptions
	Origin *sip.Request

	client *sipgo.Client
	log    *slog.Logger

	expiry time.Duration
}

func newRegisterTransaction(client *sipgo.Client, recipient sip.Uri, contact sip.ContactHeader, log *slog.Logger, opts RegisterOptions) *RegisterTransaction {
	expiry, allowHDRS := opts.Expiry, opts.AllowHeaders
	// log := p.getLoggerCtx(ctx, "Register")
	req := sip.NewRequest(sip.REGISTER, recipient)
	req.AppendHeader(&contact)

	if opts.ProxyHost != "" {
		req.SetDestination(opts.ProxyHost)
	}
	if expiry > 0 {
		expires := sip.ExpiresHeader(expiry.Seconds())
		req.AppendHeader(&expires)
	}
	if allowHDRS != nil {
		req.AppendHeader(sip.NewHeader("Allow", strings.Join(allowHDRS, ", ")))
	}

	// if opts.Username == "" {
	// 	opts.Username = opts.UserAgent
	// }

	if opts.Username == "" {
		opts.Username = client.Name()
	}

	t := &RegisterTransaction{
		Origin: req, // origin maybe updated after first register
		opts:   opts,
		client: client,
		log:    log.With("caller", "Register"),
	}

	return t
}

func (t *RegisterTransaction) Register(ctx context.Context) error {
	if err := t.register(ctx); err != nil {
		return err
	}

	if t.opts.OnRegistered != nil {
		t.opts.OnRegistered()
	}
	return nil
}
func (t *RegisterTransaction) register(ctx context.Context) error {
	username, password := t.opts.Username, t.opts.Password
	client := t.client
	req := t.Origin
	// Each REGISTER attempt is a new transaction. In particular, do not reuse
	// the Via left by a timed-out attempt while its old transaction is winding down.
	req.RemoveHeader("Via")
	contact := req.Contact().Clone()

	res, err := func() (*sip.Response, error) {

		contact := req.Contact()
		if contact.Address.Port == 0 && net.ParseIP(contact.Address.Host) != nil {

			sipgo.ClientRequestRegisterBuild(client, req)

			tx, err := client.TransactionLayer().NewClientTransaction(ctx, req)
			if err != nil {
				return nil, err
			}
			defer tx.Terminate()

			host, port, err := sip.ParseAddr(tx.Connection().LocalAddr().String())
			if err != nil {
				return nil, err
			}
			contact.Address.Host = host
			contact.Address.Port = port

			err = tx.Init()
			if err != nil {
				return nil, err
			}

			for {
				select {
				case res := <-tx.Responses():
					if res.IsProvisional() {
						continue
					}
					return res, nil

				case <-tx.Done():
					return nil, tx.Err()

				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}
		}

		return client.Do(ctx, req, sipgo.ClientRequestRegisterBuild)
	}()
	if err != nil {
		return fmt.Errorf("fail to create transaction req=%q: %w", req.StartLine(), err)
	}
	// Capture the actual wire Contact after an ephemeral port was filled in,
	// but before rport/received NAT rewriting below. Multi-binding responses
	// are matched against this identity.
	contact = req.Contact().Clone()

	via := res.Via()
	if via == nil {
		return fmt.Errorf("no Via header in response")
	}

	rewriteRegisterContactFromVia(req, via, contact)

	if res.StatusCode == sip.StatusUnauthorized || res.StatusCode == sip.StatusProxyAuthRequired {
		res, err = client.DoDigestAuth(ctx, req, res, sipgo.DigestAuth{
			Username: username,
			Password: password,
		})
		if err != nil {
			return fmt.Errorf("fail to get response req=%q : %w", req.StartLine(), err)
		}
	}

	if res.StatusCode != 200 {
		return &RegisterResponseError{
			RegisterReq: req,
			RegisterRes: res,
			Msg:         res.StartLine(),
		}
	}

	expiry, expiryErr := registerResponseExpiry(res, t.opts.Expiry, contact)
	if expiryErr != nil {
		// The 200 may already have created the remote binding. Preserve the
		// requested lease and report success so the caller can refresh and later
		// unregister that binding instead of leaking it until remote expiry.
		t.log.Warn("Failed to parse REGISTER expiry; using requested lease", "error", expiryErr)
		expiry = t.opts.Expiry
	}
	t.expiry = expiry
	return nil
}

// Expiry returns the lease granted by the latest successful REGISTER response.
// Like the transaction itself, it is not synchronized; callers must serialize
// it with Register, Qualify, and Unregister.
// rewriteRegisterContactFromVia applies the RFC 3581 source address learned
// from a REGISTER response to this UA's Contact. A reliable transport's rport
// is the outbound connection's source port, not the UA's listening Contact.
func rewriteRegisterContactFromVia(req *sip.Request, via *sip.ViaHeader, contact *sip.ContactHeader) {
	if sip.IsReliable(req.Transport()) {
		return
	}
	if rport, _ := via.Params.Get("rport"); rport != "" {
		if p, err := strconv.Atoi(rport); err == nil {
			contact.Address.Port = p
		}

		if received, _ := via.Params.Get("received"); received != "" {
			// TODO: consider parsing IP
			contact.Address.Host = received
		}

		// Update contact address of NAT
		req.ReplaceHeader(contact)
	}
}

func (t *RegisterTransaction) Expiry() time.Duration {
	return t.expiry
}

// registerResponseExpiry prefers our Contact binding's expires parameter (RFC
// 3261 §10.2.1.1) and falls back to the response-wide Expires header. A 200 can
// contain bindings for several devices, so blindly reading the first Contact
// can schedule this UA's refresh from another device's lease.
func registerResponseExpiry(res *sip.Response, fallback time.Duration, ownContact *sip.ContactHeader) (time.Duration, error) {
	contacts, err := registerResponseContacts(res)
	if err != nil {
		return 0, err
	}
	var selected *sip.ContactHeader
	if len(contacts) == 1 {
		// A single binding may reflect NAT rewriting rather than the exact
		// Contact sent by this UA; it is still the only binding to consider.
		selected = contacts[0]
	} else if ownContact != nil {
		for _, candidate := range contacts {
			if sameContactAddress(candidate, ownContact) {
				selected = candidate
				break
			}
		}
	}
	if selected != nil {
		if raw, ok := contactParam(selected.Params, "expires"); ok {
			seconds, err := strconv.Atoi(raw)
			if err != nil || seconds < 0 {
				return 0, fmt.Errorf("failed to parse server Contact expires value %q", raw)
			}
			return time.Duration(seconds) * time.Second, nil
		}
	}
	if header := res.GetHeader("Expires"); header != nil {
		seconds, err := strconv.Atoi(header.Value())
		if err != nil || seconds < 0 {
			return 0, fmt.Errorf("failed to parse server Expires value %q", header.Value())
		}
		return time.Duration(seconds) * time.Second, nil
	}
	return fallback, nil
}

func registerResponseContacts(res *sip.Response) ([]*sip.ContactHeader, error) {
	var contacts []*sip.ContactHeader
	for _, raw := range res.GetHeaders("Contact") {
		parsed, err := sip.HeadersParser(sip.DefaultHeadersParser()).ParseHeader(nil, []byte(raw.Name()+": "+raw.Value()))
		if err != nil {
			return nil, fmt.Errorf("failed to parse server Contact header: %w", err)
		}
		for _, header := range parsed {
			contact, ok := header.(*sip.ContactHeader)
			if !ok {
				return nil, fmt.Errorf("server Contact parser returned %T", header)
			}
			contacts = append(contacts, contact)
		}
	}
	return contacts, nil
}

func sameContactAddress(a, b *sip.ContactHeader) bool {
	if a.Address.User != b.Address.User ||
		!strings.EqualFold(a.Address.Host, b.Address.Host) ||
		a.Address.Port != b.Address.Port ||
		!strings.EqualFold(a.Address.Scheme, b.Address.Scheme) {
		return false
	}
	aTransport, aHasTransport := contactParam(a.Address.UriParams, "transport")
	bTransport, bHasTransport := contactParam(b.Address.UriParams, "transport")
	return !aHasTransport || !bHasTransport || strings.EqualFold(aTransport, bTransport)
}

func contactParam(params sip.HeaderParams, name string) (string, bool) {
	for _, parameter := range params {
		if strings.EqualFold(parameter.K, name) {
			return parameter.V, true
		}
	}
	return "", false
}

func (t *RegisterTransaction) QualifyLoop(ctx context.Context) error {
	// TODO: based on server response Expires header this must be adjusted
	// Allows caller to adjust
	expiry := t.expiry
	retry := t.calcRetry(expiry)
	return t.reregisterLoop(ctx, retry)
}

func (t *RegisterTransaction) reregisterLoop(ctx context.Context, retry time.Duration) error {
	ticker := time.NewTicker(retry)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C: // TODO make configurable
		}
		expiry := t.expiry
		err := t.Qualify(ctx)
		if err != nil {
			return err
		}

		if t.expiry != expiry {
			// expiry got updated
			expiry = t.expiry
			retry = t.calcRetry(expiry)

			t.log.Info("Register expiry changed", "expiry_old", expiry, "expiry_new", t.expiry, "retry", retry)
			ticker.Reset(retry)
		}
	}
}

func (t *RegisterTransaction) calcRetry(expiry time.Duration) time.Duration {
	// Allow caller to use own interval
	if t.opts.RetryInterval != 0 {
		return t.opts.RetryInterval
	}

	calc := expiry.Seconds() * 0.75
	retry := time.Duration(calc) * time.Second

	// Set to 30 in case retry is not set
	if retry == 0 {
		retry = 30 * time.Second
	}

	return retry
}

func (t *RegisterTransaction) Unregister(ctx context.Context) error {
	if err := buildUnregisterRequest(t.Origin); err != nil {
		return err
	}
	return t.doRequest(ctx, t.Origin)
}

func buildUnregisterRequest(req *sip.Request) error {
	contact := req.Contact().Clone()
	if contact == nil {
		return fmt.Errorf("cannot unregister without a Contact")
	}

	// Contact: * removes every device registered for the AOR. Expiring only
	// this transaction's Contact leaves the user's ordinary SIP phones and
	// other UAs registered, as RFC 3261 contact-specific removal intends.
	params := make(sip.HeaderParams, 0, len(contact.Params)+1)
	for _, parameter := range contact.Params {
		if !strings.EqualFold(parameter.K, "expires") {
			params = append(params, parameter)
		}
	}
	contact.Params = append(params, sip.HeaderKV{K: "expires", V: "0"})
	req.RemoveHeader("Expires")
	req.RemoveHeader("Contact")
	req.AppendHeader(contact)
	expires := sip.ExpiresHeader(0)
	req.AppendHeader(&expires)
	return nil
}

func (t *RegisterTransaction) Qualify(ctx context.Context) error {
	return t.doRequest(ctx, t.Origin)
}

func (t *RegisterTransaction) doRequest(ctx context.Context, req *sip.Request) error {
	// log := p.getLoggerCtx(ctx, "Register")
	client := t.client
	username, password := t.opts.Username, t.opts.Password
	// Send request and parse response
	// req.SetDestination(*dst)
	req.RemoveHeader("Via")
	res, err := client.Do(ctx, req, sipgo.ClientRequestRegisterBuild)
	if err != nil {
		return fmt.Errorf("fail to get response req=%q : %w", req.StartLine(), err)
	}

	if res.StatusCode == sip.StatusUnauthorized || res.StatusCode == sip.StatusProxyAuthRequired {
		res, err = client.DoDigestAuth(ctx, req, res, sipgo.DigestAuth{
			Username: username,
			Password: password,
		})
		if err != nil {
			return fmt.Errorf("fail to get response req=%q : %w", req.StartLine(), err)
		}
	}

	if res.StatusCode != 200 {
		return &RegisterResponseError{
			RegisterReq: req,
			RegisterRes: res,
			Msg:         res.StartLine(),
		}
	}

	fallback := t.expiry
	if fallback <= 0 {
		fallback = t.opts.Expiry
	}
	expiry, expiryErr := registerResponseExpiry(res, fallback, req.Contact().Clone())
	if expiryErr != nil {
		// Keep the last known good lease. A malformed refresh response must not
		// poison all later responses that omit expiry information.
		t.log.Warn("Failed to parse REGISTER refresh expiry; retaining lease", "error", expiryErr)
		expiry = fallback
	}
	t.expiry = expiry
	return nil
}

func getResponse(ctx context.Context, tx sip.ClientTransaction) (*sip.Response, error) {
	select {
	case <-tx.Done():
		return nil, fmt.Errorf("transaction died")
	case res := <-tx.Responses():
		return res, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
