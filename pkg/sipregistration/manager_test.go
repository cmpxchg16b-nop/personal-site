package sipregistration

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"testing"
	"time"
)

type fakeRuntime struct {
	name   string
	events chan<- string

	mu             sync.Mutex
	expiry         time.Duration
	registerErr    error
	refreshErr     error
	registerCalls  int
	refreshCalls   int
	unregisterCall int
	closeCalls     int

	unregisterStarted chan struct{}
	unregisterRelease chan struct{}
}

func (r *fakeRuntime) Register(context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.registerCalls++
	if r.events != nil {
		r.events <- r.name + ".register"
	}
	return r.registerErr
}

func (r *fakeRuntime) Refresh(context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.refreshCalls++
	return r.refreshErr
}

func (r *fakeRuntime) Unregister(ctx context.Context) error {
	r.mu.Lock()
	r.unregisterCall++
	if r.events != nil {
		r.events <- r.name + ".unregister"
	}
	started := r.unregisterStarted
	release := r.unregisterRelease
	r.mu.Unlock()
	if started != nil {
		started <- struct{}{}
	}
	if release != nil {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-release:
		}
	}
	return nil
}

func (r *fakeRuntime) Expiry() time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.expiry
}

func (r *fakeRuntime) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closeCalls++
	return nil
}

func (r *fakeRuntime) setRegisterError(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.registerErr = err
}

func (r *fakeRuntime) counts() (registers, refreshes, unregisters, closes int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.registerCalls, r.refreshCalls, r.unregisterCall, r.closeCalls
}

type fakeRuntimeFactory struct {
	mu          sync.Mutex
	runtimes    []*fakeRuntime
	events      chan string
	registerErr error
}

func (f *fakeRuntimeFactory) new(Credential, *Manager) (runtime, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rt := &fakeRuntime{name: fmt.Sprintf("rt%d", len(f.runtimes)+1), expiry: 80 * time.Millisecond, events: f.events, registerErr: f.registerErr}
	f.runtimes = append(f.runtimes, rt)
	return rt, nil
}

func (f *fakeRuntimeFactory) all() []*fakeRuntime {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*fakeRuntime(nil), f.runtimes...)
}

func newFakeManager(t *testing.T) (*Manager, *fakeRuntimeFactory) {
	t.Helper()
	m := NewManager(slog.New(slog.DiscardHandler))
	factory := &fakeRuntimeFactory{}
	m.newRuntime = factory.new
	m.attemptTimeout = 100 * time.Millisecond
	m.unregisterTimeout = 100 * time.Millisecond
	m.retryMin = 5 * time.Millisecond
	m.retryMax = 20 * time.Millisecond
	m.jitter = func() float64 { return 0.5 }
	t.Cleanup(func() { _ = m.Close() })
	return m, factory
}

func testInput(server, username string, enabled bool) Input {
	return Input{Server: server, Username: username, Password: "secret", Transport: TransportUDP, Enabled: enabled}
}

func waitFor(t *testing.T, description string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", description)
}

func TestManagerLifecycleAndAOROwnership(t *testing.T) {
	m, factory := newFakeManager(t)
	events := make(chan string, 32)
	factory.events = events

	created, err := m.Create("user-alice", testInput("PBX.Example:5070", "Alice", true))
	if err != nil {
		t.Fatal(err)
	}
	if created.SIPURI != "sip:Alice@pbx.example" || created.Registration.State != RegistrationRegistering {
		t.Fatalf("created = %+v", created)
	}
	if _, err := m.Create("user-bob", testInput("pbx.example", "Alice", false)); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate cross-user AOR error = %v, want ErrConflict", err)
	}
	if got := m.List("user-bob"); len(got) != 0 {
		t.Fatalf("other user's list = %+v", got)
	}

	waitFor(t, "first registration", func() bool {
		got, _ := m.Get("user-alice", created.ID)
		return got.Registration.State == RegistrationRegistered
	})
	waitFor(t, "lease refresh", func() bool {
		_, refreshes, _, _ := factory.all()[0].counts()
		return refreshes > 0
	})
	if owner, ok := m.LookupAOR("sip:Alice@PBX.EXAMPLE;transport=udp"); !ok || owner.OwnerID != "user-alice" {
		t.Fatalf("AOR lookup = %+v, %v", owner, ok)
	}

	updated, err := m.Update("user-alice", created.ID, testInput("new.example", "Alice", true))
	if err != nil {
		t.Fatal(err)
	}
	if updated.SIPURI != "sip:Alice@new.example" || updated.Password != "secret" {
		t.Fatalf("updated = %+v", updated)
	}
	waitFor(t, "replacement runtime", func() bool { return len(factory.all()) == 2 })
	_, _, oldUnregisters, oldCloses := factory.all()[0].counts()
	if oldUnregisters != 1 || oldCloses != 1 {
		t.Fatalf("old runtime unregister/close = %d/%d", oldUnregisters, oldCloses)
	}
	// The replacement's REGISTER must follow the old de-REGISTER.
	seenOldUnregister := false
	ordered := false
	timeout := time.After(time.Second)
	for !ordered {
		select {
		case event := <-events:
			if event == "rt1.unregister" {
				seenOldUnregister = true
			}
			if event == "rt2.register" {
				ordered = seenOldUnregister
				if !ordered {
					t.Fatal("new REGISTER preceded old de-REGISTER")
				}
			}
		case <-timeout:
			t.Fatal("new REGISTER was not ordered after old de-REGISTER")
		}
	}
	if _, ok := m.LookupAOR("sip:Alice@pbx.example"); ok {
		t.Fatal("old AOR remains indexed")
	}

	disabled, err := m.Update("user-alice", created.ID, Input{Server: "new.example", Username: "Alice", Transport: TransportUDP, Enabled: false})
	if err != nil {
		t.Fatal(err)
	}
	if disabled.Enabled || disabled.Registration.State != RegistrationDisabled || disabled.Password != "secret" {
		t.Fatalf("disabled = %+v", disabled)
	}
	if _, _, unregisters, _ := factory.all()[1].counts(); unregisters != 1 {
		t.Fatalf("disable de-REGISTER count = %d", unregisters)
	}
	if known, ok := m.LookupAOR("sip:Alice@new.example"); !ok || known.Enabled {
		t.Fatalf("disabled known AOR = %+v, %v", known, ok)
	}

	if _, err := m.Update("user-alice", created.ID, testInput("new.example", "Alice", true)); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "re-enabled registration", func() bool {
		got, _ := m.Get("user-alice", created.ID)
		return got.Registration.State == RegistrationRegistered
	})
	waitFor(t, "re-enabled runtime", func() bool { return len(factory.all()) == 3 })
	if err := m.Delete("user-bob", created.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-user delete = %v", err)
	}
	if err := m.Delete("user-alice", created.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := m.LookupAOR("sip:Alice@new.example"); ok {
		t.Fatal("deleted AOR remains indexed")
	}
	if _, _, unregisters, closes := factory.all()[2].counts(); unregisters != 1 || closes != 1 {
		t.Fatalf("delete unregister/close = %d/%d", unregisters, closes)
	}
}

func TestManagerRetriesFailureAndStopsAfterDisable(t *testing.T) {
	m, factory := newFakeManager(t)
	factory.registerErr = errors.New("registrar unavailable")
	created, err := m.Create("user-alice", testInput("pbx.example", "1001", true))
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "initial runtime", func() bool { return len(factory.all()) == 1 })
	rt := factory.all()[0]
	waitFor(t, "failed state", func() bool {
		got, _ := m.Get("user-alice", created.ID)
		return got.Registration.State == RegistrationFailed && got.Registration.Attempts >= 2 && got.Registration.RetryAt != nil
	})
	rt.setRegisterError(nil)
	waitFor(t, "retry success", func() bool {
		got, _ := m.Get("user-alice", created.ID)
		return got.Registration.State == RegistrationRegistered && got.Registration.Attempts == 0
	})
	registers, _, _, _ := rt.counts()
	if registers < 3 {
		t.Fatalf("REGISTER calls = %d, want at least 3", registers)
	}
	if _, err := m.Update("user-alice", created.ID, Input{Server: "pbx.example", Username: "1001", Transport: TransportUDP, Enabled: false}); err != nil {
		t.Fatal(err)
	}
	_, _, unregisters, closes := rt.counts()
	if unregisters != 1 || closes != 1 {
		t.Fatalf("cleanup after retry success = unregister %d, close %d", unregisters, closes)
	}
}

func TestManagerCloseCancelsFactoryRetry(t *testing.T) {
	m := NewManager(slog.New(slog.DiscardHandler))
	m.retryMin = time.Hour
	m.retryMax = time.Hour
	m.jitter = func() float64 { return 0.5 }
	factoryCalls := make(chan struct{}, 1)
	m.newRuntime = func(Credential, *Manager) (runtime, error) {
		factoryCalls <- struct{}{}
		return nil, errors.New("cannot bind")
	}
	if _, err := m.Create("user-alice", testInput("pbx.example", "1001", true)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-factoryCalls:
	case <-time.After(time.Second):
		t.Fatal("factory was not called")
	}
	closed := make(chan struct{})
	go func() {
		_ = m.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("Close did not interrupt retry backoff")
	}
	if _, err := m.Create("user-alice", testInput("pbx.example", "1002", false)); !errors.Is(err, ErrClosed) {
		t.Fatalf("Create after Close = %v", err)
	}
}

func TestManagerCloseCancelsAllWorkersBeforeWaiting(t *testing.T) {
	m, factory := newFakeManager(t)
	for _, username := range []string{"1001", "1002"} {
		if _, err := m.Create("user-alice", testInput("pbx.example", username, true)); err != nil {
			t.Fatal(err)
		}
	}
	waitFor(t, "both registrations", func() bool {
		for _, credential := range m.List("user-alice") {
			if credential.Registration.State != RegistrationRegistered {
				return false
			}
		}
		return len(m.List("user-alice")) == 2
	})

	runtimes := factory.all()
	if len(runtimes) != 2 {
		t.Fatalf("runtime count = %d", len(runtimes))
	}
	for _, rt := range runtimes {
		rt.unregisterStarted = make(chan struct{}, 1)
		rt.unregisterRelease = make(chan struct{})
	}
	closed := make(chan struct{})
	go func() {
		_ = m.Close()
		close(closed)
	}()
	for index, rt := range runtimes {
		select {
		case <-rt.unregisterStarted:
		case <-time.After(time.Second):
			t.Fatalf("worker %d was not cancelled while another unregister was blocked", index+1)
		}
	}
	for _, rt := range runtimes {
		close(rt.unregisterRelease)
	}
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("Close did not finish after unregister requests were released")
	}
}

func TestNormalizeInputAcceptsRegistrarHostIPAndPortForms(t *testing.T) {
	tests := []struct {
		server     string
		wantServer string
		wantHost   string
		wantPort   int
	}{
		{server: "PBX.Example", wantServer: "pbx.example", wantHost: "pbx.example"},
		{server: "192.168.1.20", wantServer: "192.168.1.20", wantHost: "192.168.1.20"},
		{server: "127.0.0.1:5060", wantServer: "127.0.0.1:5060", wantHost: "127.0.0.1", wantPort: 5060},
		{server: "100.64.0.20:5061", wantServer: "100.64.0.20:5061", wantHost: "100.64.0.20", wantPort: 5061},
		{server: "169.254.1.20", wantServer: "169.254.1.20", wantHost: "169.254.1.20"},
		{server: "localhost:5060", wantServer: "localhost:5060", wantHost: "localhost", wantPort: 5060},
		{server: "sip:192.168.1.20:5060", wantServer: "192.168.1.20:5060", wantHost: "192.168.1.20", wantPort: 5060},
		{server: "[::1]", wantServer: "[::1]", wantHost: "[::1]"},
		{server: "[fe80::20]:5060", wantServer: "[fe80::20]:5060", wantHost: "[fe80::20]", wantPort: 5060},
	}
	for _, test := range tests {
		t.Run(test.server, func(t *testing.T) {
			credential, err := normalizeInput(testInput(test.server, "1001", false))
			if err != nil {
				t.Fatalf("normalizeInput(%q): %v", test.server, err)
			}
			if credential.Server != test.wantServer || credential.registrarHost != test.wantHost || credential.registrarPort != test.wantPort {
				t.Fatalf("normalizeInput(%q) = server %q, host %q, port %d", test.server, credential.Server, credential.registrarHost, credential.registrarPort)
			}
			if credential.SIPURI != "sip:1001@"+test.wantHost {
				t.Fatalf("normalizeInput(%q) SIP URI = %q", test.server, credential.SIPURI)
			}
		})
	}
}

func TestNormalizeInputRejectsAmbiguousAddresses(t *testing.T) {
	tests := []Input{
		testInput("", "1001", false),
		testInput("pbx.example", "", false),
		testInput("pbx.example", "1001@elsewhere", false),
		testInput("sip:1001@pbx.example", "1001", false),
		testInput("pbx.example", "white space", false),
		testInput("pbx.example:70000", "1001", false),
		testInput("pbx.example:not-a-port", "1001", false),
		testInput("192.168.1.1:5060:5061", "1001", false),
		testInput("http://pbx.example", "1001", false),
		testInput("pbx.example/path", "1001", false),
		testInput(`pbx.example\path`, "1001", false),
		testInput("999.1.1.1", "1001", false),
		testInput("[::1", "1001", false),
		{Server: "pbx.example", Username: "1001", Transport: "carrier-pigeon"},
	}
	for _, input := range tests {
		if _, err := normalizeInput(input); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("normalizeInput(%+v) error = %v", input, err)
		}
	}
}

func TestDeleteReservesAORUntilOldUnregisterFinishes(t *testing.T) {
	m, factory := newFakeManager(t)
	created, err := m.Create("user-alice", testInput("pbx.example", "1001", true))
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "registered before delete", func() bool {
		got, _ := m.Get("user-alice", created.ID)
		return got.Registration.State == RegistrationRegistered
	})
	old := factory.all()[0]
	old.unregisterStarted = make(chan struct{}, 1)
	old.unregisterRelease = make(chan struct{})
	deleted := make(chan error, 1)
	go func() { deleted <- m.Delete("user-alice", created.ID) }()
	select {
	case <-old.unregisterStarted:
	case <-time.After(time.Second):
		t.Fatal("old unregister did not start")
	}

	recreated := make(chan error, 1)
	go func() {
		_, err := m.Create("user-bob", testInput("pbx.example", "1001", true))
		recreated <- err
	}()
	select {
	case err := <-recreated:
		t.Fatalf("same AOR was recreated during old unregister: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	close(old.unregisterRelease)
	if err := <-deleted; err != nil {
		t.Fatal(err)
	}
	if err := <-recreated; err != nil {
		t.Fatalf("same AOR was not reusable after unregister: %v", err)
	}
}

func TestUpdateReservesTargetAORBeforeStoppingOldWorker(t *testing.T) {
	m, factory := newFakeManager(t)
	created, err := m.Create("user-alice", testInput("old.example", "1001", true))
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "registered before update", func() bool {
		got, _ := m.Get("user-alice", created.ID)
		return got.Registration.State == RegistrationRegistered
	})
	old := factory.all()[0]
	old.unregisterStarted = make(chan struct{}, 1)
	old.unregisterRelease = make(chan struct{})
	updated := make(chan error, 1)
	go func() {
		_, err := m.Update("user-alice", created.ID, testInput("new.example", "1001", true))
		updated <- err
	}()
	select {
	case <-old.unregisterStarted:
	case <-time.After(time.Second):
		t.Fatal("old unregister did not start")
	}

	conflictingCreate := make(chan error, 1)
	go func() {
		_, err := m.Create("user-bob", testInput("new.example", "1001", false))
		conflictingCreate <- err
	}()
	select {
	case err := <-conflictingCreate:
		t.Fatalf("target AOR was claimed during update: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	close(old.unregisterRelease)
	if err := <-updated; err != nil {
		t.Fatal(err)
	}
	if err := <-conflictingCreate; !errors.Is(err, ErrConflict) {
		t.Fatalf("conflicting create error = %v, want ErrConflict", err)
	}
	if owner, ok := m.LookupAOR("sip:1001@new.example"); !ok || owner.OwnerID != "user-alice" {
		t.Fatalf("target AOR owner = %+v, %v", owner, ok)
	}
}

func TestUpdatePreserveEnabledIsAtomic(t *testing.T) {
	m, _ := newFakeManager(t)
	created, err := m.Create("user-alice", testInput("pbx.example", "1001", true))
	if err != nil {
		t.Fatal(err)
	}
	updated, err := m.Update("user-alice", created.ID, Input{
		Server: "new.example", Username: "1001", Transport: TransportUDP, Enabled: false, PreserveEnabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !updated.Enabled || updated.Registration.State == RegistrationDisabled {
		t.Fatalf("PreserveEnabled update disabled credential: %+v", updated)
	}
}

func TestCredentialQuotaAndPrivateRegistrarAcceptance(t *testing.T) {
	m, _ := newFakeManager(t)
	m.maxPerUser = 2
	m.maxCredentials = 3
	if _, err := m.Create("user-alice", testInput("one.example", "1001", false)); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Create("user-alice", testInput("two.example", "1001", false)); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Create("user-alice", testInput("three.example", "1001", false)); !errors.Is(err, ErrQuota) {
		t.Fatalf("per-user quota error = %v", err)
	}
	if _, err := m.Create("user-bob", testInput("three.example", "1001", false)); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Create("user-carol", testInput("four.example", "1001", false)); !errors.Is(err, ErrQuota) {
		t.Fatalf("global quota error = %v", err)
	}
	m.maxPerUser = 10
	m.maxCredentials = 10
	if _, err := m.Create("visitor:abc", testInput("127.0.0.1", "1001", true)); err != nil {
		t.Fatalf("visitor loopback target rejected: %v", err)
	}
	if _, err := m.Create("user-alice", testInput("127.0.0.1", "1002", true)); err != nil {
		t.Fatalf("authenticated private target rejected: %v", err)
	}
	if _, ok := m.LookupAOR("sip:1001@one.example:not-a-port"); ok {
		t.Fatal("malformed URI matched an AOR")
	}
	if _, err := m.Create("user-bob", testInput("one.example:70000", "1002", false)); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("out-of-range port error = %v", err)
	}
}
