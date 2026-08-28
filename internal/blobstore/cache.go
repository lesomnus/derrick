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
	seen map[string]cached
}

type cached struct {
	object *Object
	err    error
}

// NewCache wraps store. fixed may be nil, which caches nothing.
func NewCache(store Store, fixed func(key string) bool) *Cache {
	return &Cache{Store: store, fixed: fixed, seen: map[string]cached{}}
}

func (c *Cache) Stat(ctx context.Context, key string) (*Object, error) {
	if c.fixed == nil || !c.fixed(key) {
		return c.Store.Stat(ctx, key)
	}

	c.mu.Lock()
	hit, ok := c.seen[key]
	c.mu.Unlock()

	if ok {
		return hit.object, hit.err
	}

	object, err := c.Store.Stat(ctx, key)

	c.mu.Lock()
	c.seen[key] = cached{object: object, err: err}
	c.mu.Unlock()

	return object, err
}

// Distinct is how many keys the cache has answers for.
func (c *Cache) Distinct() int {
	c.mu.Lock()
	defer c.mu.Unlock()

	return len(c.seen)
}
