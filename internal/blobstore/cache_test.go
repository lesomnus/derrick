package blobstore

import (
	"context"
	"sync"
	"testing"
)

// blocking is a store whose Stat waits to be let go, so a test can hold every
// caller inside the store at once and see how many got in.
type blocking struct {
	Store

	release chan struct{}

	mu      sync.Mutex
	calls   int
	entered chan struct{}
}

func (b *blocking) Stat(context.Context, string) (*Object, error) {
	b.mu.Lock()
	b.calls++
	b.mu.Unlock()

	b.entered <- struct{}{}
	<-b.release

	return &Object{Size: 1}, nil
}

func (b *blocking) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.calls
}

func always(string) bool { return true }

// The point of the cache is not that it remembers an answer -- it is that the
// store is asked once. Looking, releasing the lock and then asking satisfies
// the first and not the second, and a survey that walks in parallel will find
// the difference: two workers reaching the same blob together both miss.
//
// This holds the first caller inside the store while the rest arrive, which
// makes the overlap certain rather than a matter of scheduling.
func TestCacheAsksTheStoreOncePerKey(t *testing.T) {
	store := &blocking{Store: NewMemory(), release: make(chan struct{}), entered: make(chan struct{}, 8)}
	cache := NewCache(store, always)

	const callers = 8

	var wg sync.WaitGroup
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := cache.Stat(context.Background(), "same/key"); err != nil {
				t.Errorf("stat: %v", err)
			}
		}()
	}

	// One caller is inside the store. The others are either waiting on the
	// claim it made or about to; letting it go releases all of them.
	<-store.entered
	close(store.release)
	wg.Wait()

	if got := store.count(); got != 1 {
		t.Errorf("asked the store %d times, want 1", got)
	}
	if got := cache.Distinct(); got != 1 {
		t.Errorf("cached %d keys, want 1", got)
	}
}

// A key the cache does not consider fixed is asked about every time: a tag is
// not content-addressed, so an answer from a moment ago is not an answer.
func TestCacheAsksEveryTimeForKeysItDoesNotHold(t *testing.T) {
	store := NewMemory()
	cache := NewCache(store, func(string) bool { return false })

	before := store.StatCount()
	for range 3 {
		if _, err := cache.Stat(context.Background(), "some/tag"); err != nil {
			t.Fatalf("stat: %v", err)
		}
	}

	if got := store.StatCount() - before; got != 3 {
		t.Errorf("asked the store %d times, want 3", got)
	}
	if got := cache.Distinct(); got != 0 {
		t.Errorf("cached %d keys, want none", got)
	}
}

// The error is part of the answer, and repeating it costs nothing.
func TestCacheRemembersThatAnObjectIsAbsent(t *testing.T) {
	store := NewMemory()
	cache := NewCache(store, always)

	before := store.StatCount()
	for range 3 {
		object, err := cache.Stat(context.Background(), "not/there")
		if err != nil {
			t.Fatalf("stat: %v", err)
		}
		if object != nil {
			t.Fatalf("found %v for a key that was never written", object)
		}
	}

	if got := store.StatCount() - before; got != 1 {
		t.Errorf("asked the store %d times, want 1", got)
	}
}
