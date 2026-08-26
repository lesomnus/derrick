// Package blobstore is the object store derrick writes into.
//
// The interface is deliberately three operations wide. A registry bucket is
// written once and read forever, so the only things a publisher needs are
// "is this already here", "put it here", and "read it back to check".
package blobstore

import (
	"context"
	"io"
)

// Object is what a store knows about a key without reading its contents.
type Object struct {
	Size        int64
	ContentType string
	Metadata    map[string]string
}

// PutOptions carries the object attributes the registry reads back.
type PutOptions struct {
	ContentType string
	Metadata    map[string]string
}

// Store is an object store addressed by key.
type Store interface {
	// Stat returns nil, nil when the key does not exist. Callers rely on that
	// to skip content-addressed objects they would otherwise re-upload.
	Stat(ctx context.Context, key string) (*Object, error)

	// Put writes body at key. An existing object is replaced.
	Put(ctx context.Context, key string, body io.Reader, opts PutOptions) error

	// Get reads an object back.
	Get(ctx context.Context, key string) (io.ReadCloser, error)
}
