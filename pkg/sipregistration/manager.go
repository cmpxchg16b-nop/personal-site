package sipregistration

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"math/rand/v2"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
)

const (
	defaultAttemptTimeout     = 5 * time.Second
	defaultUnregisterTimeout  = 5 * time.Second
	defaultRegistrationExpiry = 5 * time.Minute
	defaultRetryMin           = 5 * time.Second
	defaultRetryMax           = time.Minute
	defaultMaxPerUser         = 8
	defaultMaxCredentials     = 64
	aorLockBuckets            = 256
)

// runtime is one SIP protocol stack and one REGISTER transaction. A runtime is
// owned by exactly one worker goroutine: diago transactions mutate their origin
// request and are not safe for concurrent Register/Qualify/Unregister calls.
type runtime interface {
	Register(context.Context) error
	Refresh(context.Context) error
	Unregister(context.Context) error
	Expiry() time.Duration
	Close() error
}

type runtimeFactory func(Credential, *Manager) (runtime, error)

type credentialKey struct {
	ownerID string
	id      string
}

type entry struct {
	lifecycle sync.Mutex

	key        credentialKey
	credential Credential // guarded by Manager.mu
	revision   uint64     // guarded by Manager.mu
	runtime    runtime    // guarded by Manager.mu
	cancel     context.CancelFunc
	done       chan struct{}
}

// Manager is the in-memory authority for credentials, observed registration
// state, and the canonical AOR → user/credential routing table. Network I/O is
// never performed while mu is held; entry.lifecycle serializes replacements of
// the same account without blocking operations on other users.
type Manager struct {
	logger *slog.Logger

	mu       sync.RWMutex
	entries  map[credentialKey]*entry
	aors     map[string]credentialKey
	closed   bool
	aorLocks [aorLockBuckets]sync.Mutex

	closeOnce sync.Once
	workers   sync.WaitGroup

	newRuntime         runtimeFactory
	now                func() time.Time
	attemptTimeout     time.Duration
	unregisterTimeout  time.Duration
	registrationExpiry time.Duration
	retryMin           time.Duration
	retryMax           time.Duration
	jitter             func() float64

	maxPerUser             int
	maxCredentials         int
	allowPrivateRegistrars bool
	service                *Service // guarded by mu; installed by NewService
}

// Option configures process-wide registration policy before the manager serves
// requests.
type Option func(*Manager)

// WithPrivateRegistrars controls whether anonymous visitors may target private,
// loopback, and other special-use registrar addresses. Enable only for local
// development or a trusted deployment; ordinary authenticated users are not
// restricted by this option.
func WithPrivateRegistrars(allow bool) Option {
	return func(manager *Manager) { manager.allowPrivateRegistrars = allow }
}

// NewManager constructs an empty in-memory manager. Credentials begin their
// REGISTER lifecycle when created enabled; no process-wide SIP account exists.
func NewManager(logger *slog.Logger, options ...Option) *Manager {
	if logger == nil {
		logger = slog.Default()
	}
	m := &Manager{
		logger:             logger,
		entries:            make(map[credentialKey]*entry),
		aors:               make(map[string]credentialKey),
		now:                time.Now,
		attemptTimeout:     defaultAttemptTimeout,
		unregisterTimeout:  defaultUnregisterTimeout,
		registrationExpiry: defaultRegistrationExpiry,
		retryMin:           defaultRetryMin,
		retryMax:           defaultRetryMax,
		jitter:             rand.Float64,
		maxPerUser:         defaultMaxPerUser,
		maxCredentials:     defaultMaxCredentials,
	}
	for _, option := range options {
		option(m)
	}
	m.newRuntime = func(credential Credential, manager *Manager) (runtime, error) {
		return newDiagoRuntime(credential, manager)
	}
	return m
}

// List returns only owner's credentials, ordered deterministically. Returned
// values are copies; callers cannot mutate manager state.
func (m *Manager) List(ownerID string) []Credential {
	m.mu.RLock()
	defer m.mu.RUnlock()
	credentials := make([]Credential, 0)
	for _, current := range m.entries {
		if current.key.ownerID != ownerID {
			continue
		}
		credentials = append(credentials, cloneCredential(current.credential))
	}
	sort.Slice(credentials, func(i, j int) bool {
		if credentials[i].SIPURI == credentials[j].SIPURI {
			return credentials[i].ID < credentials[j].ID
		}
		return credentials[i].SIPURI < credentials[j].SIPURI
	})
	return credentials
}

// Get returns one credential only when it belongs to ownerID.
func (m *Manager) Get(ownerID, id string) (Credential, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	current, ok := m.entries[credentialKey{ownerID: ownerID, id: id}]
	if !ok {
		return Credential{}, ErrNotFound
	}
	return cloneCredential(current.credential), nil
}

// LookupAOR resolves a SIP URI to its owning user and credential. The mapping
// exists for configured credentials (including disabled ones), so inbound
// signaling can distinguish an unknown AOR from a known but offline account.
func (m *Manager) LookupAOR(rawURI string) (Credential, bool) {
	aor, err := canonicalURI(rawURI)
	if err != nil {
		return Credential{}, false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	key, ok := m.aors[aor]
	if !ok {
		return Credential{}, false
	}
	current, ok := m.entries[key]
	if !ok {
		return Credential{}, false
	}
	return cloneCredential(current.credential), true
}

func canonicalURI(raw string) (string, error) {
	uri, err := sipUriForLookup(raw)
	if err != nil {
		return "", err
	}
	if uri.User == "" || uri.Host == "" {
		return "", errors.New("SIP URI has no user or host")
	}
	return canonicalAOR(uri.User, uri.Host), nil
}

// Create stores a credential and, when enabled, starts its asynchronous
// registration worker. A successful Create means the desired configuration is
// authoritative, not that the remote registrar has accepted it.
func (m *Manager) Create(ownerID string, input Input) (Credential, error) {
	if ownerID == "" {
		return Credential{}, fmt.Errorf("%w: owner is required", ErrInvalidInput)
	}
	normalized, err := normalizeInput(input)
	if err != nil {
		return Credential{}, err
	}
	normalized.OwnerID = ownerID
	if normalized.Enabled {
		if err := m.validateRegistrarTarget(normalized); err != nil {
			return Credential{}, err
		}
	}
	key := credentialKey{ownerID: ownerID, id: uuid.NewString()}
	normalized.ID = key.id
	normalized.OwnerID = ownerID
	normalized.Registration = Registration{State: RegistrationDisabled}
	if normalized.Enabled {
		normalized.Registration.State = RegistrationRegistering
	}
	current := &entry{key: key, credential: normalized, revision: 1}

	unlockAOR := m.lockAORs(normalized.SIPURI)
	defer unlockAOR()
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return Credential{}, ErrClosed
	}
	if _, exists := m.aors[normalized.SIPURI]; exists {
		m.mu.Unlock()
		return Credential{}, ErrConflict
	}
	ownerCredentials := 0
	for existingKey := range m.entries {
		if existingKey.ownerID == ownerID {
			ownerCredentials++
		}
	}
	if ownerCredentials >= m.maxPerUser || len(m.entries) >= m.maxCredentials {
		m.mu.Unlock()
		return Credential{}, ErrQuota
	}
	m.entries[key] = current
	m.aors[normalized.SIPURI] = key
	if normalized.Enabled {
		m.startWorkerLocked(current)
	}
	result := cloneCredential(current.credential)
	m.mu.Unlock()
	return result, nil
}

// Update replaces the desired configuration. Blank passwords retain the stored
// secret. Every update restarts an enabled registration: the old worker and its
// de-REGISTER finish before the replacement starts. AOR-bucket lifecycle locks
// reserve both old and target identities throughout that wait.
func (m *Manager) Update(ownerID, id string, input Input) (Credential, error) {
	// Validate everything except password/enabled inheritance before taking locks.
	normalized, err := normalizeInput(input)
	if err != nil {
		return Credential{}, err
	}
	normalized.OwnerID = ownerID
	key := credentialKey{ownerID: ownerID, id: id}
	m.mu.RLock()
	current := m.entries[key]
	m.mu.RUnlock()
	if current == nil {
		return Credential{}, ErrNotFound
	}

	current.lifecycle.Lock()
	defer current.lifecycle.Unlock()

	m.mu.RLock()
	if m.entries[key] != current {
		m.mu.RUnlock()
		return Credential{}, ErrNotFound
	}
	if m.closed {
		m.mu.RUnlock()
		return Credential{}, ErrClosed
	}
	if normalized.Password == "" {
		normalized.Password = current.credential.Password
	}
	if input.PreserveEnabled {
		normalized.Enabled = current.credential.Enabled
	}
	oldAOR := current.credential.SIPURI
	m.mu.RUnlock()
	if normalized.Enabled {
		if err := m.validateRegistrarTarget(normalized); err != nil {
			return Credential{}, err
		}
	}

	// Reserve both identities across the de-REGISTER wait. A concurrent create
	// or update of either AOR cannot slip into that window; unrelated AORs only
	// share work in the rare case of a hash-bucket collision.
	unlockAORs := m.lockAORs(oldAOR, normalized.SIPURI)
	defer unlockAORs()
	m.mu.RLock()
	if m.entries[key] != current {
		m.mu.RUnlock()
		return Credential{}, ErrNotFound
	}
	if m.closed {
		m.mu.RUnlock()
		return Credential{}, ErrClosed
	}
	if owner, conflict := m.aors[normalized.SIPURI]; conflict && owner != key {
		m.mu.RUnlock()
		return Credential{}, ErrConflict
	}
	m.mu.RUnlock()

	m.stopWorker(current)

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return Credential{}, ErrClosed
	}
	if owner, conflict := m.aors[normalized.SIPURI]; conflict && owner != key {
		m.mu.Unlock()
		return Credential{}, ErrConflict
	}
	normalized.ID = id
	normalized.Registration = Registration{State: RegistrationDisabled}
	if normalized.Enabled {
		normalized.Registration.State = RegistrationRegistering
	}
	delete(m.aors, oldAOR)
	m.aors[normalized.SIPURI] = key
	current.credential = normalized
	current.revision++
	if normalized.Enabled {
		m.startWorkerLocked(current)
	}
	result := cloneCredential(current.credential)
	m.mu.Unlock()
	return result, nil
}

// Delete removes the credential and its AOR route before waiting for the
// de-REGISTER. Subsequent inbound signaling is therefore logged as unmatched.
func (m *Manager) Delete(ownerID, id string) error {
	key := credentialKey{ownerID: ownerID, id: id}
	m.mu.RLock()
	current := m.entries[key]
	m.mu.RUnlock()
	if current == nil {
		return ErrNotFound
	}

	current.lifecycle.Lock()
	defer current.lifecycle.Unlock()
	m.mu.RLock()
	if m.entries[key] != current {
		m.mu.RUnlock()
		return ErrNotFound
	}
	aor := current.credential.SIPURI
	m.mu.RUnlock()
	unlockAOR := m.lockAORs(aor)
	defer unlockAOR()
	m.mu.Lock()
	if m.entries[key] != current {
		m.mu.Unlock()
		return ErrNotFound
	}
	delete(m.entries, key)
	delete(m.aors, current.credential.SIPURI)
	current.revision++
	m.mu.Unlock()
	m.stopWorker(current)
	return nil
}

// Close removes all routes, stops retries, best-effort de-REGISTERs every
// account whose REGISTER may have reached a registrar, and closes its SIP stack.
// It is safe to call more than once.
func (m *Manager) Close() error {
	m.closeOnce.Do(func() {
		m.mu.Lock()
		m.closed = true
		entries := make([]*entry, 0, len(m.entries))
		for key, current := range m.entries {
			current.revision++
			entries = append(entries, current)
			delete(m.entries, key)
		}
		clear(m.aors)
		service := m.service
		m.mu.Unlock()

		// Stop accepting browser work and terminate calls before any registrar
		// teardown can remove the runtimes those calls depend on.
		if service != nil {
			_ = service.Close()
		}

		// Cancel every generation before waiting for any unregister. Otherwise N
		// unresponsive registrars make shutdown take N times the unregister timeout.
		for _, current := range entries {
			current.lifecycle.Lock()
			m.cancelWorker(current)
		}
		for _, current := range entries {
			m.stopWorker(current)
			current.lifecycle.Unlock()
		}
		m.workers.Wait()
	})
	return nil
}

// startWorkerLocked starts one generation. m.mu must be held.
func (m *Manager) startWorkerLocked(current *entry) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	current.cancel = cancel
	current.done = done
	revision := current.revision
	credential := cloneCredential(current.credential)
	m.workers.Add(1)
	go func() {
		defer m.workers.Done()
		defer close(done)
		m.runWorker(ctx, current, revision, credential)
	}()
}

func (m *Manager) cancelWorker(current *entry) {
	m.mu.RLock()
	cancel := current.cancel
	rt := current.runtime
	service := m.service
	m.mu.RUnlock()
	if operations, ok := rt.(operationRuntime); ok && service != nil {
		// Match the exact runtime pointer, not a newly installed generation with
		// the same credential ID. Delete/Close may already have bumped revision.
		service.cancelCallsForRuntime(operations)
	}
	if operations, ok := rt.(interface{ StopOperations() }); ok {
		operations.StopOperations()
	}
	if cancel != nil {
		cancel()
	}
}

func (m *Manager) stopWorker(current *entry) {
	m.cancelWorker(current)
	m.mu.RLock()
	done := current.done
	m.mu.RUnlock()
	if done == nil {
		return
	}
	<-done
	m.mu.Lock()
	current.cancel = nil
	current.done = nil
	m.mu.Unlock()
}

func (m *Manager) runWorker(ctx context.Context, current *entry, revision uint64, credential Credential) {
	var rt runtime
	registerAttempted := false
	registeredOnce := false
	failures := 0
	var registeredAt, expiresAt *time.Time
	defer func() {
		m.mu.Lock()
		if current.runtime == rt {
			current.runtime = nil
		}
		m.mu.Unlock()
		if rt == nil {
			return
		}
		if registerAttempted {
			// A registrar can apply a REGISTER before its response is lost. Expire
			// only our Contact even when no successful response was observed.
			unregisterCtx, cancel := context.WithTimeout(context.Background(), m.unregisterTimeout)
			if err := rt.Unregister(unregisterCtx); err != nil {
				m.logger.Warn("SIP de-REGISTER failed", "aor", credential.SIPURI, "user_id", credential.OwnerID, "credential_id", credential.ID, "err", err)
			}
			cancel()
		}
		if err := rt.Close(); err != nil {
			m.logger.Warn("SIP stack close failed", "aor", credential.SIPURI, "user_id", credential.OwnerID, "credential_id", credential.ID, "err", err)
		}
	}()

	for ctx.Err() == nil {
		if rt == nil {
			var err error
			rt, err = m.newRuntime(credential, m)
			if err != nil {
				failures++
				delay := m.retryDelay(failures)
				m.publishFailure(current, revision, credential, failures, delay, fmt.Errorf("start SIP stack: %w", err), registeredAt, expiresAt)
				if !waitContext(ctx, delay) {
					return
				}
				rt = nil
				continue
			}
			m.mu.Lock()
			currentGeneration := !m.closed && ctx.Err() == nil && m.entries[current.key] == current && current.revision == revision
			if currentGeneration {
				current.runtime = rt
			}
			m.mu.Unlock()
			if !currentGeneration {
				return
			}
		}

		m.publish(current, revision, Registration{State: RegistrationRegistering, Attempts: failures, RegisteredAt: registeredAt, ExpiresAt: expiresAt})
		attemptCtx, cancel := context.WithTimeout(ctx, m.attemptTimeout)
		var err error
		if registeredOnce {
			err = rt.Refresh(attemptCtx)
		} else {
			registerAttempted = true
			err = rt.Register(attemptCtx)
		}
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			failures++
			delay := m.retryDelay(failures)
			m.publishFailure(current, revision, credential, failures, delay, err, registeredAt, expiresAt)
			if !waitContext(ctx, delay) {
				return
			}
			continue
		}

		expiry := rt.Expiry()
		if expiry <= 0 {
			failures++
			delay := m.retryDelay(failures)
			m.publishFailure(current, revision, credential, failures, delay, errors.New("registrar granted no positive registration expiry"), registeredAt, expiresAt)
			if !waitContext(ctx, delay) {
				return
			}
			continue
		}
		now := m.now().UTC()
		registered := now
		expires := now.Add(expiry)
		registeredAt = &registered
		expiresAt = &expires
		registeredOnce = true
		failures = 0
		m.publish(current, revision, Registration{State: RegistrationRegistered, RegisteredAt: registeredAt, ExpiresAt: expiresAt})
		m.logger.Info("SIP registration succeeded", "aor", credential.SIPURI, "user_id", credential.OwnerID, "credential_id", credential.ID, "transport", credential.Transport, "expires_in", expiry)
		if !waitContext(ctx, refreshDelay(expiry)) {
			return
		}
	}
}

func refreshDelay(expiry time.Duration) time.Duration {
	delay := expiry * 3 / 4
	if delay <= 0 {
		return 30 * time.Second
	}
	return delay
}

func (m *Manager) publishFailure(current *entry, revision uint64, credential Credential, failures int, delay time.Duration, err error, registeredAt, expiresAt *time.Time) {
	retryAt := m.now().UTC().Add(delay)
	m.publish(current, revision, Registration{
		State:        RegistrationFailed,
		LastError:    err.Error(),
		Attempts:     failures,
		RegisteredAt: registeredAt,
		ExpiresAt:    expiresAt,
		RetryAt:      &retryAt,
	})
	m.logger.Warn("SIP registration attempt failed; retry scheduled", "aor", credential.SIPURI, "user_id", credential.OwnerID, "credential_id", credential.ID, "attempt", failures, "retry_in", delay, "err", err)
}

func (m *Manager) publish(current *entry, revision uint64, status Registration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || m.entries[current.key] != current || current.revision != revision {
		return
	}
	current.credential.Registration = cloneRegistration(status)
}

func (m *Manager) retryDelay(failures int) time.Duration {
	if failures < 1 {
		failures = 1
	}
	delay := m.retryMin
	for attempt := 1; attempt < failures && delay < m.retryMax; attempt++ {
		delay *= 2
		if delay > m.retryMax {
			delay = m.retryMax
		}
	}
	if delay > m.retryMax {
		delay = m.retryMax
	}
	if delay <= 0 {
		return time.Millisecond
	}
	// ±20% jitter avoids a fleet of credentials retrying in lockstep.
	factor := 0.8 + 0.4*m.jitter()
	return time.Duration(float64(delay) * factor)
}

func waitContext(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// lockAORs serializes lifecycle operations that can unregister/register the
// same SIP identity. Buckets avoid a process-wide network wait while keeping
// the lock table bounded. Multiple AORs are locked in bucket order to prevent
// two cross-updates from deadlocking.
func (m *Manager) lockAORs(aors ...string) func() {
	buckets := make([]int, 0, len(aors))
	seen := make(map[int]struct{}, len(aors))
	for _, aor := range aors {
		hash := fnv.New32a()
		_, _ = hash.Write([]byte(aor))
		bucket := int(hash.Sum32() % aorLockBuckets)
		if _, duplicate := seen[bucket]; duplicate {
			continue
		}
		seen[bucket] = struct{}{}
		buckets = append(buckets, bucket)
	}
	sort.Ints(buckets)
	for _, bucket := range buckets {
		m.aorLocks[bucket].Lock()
	}
	return func() {
		for index := len(buckets) - 1; index >= 0; index-- {
			m.aorLocks[buckets[index]].Unlock()
		}
	}
}
