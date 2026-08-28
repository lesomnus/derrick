package blobstore_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/lesomnus/derrick/internal/blobstore"
	"github.com/lesomnus/derrick/internal/layout"
)

const key = "dist/perception/blobs/sha256:0000000000000000000000000000000000000000000000000000000000000000"

// The case that made this necessary: several tags copying at once, all of them
// needing the same layer, all of them finding it absent and writing it.
func TestARepeatedContentAddressedWriteHappensOnce(t *testing.T) {
	store := blobstore.NewMemory()
	serial := blobstore.NewSerial(store, layout.ContentAddressed)

	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()

			err := serial.Put(context.Background(), key, strings.NewReader("a-layer"), blobstore.PutOptions{})
			if err != nil {
				t.Errorf("put %d: %v", i, err)
			}
		}()
	}
	wg.Wait()

	if got := len(store.Writes); got != 1 {
		t.Errorf("the store saw %d writes of one blob, want 1: %v", got, store.Writes)
	}
	if body, ok := store.Body(key); !ok || string(body) != "a-layer" {
		t.Errorf("the blob is %q, want %q", body, "a-layer")
	}
}

// A tag is written every time, because rewriting it is how it moves.
func TestATagIsWrittenEveryTime(t *testing.T) {
	store := blobstore.NewMemory()
	serial := blobstore.NewSerial(store, layout.ContentAddressed)

	tag := layout.ManifestKey("dist/perception", "1.4.2")
	for i := range 3 {
		body := fmt.Sprintf("manifest-%d", i)
		if err := serial.Put(context.Background(), tag, strings.NewReader(body), blobstore.PutOptions{}); err != nil {
			t.Fatalf("put: %v", err)
		}
	}

	if got := len(store.Writes); got != 3 {
		t.Errorf("the store saw %d writes of a tag, want 3", got)
	}
	if body, _ := store.Body(tag); string(body) != "manifest-2" {
		t.Errorf("the tag holds %q, want the last thing written", body)
	}
}

// Different keys are not made to wait for each other.
func TestDifferentKeysAreNotSerialised(t *testing.T) {
	store := blobstore.NewMemory()
	serial := blobstore.NewSerial(store, layout.ContentAddressed)

	var wg sync.WaitGroup
	for i := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()

			k := fmt.Sprintf("dist/perception/blobs/sha256:%064x", i)
			if err := serial.Put(context.Background(), k, strings.NewReader("layer"), blobstore.PutOptions{}); err != nil {
				t.Errorf("put: %v", err)
			}
		}()
	}
	wg.Wait()

	if got := len(store.Writes); got != 16 {
		t.Errorf("the store saw %d writes, want 16", got)
	}
}
