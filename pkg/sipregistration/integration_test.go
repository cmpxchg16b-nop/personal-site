package sipregistration

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
)

type registrarObservation struct {
	kind    string
	from    string
	contact string
	source  string
}

type loopbackRegistrar struct {
	address              string
	client               *sipgo.Client
	dropSuccessResponses atomic.Bool

	observations chan registrarObservation
}

func newLoopbackRegistrar(t *testing.T) *loopbackRegistrar {
	t.Helper()
	reservation, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Skipf("UDP loopback unavailable: %v", err)
	}
	address := reservation.LocalAddr().String()
	if err := reservation.Close(); err != nil {
		t.Fatal(err)
	}

	ua, err := sipgo.NewUA()
	if err != nil {
		t.Fatal(err)
	}
	server, err := sipgo.NewServer(ua)
	if err != nil {
		t.Fatal(err)
	}
	client, err := sipgo.NewClient(ua)
	if err != nil {
		t.Fatal(err)
	}
	registrar := &loopbackRegistrar{address: address, client: client, observations: make(chan registrarObservation, 64)}
	server.OnRegister(func(request *sip.Request, transaction sip.ServerTransaction) {
		authorization := request.GetHeader("Authorization")
		if authorization == nil || !strings.Contains(authorization.Value(), `username="1001"`) {
			response := sip.NewResponseFromRequest(request, sip.StatusUnauthorized, "Unauthorized", nil)
			response.AppendHeader(sip.NewHeader("WWW-Authenticate", `Digest realm="loopback", nonce="0123456789abcdef", algorithm=MD5`))
			_ = transaction.Respond(response)
			return
		}
		observation := registrarObservation{kind: "register", from: request.From().Address.User, source: request.Source()}
		if contact := request.Contact(); contact != nil {
			observation.contact = contact.Address.String()
		}
		if expires := request.GetHeader("Expires"); expires != nil && strings.TrimSpace(expires.Value()) == "0" {
			observation.kind = "unregister"
		}
		registrar.observations <- observation
		if registrar.dropSuccessResponses.Load() {
			return
		}
		// No Expires in the response: the runtime must use its requested lease.
		_ = transaction.Respond(sip.NewResponseFromRequest(request, 200, "OK", nil))
	})

	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan struct{})
	listenCtx := context.WithValue(ctx, sipgo.ListenReadyCtxKey, sipgo.ListenReadyFuncCtxValue(func(string, string) {
		close(ready)
	}))
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.ListenAndServe(listenCtx, "udp", address) }()
	select {
	case <-ready:
	case err := <-serveDone:
		t.Fatalf("registrar listener: %v", err)
	case <-time.After(time.Second):
		t.Fatal("registrar listener did not become ready")
	}
	t.Cleanup(func() {
		cancel()
		_ = ua.Close()
		select {
		case err := <-serveDone:
			if err != nil && !errors.Is(err, context.Canceled) && !strings.Contains(err.Error(), "closed") {
				t.Errorf("registrar shutdown: %v", err)
			}
		case <-time.After(time.Second):
			t.Error("registrar did not stop")
		}
	})
	return registrar
}

func (r *loopbackRegistrar) waitObservation(t *testing.T, kind string, after int) registrarObservation {
	t.Helper()
	seen := 0
	deadline := time.After(3 * time.Second)
	for {
		select {
		case observation := <-r.observations:
			if observation.kind != kind {
				continue
			}
			seen++
			if seen > after {
				return observation
			}
		case <-deadline:
			t.Fatalf("timed out waiting for %s observation %d", kind, after+1)
		}
	}
}

func (r *loopbackRegistrar) sendMessage(t *testing.T, registration registrarObservation, username string) int {
	t.Helper()
	contact, err := sipUriForLookup(registration.contact)
	if err != nil {
		t.Fatalf("registered Contact URI: %v", err)
	}
	recipient := sip.Uri{Scheme: "sip", User: username, Host: contact.Host, Port: contact.Port}
	request := sip.NewRequest(sip.MESSAGE, recipient)
	request.SetDestination(registration.source)
	from := &sip.FromHeader{Address: sip.Uri{Scheme: "sip", User: "caller", Host: "pbx.example"}, Params: sip.NewParams()}
	from.Params.Add("tag", sip.GenerateTagN(16))
	request.AppendHeader(from)
	request.AppendHeader(&sip.ToHeader{Address: sip.Uri{Scheme: "sip", User: username, Host: "127.0.0.1"}})
	contentType := sip.ContentTypeHeader("text/plain")
	request.AppendHeader(&contentType)
	request.SetBody([]byte("hello"))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	response, err := r.client.Do(ctx, request)
	if err != nil {
		t.Fatalf("MESSAGE to %s: %v", username, err)
	}
	return response.StatusCode
}

func TestDiagoRuntimeRegisterRefreshRouteAndUnregister(t *testing.T) {
	registrar := newLoopbackRegistrar(t)
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	manager := NewManager(logger)
	manager.registrationExpiry = time.Second // SIP Expires is integer seconds on the wire.
	manager.attemptTimeout = 500 * time.Millisecond
	manager.unregisterTimeout = 500 * time.Millisecond
	manager.retryMin = 5 * time.Millisecond
	manager.retryMax = 20 * time.Millisecond
	manager.jitter = func() float64 { return 0.5 }
	t.Cleanup(func() { _ = manager.Close() })
	defer func() {
		if t.Failed() {
			t.Logf("manager credentials: %+v", manager.List("user-alice"))
			t.Logf("SIP logs:\n%s", logs.String())
		}
	}()

	created, err := manager.Create("user-alice", Input{
		Server: registrar.address, Username: "1001", Password: "secret", Transport: TransportUDP, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	first := registrar.waitObservation(t, "register", 0)
	if first.from != "1001" || !strings.Contains(first.contact, "1001") {
		t.Fatalf("first REGISTER identity = %+v", first)
	}
	waitFor(t, "registered status", func() bool {
		got, _ := manager.Get("user-alice", created.ID)
		return got.Registration.State == RegistrationRegistered
	})
	registrar.waitObservation(t, "register", 1) // a real refresh, not only the initial request

	if status := registrar.sendMessage(t, first, "unknown"); status != sip.StatusNotFound {
		t.Fatalf("unmatched MESSAGE status = %d, want 404", status)
	}
	if status := registrar.sendMessage(t, first, "1001"); status != sip.StatusNotImplemented {
		t.Fatalf("matched but unimplemented MESSAGE status = %d, want 501", status)
	}
	if known, ok := manager.LookupAOR("sip:1001@127.0.0.1"); !ok || known.OwnerID != "user-alice" {
		t.Fatalf("AOR owner = %+v, %v", known, ok)
	}

	if err := manager.Delete("user-alice", created.ID); err != nil {
		t.Fatal(err)
	}
	registrar.waitObservation(t, "unregister", 0)
	if _, ok := manager.LookupAOR("sip:1001@127.0.0.1"); ok {
		t.Fatal("AOR remains after delete")
	}
	logText := logs.String()
	for _, expected := range []string{"unmatched inbound SIP signaling", "unknown@127.0.0.1", "inbound SIP signaling resolved to user", "user_alice"} {
		if expected == "user_alice" {
			continue // the structured field is user_id=user-alice, checked below
		}
		if !strings.Contains(logText, expected) {
			t.Errorf("log does not contain %q:\n%s", expected, logText)
		}
	}
	if !strings.Contains(logText, "user_id=user-alice") {
		t.Errorf("resolved log has no user id:\n%s", logText)
	}
	if strings.Contains(logText, "secret") {
		t.Errorf("logs leaked the password:\n%s", logText)
	}
}

func TestDiagoRuntimeUnregistersWhenSuccessResponseIsLost(t *testing.T) {
	registrar := newLoopbackRegistrar(t)
	registrar.dropSuccessResponses.Store(true)
	manager := NewManager(slog.New(slog.DiscardHandler))
	manager.registrationExpiry = time.Second
	manager.attemptTimeout = 150 * time.Millisecond
	manager.unregisterTimeout = 150 * time.Millisecond
	manager.retryMin = time.Hour
	manager.retryMax = time.Hour
	t.Cleanup(func() { _ = manager.Close() })

	created, err := manager.Create("user-alice", Input{
		Server: registrar.address, Username: "1001", Password: "secret", Transport: TransportUDP, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	registrar.waitObservation(t, "register", 0)
	// The registrar accepted the binding but its 200 was lost. Disabling the
	// credential must still expire this UA's Contact on the wire.
	if err := manager.Delete("user-alice", created.ID); err != nil {
		t.Fatal(err)
	}
	registrar.waitObservation(t, "unregister", 0)
}

type blackholeUDP struct {
	address  string
	requests chan string
}

func newBlackholeUDP(t *testing.T) *blackholeUDP {
	t.Helper()
	connection, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Skipf("UDP loopback unavailable: %v", err)
	}
	blackhole := &blackholeUDP{address: connection.LocalAddr().String(), requests: make(chan string, 32)}
	done := make(chan struct{})
	go func() {
		defer close(done)
		buffer := make([]byte, 65535)
		for {
			count, _, err := connection.ReadFromUDP(buffer)
			if err != nil {
				return
			}
			blackhole.requests <- string(append([]byte(nil), buffer[:count]...))
		}
	}()
	t.Cleanup(func() {
		_ = connection.Close()
		<-done
	})
	return blackhole
}

func (b *blackholeUDP) next(t *testing.T, timeout time.Duration) string {
	t.Helper()
	select {
	case request := <-b.requests:
		return request
	case <-time.After(timeout):
		t.Fatal("timed out waiting for UDP REGISTER")
		return ""
	}
}

func rawSIPHeader(message, name string) string {
	for _, line := range strings.Split(message, "\r\n") {
		key, value, found := strings.Cut(line, ":")
		if found && strings.EqualFold(key, name) {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func TestDiagoRuntimeRegisterTimeoutTerminatesTransaction(t *testing.T) {
	blackhole := newBlackholeUDP(t)
	manager := NewManager(slog.New(slog.DiscardHandler))
	credential, err := normalizeInput(Input{
		Server: blackhole.address, Username: "1001", Password: "secret", Transport: TransportUDP, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	credential.OwnerID = "user-alice"
	credential.ID = "blackhole"
	rt, err := newDiagoRuntime(credential, manager)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rt.Close() })

	firstVia := ""
	for attempt := 0; attempt < 2; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
		err := rt.Register(ctx)
		cancel()
		if err == nil {
			t.Fatal("blackhole REGISTER unexpectedly succeeded")
		}
		request := blackhole.next(t, time.Second)
		via := rawSIPHeader(request, "Via")
		if via == "" {
			t.Fatalf("REGISTER attempt %d has no Via", attempt+1)
		}
		if attempt == 0 {
			firstVia = via
			continue
		}
		if via == firstVia {
			t.Fatal("second REGISTER reused the timed-out transaction's Via")
		}
	}
	select {
	case request := <-blackhole.requests:
		t.Fatalf("REGISTER was retransmitted after its attempt timed out:\n%s", request)
	case <-time.After(650 * time.Millisecond):
	}
}

func TestSourceAddressForIPv6Literal(t *testing.T) {
	if _, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("::1"), Port: 0}); err != nil {
		t.Skipf("IPv6 loopback unavailable: %v", err)
	}
	if got := sourceAddressFor("[::1]", 5060); got != "::1" {
		t.Fatalf("sourceAddressFor([::1]) = %q", got)
	}
}

func ExampleManager_LookupAOR() {
	manager := NewManager(slog.New(slog.DiscardHandler))
	defer manager.Close()
	credential, _ := manager.Create("user-42", Input{Server: "pbx.example", Username: "1001", Enabled: false})
	owner, found := manager.LookupAOR(credential.SIPURI)
	fmt.Println(owner.OwnerID, found)
	// Output: user-42 true
}
