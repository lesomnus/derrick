package main_test

import (
	"context"
	"log"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"github.com/lesomnus/derrick/internal/blobstore"
	"github.com/lesomnus/derrick/internal/copier"
	"github.com/lesomnus/derrick/internal/layout"
	"github.com/lesomnus/derrick/internal/source"
	"github.com/lesomnus/derrick/internal/verify"
)

const repository = "robot/perception"

// serve starts a real registry in this process, so the source path is
// exercised against something that speaks the distribution API rather than
// against a stand-in for one.
func serve(t *testing.T) string {
	t.Helper()

	server := httptest.NewServer(registry.New(registry.Logger(newTestLogger(t))))
	t.Cleanup(server.Close)

	u, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("parse server url: %v", err)
	}

	return u.Host
}

func push(t *testing.T, host, tag string, image v1.Image) name.Reference {
	t.Helper()

	ref, err := name.ParseReference(host+"/"+repository+":"+tag, name.Insecure)
	if err != nil {
		t.Fatalf("parse reference: %v", err)
	}
	if err := remote.Write(ref, image); err != nil {
		t.Fatalf("push: %v", err)
	}

	return ref
}

func copyToBucket(t *testing.T, ref name.Reference, store blobstore.Store, tag string) *copier.Result {
	t.Helper()

	ctx := context.Background()

	src, root, err := source.Open(ctx, ref.String(), source.Options{Insecure: true})
	if err != nil {
		t.Fatalf("open source: %v", err)
	}

	c := &copier.Copier{
		Source:      src,
		Store:       store,
		Repository:  repository,
		Concurrency: 3,
		Verify:      true,
	}

	result, err := c.Run(ctx, root, tag)
	if err != nil {
		t.Fatalf("copy: %v", err)
	}

	return result
}

func verifyBucket(t *testing.T, store blobstore.Store, tag string) *verify.Report {
	t.Helper()

	v := &verify.Verifier{Store: store, Repository: repository}
	report, err := v.Tag(context.Background(), tag)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !report.OK() {
		t.Fatalf("the published tag is not servable: %v", report.Problems)
	}

	return report
}

func TestCopyImageFromRegistry(t *testing.T) {
	host := serve(t)

	image, err := random.Image(1024, 3)
	if err != nil {
		t.Fatalf("build image: %v", err)
	}
	ref := push(t, host, "1.4.2", image)

	store := blobstore.NewMemory()
	result := copyToBucket(t, ref, store, "1.4.2")

	digest, err := image.Digest()
	if err != nil {
		t.Fatalf("digest: %v", err)
	}

	report := verifyBucket(t, store, "1.4.2")
	if report.Digest != digest.String() {
		t.Errorf("published digest %s, want %s", report.Digest, digest.String())
	}

	// A config and three layers.
	if result.BlobsUploaded != 4 {
		t.Errorf("BlobsUploaded = %d, want 4", result.BlobsUploaded)
	}

	// The bytes in the bucket must be the bytes the registry served, or the
	// digest the client computes will not be the one it was promised.
	raw, err := image.RawManifest()
	if err != nil {
		t.Fatalf("raw manifest: %v", err)
	}
	stored, ok := store.Body(layout.ManifestKey(repository, digest.String()))
	if !ok {
		t.Fatal("the manifest is not in the bucket")
	}
	if string(stored) != string(raw) {
		t.Error("the stored manifest is not byte-identical to the one served")
	}
}

func TestCopyIndexFromRegistry(t *testing.T) {
	host := serve(t)

	index, err := random.Index(1024, 2, 3)
	if err != nil {
		t.Fatalf("build index: %v", err)
	}

	ref, err := name.ParseReference(host+"/"+repository+":multi", name.Insecure)
	if err != nil {
		t.Fatalf("parse reference: %v", err)
	}
	if err := remote.WriteIndex(ref, index); err != nil {
		t.Fatalf("push index: %v", err)
	}

	store := blobstore.NewMemory()
	copyToBucket(t, ref, store, "multi")

	report := verifyBucket(t, store, "multi")

	// The index plus its three children.
	if report.Manifests != 4 {
		t.Errorf("Manifests = %d, want 4", report.Manifests)
	}

	digest, err := index.Digest()
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	if report.Digest != digest.String() {
		t.Errorf("published digest %s, want %s", report.Digest, digest.String())
	}
}

// Republishing is the common case: most releases share most of their layers
// with the one before, and a copy that re-uploaded them would make every
// release cost a full image.
func TestCopyIsIdempotent(t *testing.T) {
	host := serve(t)

	image, err := random.Image(1024, 2)
	if err != nil {
		t.Fatalf("build image: %v", err)
	}
	ref := push(t, host, "1.4.2", image)

	store := blobstore.NewMemory()
	first := copyToBucket(t, ref, store, "1.4.2")
	second := copyToBucket(t, ref, store, "1.4.2")

	if first.BlobsUploaded == 0 {
		t.Fatal("the first copy uploaded nothing")
	}
	if second.BlobsUploaded != 0 {
		t.Errorf("the second copy uploaded %d blobs, want 0", second.BlobsUploaded)
	}
	if second.BlobsSkipped != first.BlobsUploaded {
		t.Errorf("BlobsSkipped = %d, want %d", second.BlobsSkipped, first.BlobsUploaded)
	}
	if second.ManifestsWritten != 0 {
		t.Errorf("the second copy rewrote %d manifests, want 0", second.ManifestsWritten)
	}

	verifyBucket(t, store, "1.4.2")
}

// Moving a tag is a single object write, which is what makes it safe to
// republish while something is pulling.
func TestTagMovesToANewImage(t *testing.T) {
	host := serve(t)

	first, err := random.Image(1024, 1)
	if err != nil {
		t.Fatalf("build image: %v", err)
	}
	second, err := random.Image(1024, 1)
	if err != nil {
		t.Fatalf("build image: %v", err)
	}

	store := blobstore.NewMemory()
	copyToBucket(t, push(t, host, "latest", first), store, "latest")
	copyToBucket(t, push(t, host, "latest", second), store, "latest")

	digest, err := second.Digest()
	if err != nil {
		t.Fatalf("digest: %v", err)
	}

	report := verifyBucket(t, store, "latest")
	if report.Digest != digest.String() {
		t.Errorf("the tag still points at %s, want %s", report.Digest, digest.String())
	}
}

type testLogger struct{ t *testing.T }

func newTestLogger(t *testing.T) *log.Logger {
	return log.New(&testLogger{t: t}, "", 0)
}

func (l *testLogger) Write(p []byte) (int, error) {
	l.t.Logf("registry: %s", p)

	return len(p), nil
}
