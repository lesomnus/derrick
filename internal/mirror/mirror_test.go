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
	"sync"
	"sync/atomic"
	"testing"

	"github.com/lesomnus/derrick/internal/blobstore"
	"github.com/lesomnus/derrick/internal/layout"
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
	newMirror := func() *mirror.Mirror {
		return &mirror.Mirror{Registry: registry, Store: store, Bucket: "registry-test", Prefix: "dist"}
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
	// A tag that has not moved costs the HEAD that resolves it in the source
	// and the HEAD that finds it in the bucket, and nothing more: the image
	// behind it is never fetched.
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
	newMirror := func() *mirror.Mirror {
		return &mirror.Mirror{Registry: registry, Store: store, Bucket: "registry-test", Prefix: "dist"}
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

// Nothing is remembered between runs, so a bucket somebody else filled — a
// `derrick copy`, a promote — is skipped for the same reason a bucket this
// mirror filled is.
func TestABucketFilledBySomethingElseIsNotCopiedAgain(t *testing.T) {
	registry := newFakeRegistry()
	repo := registry.repository(t, "dist/perception", "1.4.2")

	store := blobstore.NewMemory()
	run(t, &mirror.Mirror{Registry: registry, Store: store, Bucket: "registry-test", Prefix: "dist"})
	fetches := repo.fetches.Load()

	result := run(t, &mirror.Mirror{Registry: registry, Store: store, Bucket: "registry-test", Prefix: "dist"})

	if result.Copied != 0 {
		t.Fatalf("copied %d tags into a bucket that already had them, want 0", result.Copied)
	}
	if got := repo.fetches.Load(); got != fetches {
		t.Fatalf("fetched %d manifests to decide a tag was already there, want none", got-fetches)
	}
}

// Asking the bucket rather than a record is what makes this come out right on
// its own: there is nothing to reconcile, because there was never a second
// opinion.
func TestATagDeletedFromTheBucketIsCopiedAgain(t *testing.T) {
	registry := newFakeRegistry()
	registry.repository(t, "dist/perception", "1.4.2")

	store := blobstore.NewMemory()
	run(t, &mirror.Mirror{Registry: registry, Store: store, Bucket: "registry-test", Prefix: "dist"})

	// Something removed the tag object.
	forgetful := &forgetfulStore{Memory: store, missing: "dist/perception/manifests/1.4.2"}

	result := run(t, &mirror.Mirror{
		Registry: registry,
		Store:    forgetful,
		Bucket:   "registry-test",
		Prefix:   "dist",
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
	result := run(t, &mirror.Mirror{
		Registry: registry,
		Store:    store,
		Bucket:   "registry-test",
		Prefix:   "dist",
		DryRun:   true,
	})

	if result.Copied != 1 {
		t.Fatalf("reported %d tags to copy, want 1", result.Copied)
	}
	if len(store.Writes) != 0 {
		t.Fatalf("a dry run wrote %v", store.Writes)
	}
}

// remove deletes a tag from the source, the way a registry's own retention
// would.
func (f *fakeRepository) remove(tag string) {
	delete(f.byTag, tag)
}

func TestPruneRemovesATagTheSourceNoLongerHas(t *testing.T) {
	registry := newFakeRegistry()
	repo := registry.repository(t, "dist/perception", "1.4.1", "1.4.2")

	store := blobstore.NewMemory()
	run(t, &mirror.Mirror{Registry: registry, Store: store, Bucket: "registry-test", Prefix: "dist"})

	repo.remove("1.4.1")

	result := run(t, &mirror.Mirror{
		Registry: registry, Store: store, Bucket: "registry-test", Prefix: "dist", Prune: true,
	})

	if result.Pruned != 1 {
		t.Fatalf("pruned %d tags, want 1", result.Pruned)
	}
	if _, ok := store.Body("dist/perception/manifests/1.4.1"); ok {
		t.Error("the tag object is still in the bucket")
	}
	if _, ok := store.Body("dist/perception/manifests/1.4.2"); !ok {
		t.Error("the tag that is still in the source was pruned")
	}

	// Only the tag object goes. What it was holding up is a question about the
	// whole repository, which is what `derrick prune` answers.
	var manifests int
	for _, key := range store.Keys() {
		if strings.Contains(key, "/manifests/sha256:") {
			manifests++
		}
	}
	if manifests != 2 {
		t.Errorf("the bucket holds %d manifests, want both: pruning a tag is not pruning an image", manifests)
	}
}

func TestPruneLeavesAttachmentTagsAlone(t *testing.T) {
	registry := newFakeRegistry()
	repo := registry.repository(t, "dist/perception", "1.4.2")
	subject := repo.byTag["1.4.2"]
	signature := strings.Replace(subject.Digest, ":", "-", 1) + ".sig"
	repo.tag(t, signature, "a-signature")

	store := blobstore.NewMemory()
	run(t, &mirror.Mirror{Registry: registry, Store: store, Bucket: "registry-test", Prefix: "dist"})

	result := run(t, &mirror.Mirror{
		Registry: registry, Store: store, Bucket: "registry-test", Prefix: "dist", Prune: true,
	})

	// The copier published it, the walk skipped it, and the source still has
	// it. Comparing against the tags this run copied rather than against the
	// source listing would delete every signature in the repository.
	if result.Pruned != 0 {
		t.Errorf("pruned %d tags, want 0", result.Pruned)
	}
	if _, ok := store.Body("dist/perception/manifests/" + signature); !ok {
		t.Error("the signature tag was pruned")
	}
}

func TestPruneLeavesAnExcludedTagAlone(t *testing.T) {
	registry := newFakeRegistry()
	registry.repository(t, "dist/perception", "1.4.2", "r0")

	store := blobstore.NewMemory()

	// Published before anybody decided not to publish it.
	run(t, &mirror.Mirror{Registry: registry, Store: store, Bucket: "registry-test", Prefix: "dist"})

	result := run(t, &mirror.Mirror{
		Registry: registry, Store: store, Bucket: "registry-test", Prefix: "dist",
		Prune: true, ExcludeTags: []string{"r0"},
	})

	// Excluding a tag says do not publish it, not take it away. `derrick
	// untag` is what takes it away.
	if result.Pruned != 0 {
		t.Errorf("pruned %d tags, want 0", result.Pruned)
	}
	if _, ok := store.Body("dist/perception/manifests/r0"); !ok {
		t.Error("an excluded tag was pruned")
	}
}

func TestPruneRefusesWhenTheSourceListsNothing(t *testing.T) {
	registry := newFakeRegistry()
	repo := registry.repository(t, "dist/perception", "1.4.2")

	store := blobstore.NewMemory()
	run(t, &mirror.Mirror{Registry: registry, Store: store, Bucket: "registry-test", Prefix: "dist"})

	// A registry answering with nothing looks exactly like a repository that
	// is empty, and one of those is a reason to delete every tag we have.
	repo.remove("1.4.2")

	result := run(t, &mirror.Mirror{
		Registry: registry, Store: store, Bucket: "registry-test", Prefix: "dist", Prune: true,
	})

	if result.Pruned != 0 {
		t.Errorf("pruned %d tags from a repository that listed none, want 0", result.Pruned)
	}
	if _, ok := store.Body("dist/perception/manifests/1.4.2"); !ok {
		t.Error("a tag was pruned on the word of an empty listing")
	}
}

func TestADryRunPrunesNothing(t *testing.T) {
	registry := newFakeRegistry()
	repo := registry.repository(t, "dist/perception", "1.4.1", "1.4.2")

	store := blobstore.NewMemory()
	run(t, &mirror.Mirror{Registry: registry, Store: store, Bucket: "registry-test", Prefix: "dist"})

	repo.remove("1.4.1")

	log := &recorder{}
	result := run(t, &mirror.Mirror{
		Registry: registry, Store: store, Bucket: "registry-test", Prefix: "dist",
		Prune: true, DryRun: true, Log: log.Log,
	})

	if result.Pruned != 1 {
		t.Fatalf("reported %d tags to prune, want 1", result.Pruned)
	}
	if _, ok := store.Body("dist/perception/manifests/1.4.1"); !ok {
		t.Error("a dry run deleted the tag object")
	}
	if text := log.text(); !strings.Contains(text, "would remove") || strings.Contains(text, "; removing") {
		t.Errorf("a dry run said it removed something:\n%s", text)
	}
}

// recorder is a Logger that keeps what it was told. It locks because a mirror
// examines several tags at once.
type recorder struct {
	mu    sync.Mutex
	taken []string
}

func (r *recorder) Log(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.taken = append(r.taken, fmt.Sprintf(format, args...))
}

func (r *recorder) text() string {
	r.mu.Lock()
	defer r.mu.Unlock()

	return strings.Join(r.taken, "\n")
}

func (r *recorder) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.taken = nil
}

// A run that copies nothing should say what it decided about every tag, not
// only how many there were. The counts are the summary; the log is the record
// of what is in the bucket and why.
func TestEveryTagIsAccountedForInTheLog(t *testing.T) {
	registry := newFakeRegistry()
	repo := registry.repository(t, "dist/perception", "1.4.1", "1.4.2", "r0")
	subject := repo.byTag["1.4.2"]
	signature := strings.Replace(subject.Digest, ":", "-", 1) + ".sig"
	repo.tag(t, signature, "a-signature")

	store := blobstore.NewMemory()
	log := &recorder{}
	newMirror := func() *mirror.Mirror {
		return &mirror.Mirror{
			Registry:    registry,
			Store:       store,
			Bucket:      "registry-test",
			Prefix:      "dist",
			ExcludeTags: []string{"r0"},
			Log:         log.Log,
		}
	}

	run(t, newMirror())

	log.reset()
	result := run(t, newMirror())

	if result.Copied != 0 {
		t.Fatalf("the second run copied %d tags, want 0", result.Copied)
	}

	text := log.text()
	for _, tag := range []string{"1.4.1", "1.4.2", "r0", signature} {
		if !strings.Contains(text, "dist/perception:"+tag) {
			t.Errorf("a run that copied nothing does not account for %s:\n%s", tag, text)
		}
	}
}

// Two tags that are the same image is the ordinary case, not a corner: a
// release tag and the build tag it was cut from point at one manifest. Copying
// them at once must not mean writing every blob under them twice, which an
// object store is entitled to refuse and R2 does.
func TestABlobSharedBetweenTagsIsWrittenOnce(t *testing.T) {
	registry := newFakeRegistry()
	repo := registry.repository(t, "dist/perception")
	repo.tag(t, "1.4.2", "the-same-image")
	repo.tag(t, "stable", "the-same-image")
	repo.tag(t, "r33152680499", "the-same-image")

	store := blobstore.NewMemory()
	run(t, &mirror.Mirror{
		Registry: registry,
		Store:    store,
		Bucket:   "registry-test",
		Prefix:   "dist",
		Parallel: 4,
	})

	layer := layout.BlobKey("dist/perception", digestOf([]byte("the-same-image")))

	written := 0
	for _, key := range store.Writes {
		if key == layer {
			written++
		}
	}
	if written != 1 {
		t.Errorf("the layer under three tags was written %d times, want 1", written)
	}

	// The tags themselves are three objects, and each of them is written.
	for _, tag := range []string{"1.4.2", "stable", "r33152680499"} {
		if _, ok := store.Body(layout.ManifestKey("dist/perception", tag)); !ok {
			t.Errorf("%s was not published", tag)
		}
	}
}
