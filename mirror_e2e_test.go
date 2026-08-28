package main_test

import (
	"context"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"github.com/lesomnus/derrick/internal/blobstore"
	"github.com/lesomnus/derrick/internal/mirror"
	"github.com/lesomnus/derrick/internal/source"
	"github.com/lesomnus/derrick/internal/verify"
)

// pushTo publishes a random image into one repository of the test registry.
func pushTo(t *testing.T, host, repository, tag string) v1.Image {
	t.Helper()

	image, err := random.Image(512, 2)
	if err != nil {
		t.Fatalf("build image: %v", err)
	}

	ref, err := name.ParseReference(host+"/"+repository+":"+tag, name.Insecure)
	if err != nil {
		t.Fatalf("parse reference: %v", err)
	}
	if err := remote.Write(ref, image); err != nil {
		t.Fatalf("push %s: %v", ref, err)
	}

	return image
}

func mirrorRegistry(t *testing.T, host string, store blobstore.Store) *mirror.Result {
	t.Helper()

	registry, err := source.OpenRegistry(host, source.Options{Insecure: true})
	if err != nil {
		t.Fatalf("open registry: %v", err)
	}

	m := &mirror.Mirror{
		Registry: mirror.FromSource(registry),
		Store:    store,
		Bucket:   "registry-test",
		Prefix:   "dist",
		Parallel: 3,
		Verify:   true,
		Log:      func(format string, args ...any) { t.Logf(format, args...) },
	}

	result, err := m.Run(context.Background())
	if err != nil {
		t.Fatalf("mirror: %v", err)
	}

	return result
}

// The enumeration is the part a mirror adds, and it is the part that cannot be
// tested against a stand-in: whether a registry answers a catalog and a tag
// listing the way this expects is a property of the registry.
func TestMirrorAPrefixOfARegistry(t *testing.T) {
	host := serve(t)

	perception := pushTo(t, host, "dist/perception", "1.4.2")
	pushTo(t, host, "dist/perception", "1.4.1")
	pushTo(t, host, "dist/nested/control", "2.0.0")
	pushTo(t, host, "external/ghcr.io/foo/bar", "1.0")

	store := blobstore.NewMemory()

	result := mirrorRegistry(t, host, store)

	if result.Repositories != 2 {
		t.Errorf("mirrored %d repositories, want 2", result.Repositories)
	}
	if result.Copied != 3 {
		t.Errorf("copied %d tags, want 3", result.Copied)
	}
	if len(result.Failures) != 0 {
		t.Errorf("failures: %v", result.Failures)
	}

	digest, err := perception.Digest()
	if err != nil {
		t.Fatalf("digest: %v", err)
	}

	v := &verify.Verifier{Store: store, Repository: "dist/perception"}
	report, err := v.Tag(context.Background(), "1.4.2")
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !report.OK() {
		t.Fatalf("the mirrored tag is not servable: %v", report.Problems)
	}
	if report.Digest != digest.String() {
		t.Errorf("published digest %s, want %s", report.Digest, digest.String())
	}

	// A repository outside the prefix is not the mirror's business, and a
	// bucket the fleet pulls from is exactly the wrong place to be generous.
	if obj, err := store.Stat(context.Background(), "external/ghcr.io/foo/bar/manifests/1.0"); err != nil {
		t.Fatalf("stat: %v", err)
	} else if obj != nil {
		t.Error("a repository outside the prefix was mirrored")
	}

}

// The second run asks the source where each tag points and the bucket whether
// it is already there, finds they agree, and stops. Nothing is remembered
// between runs, and nothing needs to be.
func TestMirrorSkipsWhatItAlreadyCopied(t *testing.T) {
	host := serve(t)

	pushTo(t, host, "dist/perception", "1.4.2")
	pushTo(t, host, "dist/control", "2.0.0")

	store := blobstore.NewMemory()

	mirrorRegistry(t, host, store)
	writes := len(store.Writes)

	result := mirrorRegistry(t, host, store)

	if result.Copied != 0 {
		t.Errorf("the second run copied %d tags, want 0", result.Copied)
	}
	if result.Skipped != 2 {
		t.Errorf("the second run skipped %d tags, want 2", result.Skipped)
	}
	if got := len(store.Writes); got != writes {
		t.Errorf("the second run wrote %d objects, want none", got-writes)
	}
}
