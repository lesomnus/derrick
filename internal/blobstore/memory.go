package blobstore

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"maps"
	"sort"
	"strings"
	"sync"
	"time"
)

// Memory is a Store held in memory.
//
// It exists so that the ordering and layout rules can be tested against
// something that records exactly what was written and when, which is the part
// of a publish that has to be right and the part a real bucket is least
// convenient for checking.
type Memory struct {
	mu      sync.Mutex
	objects map[string]memoryObject

	// Writes records the key of every Put in the order it happened.
	Writes []string

	// Deletes records the key of every Delete in the order it happened.
	Deletes []string

	// Clock stamps an object's modification time. Reclaiming space turns on
	// how old an object is, so a test needs to be able to say.
	Clock func() time.Time
}

type memoryObject struct {
	body        []byte
	contentType string
	metadata    map[string]string
	modified    time.Time
}

// NewMemory returns an empty in-memory store.
func NewMemory() *Memory {
	return &Memory{objects: map[string]memoryObject{}}
}

func (m *Memory) Stat(_ context.Context, key string) (*Object, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	obj, ok := m.objects[key]
	if !ok {
		return nil, nil
	}

	return &Object{
		Size:        int64(len(obj.body)),
		ContentType: obj.contentType,
		Metadata:    maps.Clone(obj.metadata),
	}, nil
}

func (m *Memory) Put(_ context.Context, key string, body io.Reader, opts PutOptions) error {
	raw, err := io.ReadAll(body)
	if err != nil {
		return fmt.Errorf("read body for %q: %w", key, err)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	m.objects[key] = memoryObject{
		body:        raw,
		contentType: opts.ContentType,
		metadata:    maps.Clone(opts.Metadata),
		modified:    m.now(),
	}
	m.Writes = append(m.Writes, key)

	return nil
}

func (m *Memory) Get(_ context.Context, key string) (io.ReadCloser, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	obj, ok := m.objects[key]
	if !ok {
		return nil, fmt.Errorf("get %q: no such object", key)
	}

	return io.NopCloser(bytes.NewReader(obj.body)), nil
}

func (m *Memory) List(_ context.Context, prefix string, fn func(Entry) error) error {
	m.mu.Lock()
	entries := make([]Entry, 0, len(m.objects))
	for key, obj := range m.objects {
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		entries = append(entries, Entry{Key: key, Size: int64(len(obj.body)), Modified: obj.modified})
	}
	m.mu.Unlock()

	// Sorted, because a real store lists in key order and a walk that depends
	// on map iteration order would pass here and fail there.
	sort.Slice(entries, func(i, j int) bool { return entries[i].Key < entries[j].Key })

	for _, entry := range entries {
		if err := fn(entry); err != nil {
			return err
		}
	}

	return nil
}

func (m *Memory) Delete(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, ok := m.objects[key]; !ok {
		return nil
	}

	delete(m.objects, key)
	m.Deletes = append(m.Deletes, key)

	return nil
}

// SetModified backdates an object, so that a grace period can be tested
// without waiting one out.
func (m *Memory) SetModified(key string, at time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()

	obj, ok := m.objects[key]
	if !ok {
		return
	}
	obj.modified = at
	m.objects[key] = obj
}

func (m *Memory) now() time.Time {
	if m.Clock != nil {
		return m.Clock()
	}

	return time.Now()
}

// Body returns the stored bytes at key, and whether it exists.
func (m *Memory) Body(key string) ([]byte, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	obj, ok := m.objects[key]

	return obj.body, ok
}

// Keys returns every stored key, sorted.
func (m *Memory) Keys() []string {
	m.mu.Lock()
	defer m.mu.Unlock()

	keys := make([]string, 0, len(m.objects))
	for key := range m.objects {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	return keys
}

// WriteOrder returns the index of the first write of key, or -1.
func (m *Memory) WriteOrder(key string) int {
	m.mu.Lock()
	defer m.mu.Unlock()

	for i, written := range m.Writes {
		if written == key {
			return i
		}
	}

	return -1
}
