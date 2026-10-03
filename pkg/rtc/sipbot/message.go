package sipbot

// This file is the sip bot's pager-mode messaging (docs/sip-bot.md
// §7.3) — RFC 3428 SIP MESSAGE, bridged both ways, dialog-free:
//
//   - OUTBOUND (/send <aor> <message>): one MESSAGE transaction on the
//     account's own client, carrying the account's identity and leaving
//     through the account's registrar — the REGISTER's identity stamping
//     and §7.1's outbound-proxy routing, verbatim. The body is
//     text/plain; charset=utf-8, capped at the configured
//     MaxMessageBytes.
//   - INBOUND: a MESSAGE arriving at a registered account's socket is
//     rendered and forwarded into the owner's chat over the
//     unsolicited-send path. Two media types are supported: text/plain
//     (verbatim, attributed to the SIP From) and a reduced message/cpim
//     (RFC 3862 — the inner From/To preserved, attachments and
//     signatures stripped; encrypted CPIM is not supported). Anything
//     else is dropped silently and answered 415.
//
// Both directions are page-mode: an in-dialog MESSAGE (RFC 3428 permits
// one mid-call) is treated exactly like an out-of-dialog one — forwarded
// and answered, never dialog-matched.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/textproto"
	"strings"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"

	"personal-site/pkg/models/ss"
)

// DefaultMaxMessageBytes is the default /send body-size cap (the
// Configuration's zero value) — docs/sip-bot.md §10 carries the
// arithmetic: IPv6's minimum link MTU (1280) minus the IPv6+UDP headers
// (48) leaves 1232 bytes for the whole SIP datagram, and the presumed
// SIP header block (~430 bytes, a digest-challenged retry's
// Authorization included) brings the body budget to ~800.
const DefaultMaxMessageBytes = 800

// maxMaxMessageBytes bounds the configured cap: the largest UDP payload
// (65535 − 8) — past it no IP transport can carry one datagram, so a
// larger cap is a misconfiguration by construction.
const maxMaxMessageBytes = 65507

// ParseMaxMessageBytes validates the raw maxMessageBytes configuration
// value: zero (unset) selects DefaultMaxMessageBytes, anything else must
// be a positive byte count no larger than a UDP datagram can carry.
func ParseMaxMessageBytes(v int) (int, error) {
	if v == 0 {
		return DefaultMaxMessageBytes, nil
	}
	if v < 1 {
		return 0, fmt.Errorf("maxMessageBytes must be positive, got %d", v)
	}
	if v > maxMaxMessageBytes {
		return 0, fmt.Errorf("maxMessageBytes %d exceeds the largest UDP payload (%d)", v, maxMaxMessageBytes)
	}
	return v, nil
}

// messageTimeout bounds a /send's MESSAGE round trip — the handler
// answers from its outcome (the /register discipline), so the wait must
// not stall the peer's channel goroutine longer than this.
const messageTimeout = 5 * time.Second

// messageContentType is the outbound MESSAGE's body type.
const messageContentType = "text/plain; charset=utf-8"

// sendMessage sends text to target as one pager-mode SIP MESSAGE (RFC
// 3428) on the account's identity: the Request-URI is the target's (a
// bare user is completed with the account's domain, the dial path's
// rule), but the datagram leaves through the account's own SIP server —
// SetDestination(proxyAddr()), the outbound-proxy routing of
// docs/sip-bot.md §7.1 (the fallback — resolving the target's own host —
// would skip the registrar AND risk the socket-family mismatch
// bindHostFor exists to prevent). The From is pre-stamped with the AOR
// and a fresh tag: sipgo's client fills only ABSENT headers, and its
// default From would be the socket's address, not the AOR (the
// REGISTER's identity discipline). A 401/407 challenge is answered with
// the account's digest credential and retried — the destination, a
// request field, persists across the retry. The wait is bounded by
// messageTimeout; the returned error carries the proxy's verdict.
func (a *account) sendMessage(ctx context.Context, target, text string) error {
	uri := sip.Uri{}
	to := target
	if !hasHost(to) {
		to += "@" + a.session.hostPort()
	}
	if err := sip.ParseUri(withSipScheme(to), &uri); err != nil {
		return fmt.Errorf("bad recipient address %q: %w", to, err)
	}
	client, err := a.dg.Client("")
	if err != nil {
		return fmt.Errorf("the account's SIP client: %w", err)
	}
	req := sip.NewRequest(sip.MESSAGE, uri)
	req.SetDestination(a.session.proxyAddr())
	from := &sip.FromHeader{
		DisplayName: a.session.Username,
		Address:     sip.Uri{User: a.session.Username, Host: a.session.Host, Port: a.session.Port},
		Params:      sip.NewParams(),
	}
	from.Params.Add("tag", sip.GenerateTagN(16))
	req.AppendHeader(from)
	req.AppendHeader(&sip.ToHeader{Address: *uri.Clone()})
	contentType := sip.ContentTypeHeader(messageContentType)
	req.AppendHeader(&contentType)
	req.SetBody([]byte(text))

	ctx, cancel := context.WithTimeout(ctx, messageTimeout)
	defer cancel()
	res, err := client.Do(ctx, req)
	if err != nil {
		return fmt.Errorf("the MESSAGE transaction failed: %w", err)
	}
	if res.StatusCode == sip.StatusUnauthorized || res.StatusCode == sip.StatusProxyAuthRequired {
		res, err = client.DoDigestAuth(ctx, req, res, sipgo.DigestAuth{
			Username: a.session.Username,
			Password: a.session.Password,
		})
		if err != nil {
			return fmt.Errorf("the authenticated MESSAGE transaction failed: %w", err)
		}
	}
	if res.StatusCode >= 200 && res.StatusCode < 300 {
		a.logger.Info("sipbot: MESSAGE sent", "aor", a.session.AddressOfRecord, "to", to, "status", res.StatusCode)
		return nil
	}
	return fmt.Errorf("SIP %d %s", res.StatusCode, res.Reason)
}

// inboundMessage is the account's inbound-MESSAGE callback
// (sipStack.open's onMessage): one closure per account, binding the
// owning peer — inboundServe's exact shape.
func (h *sipHandler) inboundMessage(peer ss.SubscriberId) func(req *sip.Request, tx sip.ServerTransaction) error {
	return func(req *sip.Request, tx sip.ServerTransaction) error {
		return h.serveMessage(peer, req, tx)
	}
}

// serveMessage handles one inbound SIP MESSAGE: render the body by its
// media type and deliver it into the owner's chat over the
// unsolicited-send path (WriterFor — the session-start announcement's
// twin). A delivered message is a 200 OK; a peer with no live session a
// 480; a malformed CPIM body a 400; and any unsupported media type —
// encrypted CPIM included — a 415 listing the supported ones, with the
// browser never bothered. It runs on the account's serve goroutine and
// must stay fast: all of it is in-memory work plus one channel write.
func (h *sipHandler) serveMessage(peer ss.SubscriberId, req *sip.Request, tx sip.ServerTransaction) error {
	respond := func(code int, phrase string) error {
		res := sip.NewResponseFromRequest(req, code, phrase, nil)
		if code == sip.StatusUnsupportedMediaType {
			res.AppendHeader(sip.NewHeader("Accept", "text/plain, message/cpim"))
		}
		return tx.Respond(res)
	}
	var text string
	switch mediaType := contentTypeOf(req); mediaType {
	case "text/plain":
		text = fmt.Sprintf("Message from %s:\n\n%s", messageSender(req), req.Body())
	case "message/cpim":
		rendered, err := renderCPIM(req.Body(), messageSender(req))
		switch {
		case errors.Is(err, errCPIMUnsupported):
			h.logger.Info("sipbot: inbound MESSAGE with an unsupported cpim body, dropped", "peer", peer, "err", err)
			return respond(sip.StatusUnsupportedMediaType, "Unsupported Media Type")
		case err != nil:
			h.logger.Info("sipbot: inbound MESSAGE with a malformed cpim body", "peer", peer, "err", err)
			return respond(sip.StatusBadRequest, "Bad Request")
		}
		text = rendered
	default:
		h.logger.Info("sipbot: inbound MESSAGE of an unsupported media type, dropped", "peer", peer, "contentType", mediaType)
		return respond(sip.StatusUnsupportedMediaType, "Unsupported Media Type")
	}
	if _, err := h.writers.WriterFor(peer).Reply(text); err != nil {
		h.logger.Warn("sipbot: the inbound MESSAGE could not reach the chat", "peer", peer, "err", err)
		return respond(sip.StatusTemporarilyUnavailable, "Temporarily Unavailable")
	}
	h.logger.Info("sipbot: inbound MESSAGE forwarded to the chat", "peer", peer)
	return respond(sip.StatusOK, "OK")
}

// contentTypeOf is the MESSAGE body's declared media type, lowercased;
// an absent Content-Type (RFC 3261 assumes application/sdp — unsupported
// here either way) or an unparseable one reads as empty, which the
// dispatch treats as unsupported.
func contentTypeOf(req *sip.Request) string {
	ct := req.ContentType()
	if ct == nil {
		return ""
	}
	mediaType, _, err := mime.ParseMediaType(ct.Value())
	if err != nil {
		return ""
	}
	return strings.ToLower(mediaType)
}

// messageSender renders the MESSAGE's SIP From for the chat line —
// "sip:user@host"; the degenerate wire shapes read as an unknown sender
// (callerOf's guard, mirrored).
func messageSender(req *sip.Request) string {
	from := req.From()
	if from == nil || from.Address.User == "" {
		return "an unknown sender"
	}
	return "sip:" + from.Address.User + "@" + from.Address.Host
}

// cpimMaxNest bounds how deep the reduced CPIM parser follows nested
// multipart entities — a multipart/signed wrapping a multipart/mixed is
// the realistic maximum.
const cpimMaxNest = 2

// errCPIMUnsupported marks a CPIM entity with nothing forwardable:
// non-text content, an encrypted wrapper, or a part whose transfer
// encoding is not decoded (the handler's 415 cue — a malformed body is
// any OTHER error, a 400).
var errCPIMUnsupported = errors.New("no forwardable content")

// renderCPIM renders a reduced message/cpim body (RFC 3862) as chat
// plaintext: the CPIM headers' From and To are the IM-layer identity and
// are kept verbatim (fallbackFrom — the SIP From — stands in for a
// missing From), and the encapsulated MIME entity contributes its first
// text part — which is how a multipart/mixed's attachments and a
// multipart/signed's signature are stripped. base64 and quoted-printable
// transfer encodings are decoded; charsets pass through untouched.
// Encrypted CPIM (application/pkcs7-mime, multipart/encrypted) and
// bodies with no text part at all answer errCPIMUnsupported.
func renderCPIM(body []byte, fallbackFrom string) (string, error) {
	hdr, rest, err := readMIMEBlock(body)
	if err != nil {
		return "", fmt.Errorf("the cpim headers: %w", err)
	}
	from, to := hdr.Get("From"), hdr.Get("To")
	if from == "" {
		from = fallbackFrom
	}
	text, err := cpimEntity(rest, cpimMaxNest)
	if err != nil {
		return "", err
	}
	if to != "" {
		return fmt.Sprintf("Message from %s to %s:\n\n%s", from, to, text), nil
	}
	return fmt.Sprintf("Message from %s:\n\n%s", from, text), nil
}

// readMIMEBlock splits one MIME header block off b, answering the
// headers and everything after the blank line.
func readMIMEBlock(b []byte) (textproto.MIMEHeader, []byte, error) {
	tp := textproto.NewReader(bufio.NewReader(bytes.NewReader(b)))
	hdr, err := tp.ReadMIMEHeader()
	if err != nil {
		return nil, nil, err
	}
	rest, err := io.ReadAll(tp.R)
	if err != nil {
		return nil, nil, err
	}
	return hdr, rest, nil
}

// cpimEntity renders one MIME entity off the wire (its header block,
// then its body). An entity whose header block does not parse at all is
// read leniently as a headerless text/plain body — the MIME default —
// rather than rejected: the bridge's job is delivery, not validation.
func cpimEntity(body []byte, nest int) (string, error) {
	hdr, rest, err := readMIMEBlock(body)
	if err != nil {
		hdr, rest = textproto.MIMEHeader{}, body
	}
	return cpimTypedBody(hdr, rest, nest)
}

// cpimTypedBody answers an entity body's first text part by its declared
// content type: a text/* body decodes by its Content-Transfer-Encoding;
// a multipart entity (encrypted excluded) is searched part by part, the
// parts' headers already parsed, recursing up to nest levels.
func cpimTypedBody(hdr textproto.MIMEHeader, body []byte, nest int) (string, error) {
	mediaType := "text/plain" // the MIME default when Content-Type is absent
	var params map[string]string
	if ct := hdr.Get("Content-Type"); ct != "" {
		mt, p, err := mime.ParseMediaType(ct)
		if err != nil {
			return "", fmt.Errorf("the mime entity's content type %q: %w", ct, err)
		}
		mediaType, params = strings.ToLower(mt), p
	}
	if strings.HasPrefix(mediaType, "multipart/") {
		if nest <= 0 || mediaType == "multipart/encrypted" {
			return "", errCPIMUnsupported
		}
		boundary := params["boundary"]
		if boundary == "" {
			return "", fmt.Errorf("multipart without a boundary")
		}
		mr := multipart.NewReader(bytes.NewReader(body), boundary)
		for {
			part, err := mr.NextPart()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return "", fmt.Errorf("the multipart's part: %w", err)
			}
			pbody, err := io.ReadAll(part)
			if err != nil {
				return "", fmt.Errorf("the multipart's part body: %w", err)
			}
			text, err := cpimTypedBody(textproto.MIMEHeader(part.Header), pbody, nest-1)
			if errors.Is(err, errCPIMUnsupported) {
				continue // an attachment, a signature — stripped
			}
			return text, err
		}
		return "", errCPIMUnsupported
	}
	if !strings.HasPrefix(mediaType, "text/") {
		return "", errCPIMUnsupported
	}
	decoded, err := decodeTransfer(body, hdr.Get("Content-Transfer-Encoding"))
	if err != nil {
		return "", err
	}
	return strings.TrimRight(string(decoded), "\r\n"), nil
}

// decodeTransfer applies a Content-Transfer-Encoding: base64 and
// quoted-printable are decoded; 7bit/8bit/binary (and an absent
// encoding) pass through; anything else is unsupported.
func decodeTransfer(body []byte, encoding string) ([]byte, error) {
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "", "7bit", "8bit", "binary":
		return body, nil
	case "base64":
		decoded, err := base64.StdEncoding.DecodeString(strings.Map(func(r rune) rune {
			if r == '\r' || r == '\n' {
				return -1
			}
			return r
		}, string(body)))
		if err != nil {
			return nil, fmt.Errorf("the base64 body: %w", err)
		}
		return decoded, nil
	case "quoted-printable":
		decoded, err := io.ReadAll(quotedprintable.NewReader(bytes.NewReader(body)))
		if err != nil {
			return nil, fmt.Errorf("the quoted-printable body: %w", err)
		}
		return decoded, nil
	}
	return nil, errCPIMUnsupported
}
