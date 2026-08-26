package blobstore

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"maps"
	"sort"
	"sync"
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
}

type memoryObject struct {
	body        []byte
	contentType string
	metadata    map[string]string
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
