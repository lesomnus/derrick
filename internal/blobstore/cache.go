package blobstore

import (
	"context"
	"sync"
)

// Cache remembers what Stat found, for keys whose contents their own key
// decides.
//
// Walking a repository asks about the same blob once per tag that references
// it, and tags of one repository share nearly all of their layers: a hundred
// tags over sixty blobs is six thousand questions about a few hundred
// distinct objects. Those objects are content-addressed, so the answer cannot
// change underneath a walk in a way that matters — the bytes at that key are
// the bytes that key names, present or absent.
//
// Fixed decides which keys those are. Anything else is asked every time, so a
// tag is never answered from a cache.
type Cache struct {
	Store

	fixed func(key string) bool

	mu   sync.Mutex
	seen map[string]*cached
}

// cached is one key's answer, which may not have arrived yet.
//
// The entry goes into the map before the question is asked, so a second caller
// finds it and waits rather than asking again. ready is closed once object and
// err are set, and closing it is what makes them safe to read.
type cached struct {
	ready  chan struct{}
	object *Object
	err    error
}

// NewCache wraps store. fixed may be nil, which caches nothing.
func NewCache(store Store, fixed func(key string) bool) *Cache {
	return &Cache{Store: store, fixed: fixed, seen: map[string]*cached{}}
}

// Stat answers from the cache, and asks the store at most once per key.
//
// The at-most-once matters because the walk is parallel. Looking, releasing the
// lock, and then asking would let two workers reaching the same blob at the
// same time both miss and both ask -- the map still ends up with one entry, so
// the count of distinct objects stays right and only the number of requests
// gives it away. That is the whole property this type exists for, and it was
// untrue: the survey of a repository whose tags share their layers made half
// again as many requests as it reported.
//
// So the entry is claimed under the lock before the store is asked, and anyone
// who finds a claim waits on it.
func (c *Cache) Stat(ctx context.Context, key string) (*Object, error) {
	if c.fixed == nil || !c.fixed(key) {
		return c.Store.Stat(ctx, key)
	}

	c.mu.Lock()
	entry, ok := c.seen[key]
	if ok {
		c.mu.Unlock()
		<-entry.ready

		return entry.object, entry.err
	}

	entry = &cached{ready: make(chan struct{})}
	c.seen[key] = entry
	c.mu.Unlock()

	entry.object, entry.err = c.Store.Stat(ctx, key)
	close(entry.ready)

	return entry.object, entry.err
}

// Distinct is how many keys the cache holds an answer for, counting one still
// being asked about. A survey reads it after its walk, by which time there are
// none of those.
func (c *Cache) Distinct() int {
	c.mu.Lock()
	defer c.mu.Unlock()

	return len(c.seen)
}
