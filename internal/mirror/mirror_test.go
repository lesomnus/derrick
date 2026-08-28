package mirror_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/lesomnus/derrick/internal/blobstore"
	"github.com/lesomnus/derrick/internal/layout"
	"github.com/lesomnus/derrick/internal/ledger"
	"github.com/lesomnus/derrick/internal/mirror"
	"github.com/lesomnus/derrick/internal/source"
)

type fakeRegistry struct {
	repositories map[string]*fakeRepository
	catalogErr   error
}

func newFakeRegistry() *fakeRegistry {
	return &fakeRegistry{repositories: map[string]*fakeRepository{}}
}

func (r *fakeRegistry) Name() string {
	return "cr.test"
}

func (r *fakeRegistry) Catalog(context.Context) ([]string, error) {
	if r.catalogErr != nil {
		return nil, r.catalogErr
	}

	names := make([]string, 0, len(r.repositories))
	for name := range r.repositories {
		names = append(names, name)
	}

	return names, nil
}

func (r *fakeRegistry) Repository(_ context.Context, name string) (mirror.Repository, error) {
	repo, ok := r.repositories[name]
	if !ok {
		return nil, fmt.Errorf("no repository %s", name)
	}

	return repo, nil
}

// repository adds a repository holding one single-layer image per tag, where
// the layer content is the tag itself so that every tag is a distinct image.
func (r *fakeRegistry) repository(t *testing.T, name string, tags ...string) *fakeRepository {
	t.Helper()

	repo := &fakeRepository{
		byTag:    map[string]*source.Manifest{},
		byDigest: map[string]*source.Manifest{},
		blobs:    map[string][]byte{},
	}
	r.repositories[name] = repo

	for _, tag := range tags {
		repo.tag(t, tag, name+"/"+tag)
	}

	return repo
}

type fakeRepository struct {
	byTag    map[string]*source.Manifest
	byDigest map[string]*source.Manifest
	blobs    map[string][]byte

	digests atomic.Int64 // how many times a tag was resolved
	fetches atomic.Int64 // how many times a manifest was fetched by tag

	tagsErr   error
	digestErr error
}

func (f *fakeRepository) Tags(context.Context) ([]string, error) {
	if f.tagsErr != nil {
		return nil, f.tagsErr
	}

	tags := make([]string, 0, len(f.byTag))
	for tag := range f.byTag {
		tags = append(tags, tag)
	}

	return tags, nil
}

func (f *fakeRepository) Digest(_ context.Context, tag string) (string, bool, error) {
	f.digests.Add(1)
	if f.digestErr != nil {
		return "", false, f.digestErr
	}

	m, ok := f.byTag[tag]
	if !ok {
		return "", false, nil
	}

	return m.Digest, true, nil
}

func (f *fakeRepository) Manifest(_ context.Context, digest string) (*source.Manifest, error) {
	m, ok := f.byDigest[digest]
	if !ok {
		return nil, fmt.Errorf("no manifest %s", digest)
	}

	return m, nil
}

func (f *fakeRepository) ManifestByTag(_ context.Context, tag string) (*source.Manifest, bool, error) {
	f.fetches.Add(1)
	m, ok := f.byTag[tag]

	return m, ok, nil
}

func (f *fakeRepository) Blob(_ context.Context, digest string) (io.ReadCloser, error) {
	body, ok := f.blobs[digest]
	if !ok {
		return nil, fmt.Errorf("no blob %s", digest)
	}

	return io.NopCloser(bytes.NewReader(body)), nil
}

func (f *fakeRepository) Referrers(context.Context, string) ([]layout.Descriptor, error) {
	return nil, nil
}

// tag points tag at a new image whose layer holds content.
func (f *fakeRepository) tag(t *testing.T, tag, content string) *source.Manifest {
	t.Helper()

	config := f.blob("config-" + content)
	layer := f.blob(content)

	raw, err := json.Marshal(layout.Manifest{
		SchemaVersion: 2,
		MediaType:     "application/vnd.oci.image.manifest.v1+json",
		Config:        &config,
		Layers:        []layout.Descriptor{layer},
	})
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	parsed, err := layout.ParseManifest(raw)
	if err != nil {
		t.Fatalf("parse manifest: %v", err)
	}

	m := &source.Manifest{
		Digest:    digestOf(raw),
		MediaType: "application/vnd.oci.image.manifest.v1+json",
		Size:      int64(len(raw)),
		Raw:       raw,
		Parsed:    parsed,
	}
	f.byDigest[m.Digest] = m
	f.byTag[tag] = m

	return m
}

func (f *fakeRepository) blob(content string) layout.Descriptor {
	digest := digestOf([]byte(content))
	f.blobs[digest] = []byte(content)

	return layout.Descriptor{
		MediaType: "application/vnd.oci.image.layer.v1.tar+gzip",
		Digest:    digest,
		Size:      int64(len(content)),
	}
}

func digestOf(raw []byte) string {
	sum := sha256.Sum256(raw)

	return "sha256:" + hex.EncodeToString(sum[:])
}

func run(t *testing.T, m *mirror.Mirror) *mirror.Result {
	t.Helper()

	result, err := m.Run(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	return result
}

func TestMirrorsOnlyThePrefix(t *testing.T) {
	registry := newFakeRegistry()
	registry.repository(t, "dist/perception", "1.4.1", "1.4.2")
	registry.repository(t, "dist/nested/control", "2.0.0")
	registry.repository(t, "external/ghcr.io/foo/bar", "1.0")
	registry.repository(t, "scratch/wip", "latest")

	store := blobstore.NewMemory()
	result := run(t, &mirror.Mirror{
		Registry: registry,
		Store:    store,
		Bucket:   "registry-test",
		Prefix:   "dist",
		Ledger:   ledger.New(),
	})

	if result.Repositories != 2 {
		t.Fatalf("mirrored %d repositories, want 2", result.Repositories)
	}
	if result.Copied != 3 {
		t.Fatalf("copied %d tags, want 3", result.Copied)
	}

	for _, key := range []string{
		"dist/perception/manifests/1.4.1",
		"dist/perception/manifests/1.4.2",
		"dist/nested/control/manifests/2.0.0",
	} {
		if _, ok := store.Body(key); !ok {
			t.Errorf("%s was not published", key)
		}
	}
	for _, key := range store.Keys() {
		if strings.HasPrefix(key, "external/") || strings.HasPrefix(key, "scratch/") {
			t.Errorf("%s is outside the prefix and was published anyway", key)
		}
	}
}

func TestASecondRunCopiesNothing(t *testing.T) {
	registry := newFakeRegistry()
	repo := registry.repository(t, "dist/perception", "1.4.1", "1.4.2")

	store := blobstore.NewMemory()
	book := ledger.New()
	newMirror := func() *mirror.Mirror {
		return &mirror.Mirror{Registry: registry, Store: store, Bucket: "registry-test", Prefix: "dist", Ledger: book}
	}

	run(t, newMirror())
	writes := len(store.Writes)
	fetches := repo.fetches.Load()

	result := run(t, newMirror())

	if result.Copied != 0 {
		t.Fatalf("the second run copied %d tags, want 0", result.Copied)
	}
	if result.Skipped != 2 {
		t.Fatalf("the second run skipped %d tags, want 2", result.Skipped)
	}
	if got := len(store.Writes); got != writes {
		t.Fatalf("the second run wrote %d objects, want none", got-writes)
	}
	// The whole point of the ledger: a known tag costs the HEAD that resolves
	// it and nothing more.
	if got := repo.fetches.Load(); got != fetches {
		t.Fatalf("the second run fetched %d manifests, want none", got-fetches)
	}
	if got := repo.digests.Load(); got != 4 {
		t.Fatalf("resolved %d tags over two runs, want 4", got)
	}
}

func TestAMovedTagIsCopiedAgain(t *testing.T) {
	registry := newFakeRegistry()
	repo := registry.repository(t, "dist/perception", "stable")

	store := blobstore.NewMemory()
	book := ledger.New()
	newMirror := func() *mirror.Mirror {
		return &mirror.Mirror{Registry: registry, Store: store, Bucket: "registry-test", Prefix: "dist", Ledger: book}
	}

	run(t, newMirror())

	moved := repo.tag(t, "stable", "a-different-image")

	result := run(t, newMirror())
	if result.Copied != 1 {
		t.Fatalf("copied %d tags after the tag moved, want 1", result.Copied)
	}

	obj, err := store.Stat(context.Background(), "dist/perception/manifests/stable")
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := obj.Metadata[layout.DigestMetadataKey]; got != moved.Digest {
		t.Fatalf("the tag object records %s, want %s", got, moved.Digest)
	}

	entry, ok := book.Lookup("dist/perception", "stable")
	if !ok {
		t.Fatal("the moved tag left no ledger entry")
	}
	if entry.Digest != moved.Digest {
		t.Fatalf("the ledger records %s, want %s", entry.Digest, moved.Digest)
	}
}

func TestAttachmentTagsAreNotMirroredOnTheirOwn(t *testing.T) {
	registry := newFakeRegistry()
	repo := registry.repository(t, "dist/perception", "1.4.2")
	subject := repo.byTag["1.4.2"]
	repo.tag(t, strings.Replace(subject.Digest, ":", "-", 1)+".sig", "a-signature")

	store := blobstore.NewMemory()
	result := run(t, &mirror.Mirror{
		Registry: registry,
		Store:    store,
		Bucket:   "registry-test",
		Prefix:   "dist",
		Ledger:   ledger.New(),
	})

	if result.Copied != 1 {
		t.Fatalf("copied %d tags, want 1", result.Copied)
	}
	if result.Excluded != 1 {
		t.Fatalf("excluded %d tags, want 1", result.Excluded)
	}

	// Excluded from the walk, and published anyway: the copier carries an
	// attachment across with the subject it hangs off. Skipping it as a
	// top-level tag saves the second copy, and loses nothing.
	signature := strings.Replace(subject.Digest, ":", "-", 1) + ".sig"
	if _, ok := store.Body("dist/perception/manifests/" + signature); !ok {
		t.Fatal("the signature did not come across with its subject")
	}
}

func TestAlreadyPublishedTagsAreAdoptedIntoTheLedger(t *testing.T) {
	registry := newFakeRegistry()
	repo := registry.repository(t, "dist/perception", "1.4.2")

	store := blobstore.NewMemory()

	// A bucket published into before there was a ledger, by a promote.
	run(t, &mirror.Mirror{Registry: registry, Store: store, Bucket: "registry-test", Prefix: "dist"})
	fetches := repo.fetches.Load()

	book := ledger.New()
	result := run(t, &mirror.Mirror{Registry: registry, Store: store, Bucket: "registry-test", Prefix: "dist", Ledger: book})

	if result.Copied != 0 {
		t.Fatalf("copied %d tags into a bucket that already had them, want 0", result.Copied)
	}
	if got := repo.fetches.Load(); got != fetches {
		t.Fatalf("fetched %d manifests to adopt a published tag, want none", got-fetches)
	}
	if _, ok := book.Lookup("dist/perception", "1.4.2"); !ok {
		t.Fatal("an already-published tag was not recorded, so the next run will look again")
	}
	if _, err := book.Save(context.Background(), store, ledger.DefaultKey); err != nil {
		t.Fatalf("save: %v", err)
	}
	if _, ok := store.Body(ledger.DefaultKey); !ok {
		t.Fatal("the ledger was not written to the bucket")
	}
}

func TestRecheckNoticesAMissingTagObject(t *testing.T) {
	registry := newFakeRegistry()
	registry.repository(t, "dist/perception", "1.4.2")

	store := blobstore.NewMemory()
	book := ledger.New()
	run(t, &mirror.Mirror{Registry: registry, Store: store, Bucket: "registry-test", Prefix: "dist", Ledger: book})

	// Something removed the tag object; the ledger still says it is published.
	forgetful := &forgetfulStore{Memory: store, missing: "dist/perception/manifests/1.4.2"}

	result := run(t, &mirror.Mirror{
		Registry: registry,
		Store:    forgetful,
		Bucket:   "registry-test",
		Prefix:   "dist",
		Ledger:   book,
		Recheck:  true,
	})

	if result.Copied != 1 {
		t.Fatalf("copied %d tags after the bucket lost one, want 1", result.Copied)
	}
}

// forgetfulStore is a store with a hole in it, which is what a tag object
// deleted by hand looks like to a mirror.
type forgetfulStore struct {
	*blobstore.Memory
	missing string
}

func (s *forgetfulStore) Stat(ctx context.Context, key string) (*blobstore.Object, error) {
	if key == s.missing {
		return nil, nil
	}

	return s.Memory.Stat(ctx, key)
}

func TestOneBrokenRepositoryDoesNotStopTheRest(t *testing.T) {
	registry := newFakeRegistry()
	broken := registry.repository(t, "dist/broken", "1.0")
	broken.tagsErr = errors.New("the registry said no")
	registry.repository(t, "dist/perception", "1.4.2")

	store := blobstore.NewMemory()
	result := run(t, &mirror.Mirror{
		Registry: registry,
		Store:    store,
		Bucket:   "registry-test",
		Prefix:   "dist",
		Ledger:   ledger.New(),
	})

	if result.Copied != 1 {
		t.Fatalf("copied %d tags, want the one that was not broken", result.Copied)
	}
	if len(result.Failures) != 1 {
		t.Fatalf("reported %d failures, want 1", len(result.Failures))
	}
	if result.Failures[0].Repository != "dist/broken" {
		t.Fatalf("the failure names %q", result.Failures[0].Repository)
	}
}

func TestExclusions(t *testing.T) {
	registry := newFakeRegistry()
	registry.repository(t, "dist/perception", "1.4.2", "nightly")
	registry.repository(t, "dist/sandbox", "1.0")

	store := blobstore.NewMemory()
	result := run(t, &mirror.Mirror{
		Registry:            registry,
		Store:               store,
		Bucket:              "registry-test",
		Prefix:              "dist",
		Ledger:              ledger.New(),
		ExcludeRepositories: []string{"dist/sandbox"},
		ExcludeTags:         []string{"nightly"},
	})

	if result.Repositories != 1 {
		t.Fatalf("mirrored %d repositories, want 1", result.Repositories)
	}
	if result.Copied != 1 {
		t.Fatalf("copied %d tags, want 1", result.Copied)
	}
	if result.Excluded != 2 {
		t.Fatalf("excluded %d, want 2", result.Excluded)
	}
}

func TestABadPatternIsRefusedBeforeAnythingIsRead(t *testing.T) {
	registry := newFakeRegistry()
	registry.repository(t, "dist/perception", "1.4.2")

	store := blobstore.NewMemory()
	m := &mirror.Mirror{
		Registry:    registry,
		Store:       store,
		Bucket:      "registry-test",
		Prefix:      "dist",
		ExcludeTags: []string{"["},
	}

	if _, err := m.Run(context.Background()); err == nil {
		t.Fatal("a malformed pattern was accepted")
	}
	if len(store.Writes) != 0 {
		t.Fatal("something was written despite the bad pattern")
	}
}

func TestDryRunWritesNothing(t *testing.T) {
	registry := newFakeRegistry()
	registry.repository(t, "dist/perception", "1.4.2")

	store := blobstore.NewMemory()
	book := ledger.New()
	result := run(t, &mirror.Mirror{
		Registry: registry,
		Store:    store,
		Bucket:   "registry-test",
		Prefix:   "dist",
		Ledger:   book,
		DryRun:   true,
	})

	if result.Copied != 1 {
		t.Fatalf("reported %d tags to copy, want 1", result.Copied)
	}
	if len(store.Writes) != 0 {
		t.Fatalf("a dry run wrote %v", store.Writes)
	}
	if book.Len() != 0 {
		t.Fatal("a dry run recorded a tag it did not copy")
	}
}
