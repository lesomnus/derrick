// Package blobstore is the object store derrick writes into.
//
// The interface is deliberately three operations wide. A registry bucket is
// written once and read forever, so the only things a publisher needs are
// "is this already here", "put it here", and "read it back to check".
package blobstore

import (
	"context"
	"io"
	"time"
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

// Entry is an object as it appears in a listing.
type Entry struct {
	Key      string
	Size     int64
	Modified time.Time
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

// Lister is a store whose keys can be walked.
//
// Publishing never needs this: every key a publish writes is computed from the
// image. Reclaiming space does, because the question it asks — what is here
// that nothing points at — cannot be answered from the images that are still
// wanted.
type Lister interface {
	// List calls fn for every object whose key begins with prefix. An error
	// from fn stops the walk and is returned.
	List(ctx context.Context, prefix string, fn func(Entry) error) error
}

// Deleter is a store objects can be removed from.
type Deleter interface {
	// Delete removes key. A key that does not exist is not an error, because
	// the caller's intent — that it be gone — is already satisfied.
	Delete(ctx context.Context, key string) error
}

// Bucket is a store that can also be walked and deleted from.
//
// The two halves are deliberately separate interfaces: everything that serves
// or publishes an image needs only Store, and a type that cannot delete cannot
// delete the wrong thing.
type Bucket interface {
	Store
	Lister
	Deleter
}
