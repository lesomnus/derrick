package blobstore_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"testing"
	"time"

	"github.com/lesomnus/derrick/internal/blobstore"
)

// The uploader switches to multipart above its part size, and the SDK will not
// take a part size below this, so an object has to be genuinely larger than it
// to exercise the path at all.
const multipartSize = 12 << 20

// TestMultipartUploadAgainstARealStore uploads an object large enough to go
// multipart, reads it back and checks it byte for byte.
//
// It is skipped unless a bucket is named, because the thing it is testing is
// not derrick: it is whether an S3-compatible service accepts what the SDK
// sends for a multipart upload with the settings NewS3 chooses. Recent SDKs
// attach x-amz-checksum-* headers that R2 refuses, which is why those settings
// exist, and multipart attaches them again per part and once more at the end —
// a code path a small object never reaches.
//
// A large layer is the first thing that would find this out otherwise, and by
// then somebody is trying to ship it.
//
//	DERRICK_TEST_BUCKET=my-bucket \
//	DERRICK_TEST_ENDPOINT=https://<account>.r2.cloudflarestorage.com \
//	AWS_ACCESS_KEY_ID=… AWS_SECRET_ACCESS_KEY=… \
//	go test ./internal/blobstore -run Multipart -v
func TestMultipartUploadAgainstARealStore(t *testing.T) {
	bucket := os.Getenv("DERRICK_TEST_BUCKET")
	if bucket == "" {
		t.Skip("DERRICK_TEST_BUCKET is not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	store, err := blobstore.NewS3(ctx, blobstore.S3Config{
		Bucket:      bucket,
		Endpoint:    os.Getenv("DERRICK_TEST_ENDPOINT"),
		Region:      "auto",
		Concurrency: 3,
	})
	if err != nil {
		t.Fatalf("open bucket: %v", err)
	}

	body := make([]byte, multipartSize)
	if _, err := rand.Read(body); err != nil {
		t.Fatalf("make a body: %v", err)
	}
	want := sha256.Sum256(body)

	// Not under a repository, and deliberately: no route a serverless-registry
	// serves maps to a key with no blobs/, manifests/ or _referrers/ segment
	// in it, so nothing this writes is reachable by anything holding a pull
	// token. The suffix keeps two runs from colliding.
	key := fmt.Sprintf("_test/multipart-%d", time.Now().UnixNano())

	t.Cleanup(func() {
		if err := store.Delete(context.Background(), key); err != nil {
			t.Errorf("clean up %s: %v", key, err)
		}
	})

	start := time.Now()
	if err := store.Put(ctx, key, bytes.NewReader(body), blobstore.PutOptions{}); err != nil {
		t.Fatalf("multipart upload of %d bytes: %v", len(body), err)
	}
	t.Logf("uploaded %d bytes in %s", len(body), time.Since(start).Round(time.Millisecond))

	obj, err := store.Stat(ctx, key)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if obj == nil {
		t.Fatal("the object is not there after a successful upload")
	}
	if obj.Size != int64(len(body)) {
		t.Fatalf("the object is %d bytes, want %d", obj.Size, len(body))
	}

	rc, err := store.Get(ctx, key)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer rc.Close()

	read, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}

	// The bytes are what matters. A multipart upload that completed but
	// assembled wrong would serve a layer whose digest does not match the
	// manifest naming it, which a client discovers at the end of a pull.
	got := sha256.Sum256(read)
	if got != want {
		t.Fatalf("what came back hashes to %s, want %s", hex.EncodeToString(got[:]), hex.EncodeToString(want[:]))
	}

	t.Logf("%d bytes round-tripped intact through a multipart upload", len(read))
}
