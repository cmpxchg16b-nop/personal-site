package sipbot

import (
	"fmt"
	"sync"
	"testing"

	"personal-site/pkg/models/ss"
)

// TestOnMemoryUserSessionStorage covers the store's semantics: an empty
// store misses, Store publishes and replaces, Delete drops once and
// reports what it dropped.
func TestOnMemoryUserSessionStorage(t *testing.T) {
	s := NewOnMemoryUserSessionStorage()
	user := ss.SubscriberId("1-user")
	sess := UserSession{AddressOfRecord: "2001@sip.example.com", Username: "2001", Host: "sip.example.com", Password: "passW_0rd"}

	if _, ok := s.Load(user); ok {
		t.Fatal("an empty store hits")
	}
	if _, ok := s.Delete(user); ok {
		t.Fatal("deleting an empty entry reports true")
	}

	s.Store(user, sess)
	got, ok := s.Load(user)
	if !ok || got != sess {
		t.Fatalf("Load = %+v, %v; want the stored session", got, ok)
	}

	replacement := UserSession{AddressOfRecord: "2002@sip.example.com", Username: "2002", Host: "sip.example.com", Password: "other"}
	s.Store(user, replacement)
	if got, _ := s.Load(user); got != replacement {
		t.Fatalf("after replacement Load = %+v, want %+v", got, replacement)
	}

	dropped, ok := s.Delete(user)
	if !ok || dropped != replacement {
		t.Fatalf("Delete = %+v, %v; want the replaced session", dropped, ok)
	}
	if _, ok := s.Load(user); ok {
		t.Fatal("a deleted entry still hits")
	}
}

// TestOnMemoryUserSessionStorageConcurrency hammers the store from many
// goroutines — the interface's contract is concurrent use — under the
// race detector.
func TestOnMemoryUserSessionStorageConcurrency(t *testing.T) {
	s := NewOnMemoryUserSessionStorage()
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			user := ss.SubscriberId(fmt.Sprintf("user-%d", i%4))
			sess := UserSession{AddressOfRecord: fmt.Sprintf("%d@sip.example.com", i), Username: "u", Host: "h", Password: "p"}
			for j := 0; j < 100; j++ {
				s.Store(user, sess)
				s.Load(user)
				s.Delete(user)
			}
		}(i)
	}
	wg.Wait()
}
