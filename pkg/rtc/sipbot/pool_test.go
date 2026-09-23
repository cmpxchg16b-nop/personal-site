package sipbot

import (
	"fmt"
	"slices"
	"sync"
	"testing"
)

// TestNewSIPCredentialPool covers construction: the explicit entries are
// parsed (the scheme tolerated, a port kept), the ranges validated — a
// malformed, reversed, or oversized usernameRange, an empty sipServer,
// a sipServer with a user part all fail.
func TestNewSIPCredentialPool(t *testing.T) {
	pool, err := NewSIPCredentialPool(
		[]SIPCredential{{URI: "sip:1001@sip.example.com", Password: "1234"}},
		[]SIPCredentialRange{{UsernameRange: "1101-1120", Password: "hell0", SIPServer: "pbx.example.com:5061"}},
	)
	if err != nil {
		t.Fatalf("NewSIPCredentialPool: %v", err)
	}
	if pool.total != 21 {
		t.Fatalf("pool.total = %d, want 21", pool.total)
	}

	for _, tc := range []struct {
		name   string
		creds  []SIPCredential
		ranges []SIPCredentialRange
	}{
		{"a bad sipUri", []SIPCredential{{URI: "not-an-uri", Password: "p"}}, nil},
		{"an empty usernameRange", nil, []SIPCredentialRange{{UsernameRange: "", Password: "p", SIPServer: "h"}}},
		{"a non-integer range", nil, []SIPCredentialRange{{UsernameRange: "abc-def", Password: "p", SIPServer: "h"}}},
		{"a reversed range", nil, []SIPCredentialRange{{UsernameRange: "1120-1101", Password: "p", SIPServer: "h"}}},
		{"a signed range", nil, []SIPCredentialRange{{UsernameRange: "+1-2", Password: "p", SIPServer: "h"}}},
		{"a range with a trailing dash", nil, []SIPCredentialRange{{UsernameRange: "1-2-3", Password: "p", SIPServer: "h"}}},
		{"an oversized range", nil, []SIPCredentialRange{{UsernameRange: "1-100001", Password: "p", SIPServer: "h"}}},
		{"an empty sipServer", nil, []SIPCredentialRange{{UsernameRange: "1-2", Password: "p"}}},
		{"a sipServer with a user part", nil, []SIPCredentialRange{{UsernameRange: "1-2", Password: "p", SIPServer: "user@host"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewSIPCredentialPool(tc.creds, tc.ranges); err == nil {
				t.Fatalf("NewSIPCredentialPool(%v, %v) succeeded, want an error", tc.creds, tc.ranges)
			}
		})
	}
}

// TestSIPCredentialPoolAllocate covers the allocation order — the
// explicit entries first, then the ranges' members in order — the
// exhaustion, and the release channel: a released loan is re-loaned
// once the fresh credentials run out, and not before (fresh-first).
func TestSIPCredentialPoolAllocate(t *testing.T) {
	pool, err := NewSIPCredentialPool(
		[]SIPCredential{{URI: "1001@sip.example.com", Password: "e"}},
		[]SIPCredentialRange{{UsernameRange: "1101-1103", Password: "r", SIPServer: "sip.example.com:5061"}},
	)
	if err != nil {
		t.Fatalf("NewSIPCredentialPool: %v", err)
	}
	want := []UserSession{
		{AddressOfRecord: "1001@sip.example.com", Username: "1001", Host: "sip.example.com", Password: "e", Pooled: true},
		{AddressOfRecord: "1101@sip.example.com:5061", Username: "1101", Host: "sip.example.com", Port: 5061, Password: "r", Pooled: true},
		{AddressOfRecord: "1102@sip.example.com:5061", Username: "1102", Host: "sip.example.com", Port: 5061, Password: "r", Pooled: true},
		{AddressOfRecord: "1103@sip.example.com:5061", Username: "1103", Host: "sip.example.com", Port: 5061, Password: "r", Pooled: true},
	}
	var got []UserSession
	for range want {
		c, ok := pool.Allocate()
		if !ok {
			t.Fatalf("Allocate #%d failed, want a credential", len(got)+1)
		}
		got = append(got, c)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("the allocation order = %+v, want %+v", got, want)
	}

	// Exhausted: no fresh credential, nothing released yet.
	if c, ok := pool.Allocate(); ok {
		t.Fatalf("an exhausted pool loaned %+v", c)
	}

	// A released loan is handed out again.
	pool.Release(got[1])
	if c, ok := pool.Allocate(); !ok || c != got[1] {
		t.Fatalf("Allocate after Release = %+v, %v; want the released %+v", c, ok, got[1])
	}
	if c, ok := pool.Allocate(); ok {
		t.Fatalf("the pool loaned past its size: %+v", c)
	}
}

// TestSIPCredentialPoolFreshFirst covers the ordering within a
// half-loaned pool: while fresh credentials remain, a released loan
// waits — a just-failed account sinks to the back of the queue instead
// of failing the very next user too.
func TestSIPCredentialPoolFreshFirst(t *testing.T) {
	pool, err := NewSIPCredentialPool(nil, []SIPCredentialRange{{UsernameRange: "1-2", Password: "p", SIPServer: "h"}})
	if err != nil {
		t.Fatalf("NewSIPCredentialPool: %v", err)
	}
	first, _ := pool.Allocate()
	pool.Release(first)
	second, ok := pool.Allocate()
	if !ok || second == first {
		t.Fatalf("Allocate with a fresh credential left = %+v, %v; want the fresh one, not the released %+v", second, ok, first)
	}
	// The fresh run out; the released loan is all that remains.
	third, ok := pool.Allocate()
	if !ok || third != first {
		t.Fatalf("Allocate with only the released loan left = %+v, %v; want %+v", third, ok, first)
	}
	if c, ok := pool.Allocate(); ok {
		t.Fatalf("the pool loaned past its size: %+v", c)
	}
}

// TestSIPCredentialPoolNil covers the nil pool — a valid empty pool:
// Allocate always fails, Release never panics.
func TestSIPCredentialPoolNil(t *testing.T) {
	var pool *SIPCredentialPool
	if c, ok := pool.Allocate(); ok {
		t.Fatalf("a nil pool loaned %+v", c)
	}
	pool.Release(UserSession{AddressOfRecord: "1001@sip.example.com"})
}

// TestSIPCredentialPoolConcurrency hammers Allocate and Release from
// many goroutines under -race: the whole pool can be loaned out
// concurrently, every loan distinct; the churn phase mixes allocations
// and releases freely.
func TestSIPCredentialPoolConcurrency(t *testing.T) {
	const size = 100
	pool, err := NewSIPCredentialPool(nil, []SIPCredentialRange{{UsernameRange: fmt.Sprintf("1-%d", size), Password: "p", SIPServer: "h"}})
	if err != nil {
		t.Fatalf("NewSIPCredentialPool: %v", err)
	}

	// Phase 1: the whole pool allocated concurrently, every loan distinct.
	results := make(chan UserSession, size)
	var wg sync.WaitGroup
	for i := 0; i < size; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if c, ok := pool.Allocate(); ok {
				results <- c
			}
		}()
	}
	wg.Wait()
	close(results)
	seen := map[string]bool{}
	n := 0
	for c := range results {
		n++
		if seen[c.AddressOfRecord] {
			t.Fatalf("the pool loaned %q twice", c.AddressOfRecord)
		}
		seen[c.AddressOfRecord] = true
	}
	if n != size {
		t.Fatalf("the pool loaned %d credentials, want %d", n, size)
	}

	// Phase 2: everything back in, then allocate/release churn.
	for aor := range seen {
		pool.Release(UserSession{AddressOfRecord: aor})
	}
	var wg2 sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg2.Add(1)
		go func() {
			defer wg2.Done()
			for j := 0; j < 50; j++ {
				if c, ok := pool.Allocate(); ok {
					pool.Release(c)
				}
			}
		}()
	}
	wg2.Wait()
}
