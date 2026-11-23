package sipregistration

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/emiago/diago"
	"github.com/emiago/diago/media"
	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
)

type testOperationRuntime struct {
	*fakeRuntime
	ctx    context.Context
	cancel context.CancelFunc
}

func newTestOperationRuntime() *testOperationRuntime {
	ctx, cancel := context.WithCancel(context.Background())
	return &testOperationRuntime{fakeRuntime: &fakeRuntime{expiry: time.Minute}, ctx: ctx, cancel: cancel}
}

func (r *testOperationRuntime) OperationContext() context.Context { return r.ctx }
func (r *testOperationRuntime) MediaBindIP() net.IP               { return net.ParseIP("127.0.0.1") }
func (r *testOperationRuntime) StopOperations()                   { r.cancel() }
func (r *testOperationRuntime) Invite(context.Context, Credential, sip.Uri, *media.MediaSession, func(int)) (*diago.DialogClientSession, error) {
	return nil, errors.New("unexpected INVITE")
}
func (r *testOperationRuntime) SendMessage(context.Context, Credential, sip.Uri, string) error {
	return nil
}

func newOperationLeaseEntry(t *testing.T, rt runtime) (*Manager, *entry) {
	t.Helper()
	manager := NewManager(slog.New(slog.DiscardHandler))
	credential, err := normalizeInput(testInput("pbx.example:5070", "1001", true))
	if err != nil {
		t.Fatal(err)
	}
	credential.OwnerID = "user-alice"
	credential.ID = "credential-one"
	expiresAt := time.Now().Add(time.Hour)
	credential.Registration = Registration{State: RegistrationRegistered, ExpiresAt: &expiresAt}
	key := credentialKey{ownerID: credential.OwnerID, id: credential.ID}
	current := &entry{key: key, credential: credential, revision: 7, runtime: rt}
	manager.entries[key] = current
	manager.aors[credential.SIPURI] = key
	t.Cleanup(func() { _ = manager.Close() })
	return manager, current
}

func TestAcquireOperationChecksOwnerRuntimeAndBinding(t *testing.T) {
	operations := newTestOperationRuntime()
	manager, current := newOperationLeaseEntry(t, operations)

	if _, _, _, err := manager.acquireOperation("user-bob", current.credential.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("wrong-owner acquisition error = %v, want ErrNotFound", err)
	}
	if _, _, _, err := manager.acquireOperation("user-alice", "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing acquisition error = %v, want ErrNotFound", err)
	}
	credential, leased, revision, err := manager.acquireOperation("user-alice", current.credential.ID)
	if err != nil {
		t.Fatal(err)
	}
	if credential.ID != current.credential.ID || credential.Password != "secret" || leased != operations || revision != 7 {
		t.Fatalf("lease = credential %+v, runtime %p, revision %d", credential, leased, revision)
	}

	// A failed refresh does not invalidate an earlier, unexpired binding.
	current.credential.Registration.State = RegistrationFailed
	if _, _, _, err := manager.acquireOperation("user-alice", current.credential.ID); err != nil {
		t.Fatalf("unexpired failed-refresh lease: %v", err)
	}
	current.credential.Registration.State = RegistrationRegistering
	if _, _, _, err := manager.acquireOperation("user-alice", current.credential.ID); err != nil {
		t.Fatalf("unexpired refreshing lease: %v", err)
	}

	tests := []struct {
		name  string
		apply func()
	}{
		{name: "disabled", apply: func() { current.credential.Enabled = false }},
		{name: "no runtime", apply: func() { current.runtime = nil }},
		{name: "unsupported runtime", apply: func() { current.runtime = &fakeRuntime{} }},
		{name: "no binding", apply: func() { current.credential.Registration.ExpiresAt = nil }},
		{name: "expired", apply: func() {
			expiresAt := time.Now().Add(-time.Second)
			current.credential.Registration.ExpiresAt = &expiresAt
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			savedCredential := cloneCredential(current.credential)
			savedRuntime := current.runtime
			test.apply()
			if _, _, _, err := manager.acquireOperation("user-alice", current.credential.ID); !errors.Is(err, ErrOffline) {
				t.Fatalf("acquisition error = %v, want ErrOffline", err)
			}
			current.credential = savedCredential
			current.runtime = savedRuntime
		})
	}
}

func TestLeaseForRuntimeRequiresExactCurrentPointer(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	rt := &diagoRuntime{operationCtx: ctx, stopOperations: cancel}
	manager, current := newOperationLeaseEntry(t, rt)

	credential, operations, revision, ok := manager.leaseForRuntime(rt)
	if !ok || credential.ID != current.credential.ID || operations != rt || revision != current.revision {
		t.Fatalf("runtime lease = %+v, %p, %d, %v", credential, operations, revision, ok)
	}
	if _, _, _, ok := manager.leaseForRuntime(nil); ok {
		t.Fatal("nil runtime acquired a lease")
	}
	replacement := &diagoRuntime{}
	if _, _, _, ok := manager.leaseForRuntime(replacement); ok {
		t.Fatal("unattached runtime acquired a lease")
	}
	current.runtime = replacement
	if _, _, _, ok := manager.leaseForRuntime(rt); ok {
		t.Fatal("replaced runtime acquired the new generation")
	}
}

func TestParseSIPTarget(t *testing.T) {
	udp, err := normalizeInput(testInput("Registrar.Example:5070", "1001", true))
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name        string
		raw         string
		wantUser    string
		wantHost    string
		wantPort    int
		wantScheme  string
		wantDisplay string
	}{
		{name: "bare user", raw: "  2002  ", wantUser: "2002", wantHost: "registrar.example", wantScheme: "sip", wantDisplay: "sip:2002@registrar.example"},
		{name: "bare user with scheme", raw: "sip:2002", wantUser: "2002", wantHost: "registrar.example", wantScheme: "sip", wantDisplay: "sip:2002@registrar.example"},
		{name: "user host", raw: "2002@Example.com", wantUser: "2002", wantHost: "Example.com", wantScheme: "sip", wantDisplay: "sip:2002@Example.com"},
		{name: "scheme port and matching transport", raw: "SIP:2002@example.com:5062;transport=UDP", wantUser: "2002", wantHost: "example.com", wantPort: 5062, wantScheme: "sip", wantDisplay: "sip:2002@example.com"},
		{name: "other URI parameter", raw: "2002@example.com;foo=bar", wantUser: "2002", wantHost: "example.com", wantScheme: "sip", wantDisplay: "sip:2002@example.com"},
		{name: "IPv6", raw: "2002@[::1]", wantUser: "2002", wantHost: "[::1]", wantScheme: "sip", wantDisplay: "sip:2002@[::1]"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			uri, display, err := parseSIPTarget(udp, test.raw)
			if err != nil {
				t.Fatal(err)
			}
			if uri.User != test.wantUser || uri.Host != test.wantHost || uri.Port != test.wantPort || uri.Scheme != test.wantScheme || display != test.wantDisplay {
				t.Fatalf("parseSIPTarget(%q) = %+v, %q", test.raw, uri, display)
			}
		})
	}

	invalid := []string{
		"",
		"2002 @example.com",
		"2002@example.com\nVia: injected",
		"2002@example.com?subject=hello",
		"sip:@example.com",
		"sip:2002@",
		"sip:2002@example.com:not-a-port",
		"sip:2002@example.com:70000",
		"tel:2002",
		"sips:2002@example.com",
		"sip:2002@example.com;transport=tcp",
		"sip:2002:secret@example.com",
		"sip://2002@example.com",
		strings.Repeat("a", maxSIPTargetBytes+1),
		string([]byte{0xff}),
	}
	for _, raw := range invalid {
		t.Run("invalid_"+raw, func(t *testing.T) {
			if _, _, err := parseSIPTarget(udp, raw); !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("parseSIPTarget(%q) error = %v, want ErrInvalidInput", raw, err)
			}
		})
	}

	tlsCredential := udp
	tlsCredential.Transport = TransportTLS
	uri, display, err := parseSIPTarget(tlsCredential, "sips:2002@example.com;transport=tls")
	if err != nil || uri.Scheme != "sips" || display != "sip:2002@example.com" {
		t.Fatalf("TLS target = %+v, %q, %v", uri, display, err)
	}
	for _, raw := range []string{"sip:2002@example.com", "sips:2002@example.com;transport=udp"} {
		if _, _, err := parseSIPTarget(tlsCredential, raw); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("TLS parseSIPTarget(%q) error = %v", raw, err)
		}
	}
}

func TestValidateOutboundText(t *testing.T) {
	valid := []string{"hello", " ", strings.Repeat("x", maxOutboundTextBytes), "こんにちは"}
	for _, text := range valid {
		if err := validateOutboundText(text); err != nil {
			t.Fatalf("valid text rejected: %v", err)
		}
	}
	invalid := []string{"", string([]byte{0xff}), strings.Repeat("x", maxOutboundTextBytes+1)}
	for _, text := range invalid {
		if err := validateOutboundText(text); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("invalid text error = %v, want ErrInvalidInput", err)
		}
	}
}

func TestDiagoRuntimeInviteClosesMediaBeforeDialogOwnership(t *testing.T) {
	tests := []struct {
		name       string
		newRuntime func(*testing.T) *diagoRuntime
		ctx        func() context.Context
		wantErr    error
	}{
		{
			name: "operation context",
			newRuntime: func(*testing.T) *diagoRuntime {
				return &diagoRuntime{operationCtx: context.Background()}
			},
			ctx: func() context.Context {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				return ctx
			},
			wantErr: context.Canceled,
		},
		{
			name: "registrar destination",
			newRuntime: func(*testing.T) *diagoRuntime {
				return &diagoRuntime{operationCtx: context.Background()}
			},
			ctx:     context.Background,
			wantErr: ErrOffline,
		},
		{
			name: "nil diago",
			newRuntime: func(*testing.T) *diagoRuntime {
				return &diagoRuntime{operationCtx: context.Background(), proxyHost: "127.0.0.1", proxyPort: 5060}
			},
			ctx:     context.Background,
			wantErr: ErrOffline,
		},
		{
			name: "dialog creation",
			newRuntime: func(t *testing.T) *diagoRuntime {
				ua, err := sipgo.NewUA()
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = ua.Close() })
				return &diagoRuntime{
					operationCtx: context.Background(),
					dg:           diago.NewDiago(ua),
					proxyHost:    "127.0.0.1",
					proxyPort:    5060,
				}
			},
			ctx: context.Background,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rt := test.newRuntime(t)
			session, err := media.NewMediaSession(net.ParseIP("127.0.0.1"), 0)
			if err != nil {
				t.Fatal(err)
			}
			dialog, err := rt.Invite(test.ctx(), Credential{Transport: TransportUDP}, sip.Uri{User: "2002", Host: "127.0.0.1"}, session, nil)
			if err == nil {
				t.Fatal("INVITE unexpectedly succeeded")
			}
			if test.wantErr != nil && !errors.Is(err, test.wantErr) {
				t.Fatalf("INVITE error = %v, want %v", err, test.wantErr)
			}
			if dialog != nil {
				t.Fatal("failed INVITE returned a dialog before ownership transfer")
			}
			if err := session.StopRTP(1, 0); !errors.Is(err, net.ErrClosed) {
				t.Fatalf("media socket remains open after early INVITE error: %v", err)
			}
		})
	}
}

func TestDiagoRuntimeInviteDialogFailureClosesMediaOnce(t *testing.T) {
	serverCtx, stopServer := context.WithCancel(context.Background())
	defer stopServer()
	serverUA, err := sipgo.NewUA(sipgo.WithUserAgent("server"))
	if err != nil {
		t.Fatal(err)
	}
	defer serverUA.Close()
	server := diago.NewDiago(serverUA, diago.WithTransport(diago.Transport{Transport: "udp", BindHost: "127.0.0.1", BindPort: 0}))
	if err := server.ServeBackground(serverCtx, func(dialog *diago.DialogServerSession) {
		_ = dialog.Respond(sip.StatusForbidden, "Forbidden", nil)
	}); err != nil {
		t.Fatal(err)
	}
	ports := serverUA.TransportLayer().ListenPorts("udp")
	if len(ports) != 1 {
		t.Fatalf("server UDP ports = %v, want one", ports)
	}

	manager := NewManager(slog.New(slog.DiscardHandler))
	credential, err := normalizeInput(testInput(net.JoinHostPort("127.0.0.1", strconv.Itoa(ports[0])), "1001", true))
	if err != nil {
		t.Fatal(err)
	}
	runtimeValue, err := newDiagoRuntime(credential, manager)
	if err != nil {
		t.Fatal(err)
	}
	rt := runtimeValue.(*diagoRuntime)
	t.Cleanup(func() { _ = rt.Close() })
	target, _, err := parseSIPTarget(credential, "2002@target.invalid")
	if err != nil {
		t.Fatal(err)
	}
	session, err := media.NewMediaSession(net.ParseIP("127.0.0.1"), 0)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	dialog, err := rt.Invite(ctx, credential, target, session, nil)
	if err == nil {
		t.Fatal("INVITE unexpectedly succeeded")
	}
	if !strings.Contains(err.Error(), "403 Forbidden") {
		t.Fatalf("INVITE error = %v, want 403 response", err)
	}
	if errors.Is(err, net.ErrClosed) {
		t.Fatalf("INVITE error contains a duplicate media close: %v", err)
	}
	if dialog != nil {
		t.Fatal("failed INVITE returned a dialog after closing it")
	}
	if err := session.StopRTP(1, 0); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("media socket remains open after dialog INVITE error: %v", err)
	}
}

func TestStopOperationsCancelsOutboundMessage(t *testing.T) {
	blackhole := newBlackholeUDP(t)
	manager := NewManager(slog.New(slog.DiscardHandler))
	credential, err := normalizeInput(testInput(blackhole.address, "1001", true))
	if err != nil {
		t.Fatal(err)
	}
	runtimeValue, err := newDiagoRuntime(credential, manager)
	if err != nil {
		t.Fatal(err)
	}
	rt := runtimeValue.(*diagoRuntime)
	t.Cleanup(func() { _ = rt.Close() })
	target, _, err := parseSIPTarget(credential, "2002@target.invalid")
	if err != nil {
		t.Fatal(err)
	}

	sent := make(chan error, 1)
	go func() { sent <- rt.SendMessage(context.Background(), credential, target, "hello") }()
	select {
	case request := <-blackhole.requests:
		if !strings.HasPrefix(request, "MESSAGE ") {
			t.Fatalf("request = %q, want MESSAGE", request)
		}
	case <-time.After(time.Second):
		t.Fatal("outbound MESSAGE did not start")
	}
	rt.StopOperations()
	select {
	case <-rt.OperationContext().Done():
	case <-time.After(time.Second):
		t.Fatal("operation context was not canceled")
	}
	select {
	case err := <-sent:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("MESSAGE error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("StopOperations did not cancel MESSAGE")
	}
	if err := rt.SendMessage(context.Background(), credential, target, "hello"); !errors.Is(err, context.Canceled) {
		t.Fatalf("new MESSAGE after StopOperations error = %v", err)
	}
	rt.StopOperations() // idempotent
}
