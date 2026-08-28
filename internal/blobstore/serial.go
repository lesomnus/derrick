package blobstore

import (
	"context"
	"io"
	"sync"
)

// Serial wraps a store so that no two goroutines write the same key at once.
//
// Object stores do not all take kindly to that, and R2 in particular answers
// with `429 ServiceUnavailable: Reduce your concurrent request rate for the
// same object`. A publisher runs into it without meaning to, because images
// share layers: copying four tags at once that were built from the same base
// — or four tags that are the same image under different names — is four
// goroutines uploading one blob to one key.
//
// Fixed reports the keys whose contents are determined by the key itself. A
// repeat write of one of those is skipped rather than merely serialised: the
// object already holds the bytes it would have been written with, and the
// second upload was work nobody needed. Anything else — a tag, whose whole
// purpose is to be rewritten — is serialised and written.
//
// The bookkeeping is per Serial and grows with the number of distinct keys
// written, which is the size of one publish. It is meant to live for a run.
type Serial struct {
	Store

	fixed func(key string) bool

	mu      sync.Mutex
	locks   map[string]*sync.Mutex
	written map[string]bool
}

// NewSerial wraps store. fixed may be nil, which serialises every repeat write
// instead of skipping any of them.
func NewSerial(store Store, fixed func(key string) bool) *Serial {
	return &Serial{
		Store:   store,
		fixed:   fixed,
		locks:   map[string]*sync.Mutex{},
		written: map[string]bool{},
	}
}

func (s *Serial) Put(ctx context.Context, key string, body io.Reader, opts PutOptions) error {
	lock := s.lockFor(key)
	lock.Lock()
	defer lock.Unlock()

	if s.skip(key) {
		// The body is left unread; closing it is the caller's job either way,
		// and reading it would be the transfer this exists to avoid.
		return nil
	}

	if err := s.Store.Put(ctx, key, body, opts); err != nil {
		return err
	}

	s.mu.Lock()
	s.written[key] = true
	s.mu.Unlock()

	return nil
}

func (s *Serial) skip(key string) bool {
	if s.fixed == nil || !s.fixed(key) {
		return false
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	return s.written[key]
}

func (s *Serial) lockFor(key string) *sync.Mutex {
	s.mu.Lock()
	defer s.mu.Unlock()

	lock, ok := s.locks[key]
	if !ok {
		lock = &sync.Mutex{}
		s.locks[key] = lock
	}

	return lock
}
