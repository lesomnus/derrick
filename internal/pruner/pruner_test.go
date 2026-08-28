package pruner_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/lesomnus/derrick/internal/blobstore"
	"github.com/lesomnus/derrick/internal/layout"
	"github.com/lesomnus/derrick/internal/pruner"
)

const repository = "dist/perception"

var now = time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)

// bucket writes the layout by hand, so that what the pruner walks is keys and
// object metadata rather than anything it and the copier agree on privately.
type bucket struct {
	*blobstore.Memory
	t *testing.T
}

func newBucket(t *testing.T) *bucket {
	t.Helper()

	store := blobstore.NewMemory()
	// Old enough that the grace period is not what any of these tests is
	// about; the one that is says so.
	store.Clock = func() time.Time { return now.Add(-30 * 24 * time.Hour) }

	return &bucket{Memory: store, t: t}
}

func (b *bucket) put(key, body, contentType string, metadata map[string]string) {
	b.t.Helper()

	if err := b.Put(context.Background(), key, strings.NewReader(body), blobstore.PutOptions{
		ContentType: contentType,
		Metadata:    metadata,
	}); err != nil {
		b.t.Fatalf("put %s: %v", key, err)
	}
}

func (b *bucket) blob(content string) layout.Descriptor {
	b.t.Helper()

	digest := digestOf([]byte(content))
	b.put(layout.BlobKey(repository, digest), content, "", nil)

	return layout.Descriptor{
		MediaType: "application/vnd.oci.image.layer.v1.tar+gzip",
		Digest:    digest,
		Size:      int64(len(content)),
	}
}

// image writes a manifest and the blobs it names, and returns its digest.
func (b *bucket) image(layers ...string) string {
	b.t.Helper()

	config := b.blob("config-" + strings.Join(layers, "-"))
	descriptors := make([]layout.Descriptor, 0, len(layers))
	for _, layer := range layers {
		descriptors = append(descriptors, b.blob(layer))
	}

	return b.manifest(layout.Manifest{
		SchemaVersion: 2,
		MediaType:     "application/vnd.oci.image.manifest.v1+json",
		Config:        &config,
		Layers:        descriptors,
	})
}

func (b *bucket) index(children ...string) string {
	b.t.Helper()

	descriptors := make([]layout.Descriptor, 0, len(children))
	for _, child := range children {
		descriptors = append(descriptors, layout.Descriptor{
			MediaType: "application/vnd.oci.image.manifest.v1+json",
			Digest:    child,
		})
	}

	return b.manifest(layout.Manifest{
		SchemaVersion: 2,
		MediaType:     "application/vnd.oci.image.index.v1+json",
		Manifests:     descriptors,
	})
}

// signature writes a manifest that hangs off subject, the way cosign's does.
func (b *bucket) signature(subject string) string {
	b.t.Helper()

	config := b.blob("signature-config-" + subject)
	payload := b.blob("signature-payload-" + subject)

	digest := b.manifest(layout.Manifest{
		SchemaVersion: 2,
		MediaType:     "application/vnd.oci.image.manifest.v1+json",
		Config:        &config,
		Layers:        []layout.Descriptor{payload},
		Subject:       &layout.Descriptor{Digest: subject},
	})

	descriptor, err := json.Marshal(layout.Descriptor{
		MediaType: "application/vnd.oci.image.manifest.v1+json",
		Digest:    digest,
	})
	if err != nil {
		b.t.Fatalf("marshal descriptor: %v", err)
	}
	b.put(layout.ReferrerKey(repository, subject, digest), string(descriptor), layout.ReferrerContentType, nil)

	return digest
}

func (b *bucket) manifest(m layout.Manifest) string {
	b.t.Helper()

	raw, err := json.Marshal(m)
	if err != nil {
		b.t.Fatalf("marshal manifest: %v", err)
	}
	digest := digestOf(raw)
	b.put(layout.ManifestKey(repository, digest), string(raw), m.MediaType, nil)

	return digest
}

func (b *bucket) tag(name, digest string) {
	b.t.Helper()

	raw, ok := b.Body(layout.ManifestKey(repository, digest))
	if !ok {
		b.t.Fatalf("no manifest %s to tag", digest)
	}
	b.put(layout.ManifestKey(repository, name), string(raw), "application/vnd.oci.image.manifest.v1+json",
		map[string]string{layout.DigestMetadataKey: digest})
}

func (b *bucket) untag(name string) {
	b.t.Helper()

	if err := b.Delete(context.Background(), layout.ManifestKey(repository, name)); err != nil {
		b.t.Fatalf("untag %s: %v", name, err)
	}
}

func digestOf(raw []byte) string {
	sum := sha256.Sum256(raw)

	return "sha256:" + hex.EncodeToString(sum[:])
}

func run(t *testing.T, b *bucket, apply bool) *pruner.Result {
	t.Helper()

	p := &pruner.Pruner{
		Store:      b.Memory,
		Repository: repository,
		Apply:      apply,
		Now:        func() time.Time { return now },
		Log:        func(format string, args ...any) { t.Logf(format, args...) },
	}

	result, err := p.Run(context.Background())
	if err != nil {
		t.Fatalf("prune: %v", err)
	}

	return result
}

func TestNothingUnreachableIsDeleted(t *testing.T) {
	b := newBucket(t)
	b.tag("1.4.2", b.image("layer-one", "layer-two"))

	before := len(b.Keys())
	result := run(t, b, true)

	if result.Deleted.Total() != 0 {
		t.Errorf("deleted %d objects from a repository where everything is reachable", result.Deleted.Total())
	}
	if got := len(b.Keys()); got != before {
		t.Errorf("the bucket went from %d keys to %d", before, got)
	}
}

// The case the whole thing exists for: an untagged image goes, and the layers
// it shared with one that is still tagged stay.
func TestAnUntaggedImageIsReclaimedButSharedLayersStay(t *testing.T) {
	b := newBucket(t)

	old := b.image("shared-layer", "old-layer")
	current := b.image("shared-layer", "new-layer")
	b.tag("1.4.1", old)
	b.tag("1.4.2", current)

	b.untag("1.4.1")

	result := run(t, b, true)

	if result.Deleted.Manifests != 1 {
		t.Errorf("deleted %d manifests, want the untagged one", result.Deleted.Manifests)
	}
	// Its own config and its exclusive layer; the shared layer is still named
	// by the tagged image.
	if result.Deleted.Blobs != 2 {
		t.Errorf("deleted %d blobs, want 2", result.Deleted.Blobs)
	}

	if _, ok := b.Body(layout.ManifestKey(repository, old)); ok {
		t.Error("the untagged manifest is still there")
	}
	if _, ok := b.Body(layout.BlobKey(repository, digestOf([]byte("shared-layer")))); !ok {
		t.Error("a layer the tagged image still needs was deleted")
	}
	if _, ok := b.Body(layout.BlobKey(repository, digestOf([]byte("old-layer")))); ok {
		t.Error("the layer only the untagged image used is still there")
	}
	if _, ok := b.Body(layout.ManifestKey(repository, current)); !ok {
		t.Error("the tagged manifest was deleted")
	}
}

// A publish writes blobs before the manifest that names them, so a young
// unreferenced blob is not garbage; it is an image arriving.
func TestAPublishInFlightIsNotGarbage(t *testing.T) {
	b := newBucket(t)
	b.tag("1.4.2", b.image("layer-one"))

	arriving := b.blob("a-layer-whose-manifest-has-not-landed")
	b.SetModified(layout.BlobKey(repository, arriving.Digest), now.Add(-5*time.Minute))

	result := run(t, b, true)

	if result.Deleted.Blobs != 0 {
		t.Errorf("deleted %d blobs, want 0: an unreferenced blob written five minutes ago is a publish in flight", result.Deleted.Blobs)
	}
	if result.Withheld.Blobs != 1 {
		t.Errorf("held back %d blobs, want 1", result.Withheld.Blobs)
	}
	if _, ok := b.Body(layout.BlobKey(repository, arriving.Digest)); !ok {
		t.Error("the arriving blob was deleted")
	}
}

func TestWithoutApplyNothingIsWritten(t *testing.T) {
	b := newBucket(t)
	b.tag("1.4.2", b.image("layer-one"))
	b.tag("keep", b.image("another-layer"))
	b.untag("1.4.2")

	before := len(b.Keys())
	result := run(t, b, false)

	if result.Deleted.Total() == 0 {
		t.Fatal("a dry run found nothing to delete after an image was untagged")
	}
	if got := len(b.Keys()); got != before {
		t.Errorf("a dry run changed the bucket: %d keys, was %d", got, before)
	}
}

// A repository with no tags is one whose images are pulled by digest. It is
// not a repository to empty.
func TestARepositoryWithNoTagsIsLeftAlone(t *testing.T) {
	b := newBucket(t)
	b.image("layer-one")

	before := len(b.Keys())
	result := run(t, b, true)

	if result.Deleted.Total() != 0 {
		t.Errorf("deleted %d objects from a repository that has no tags to walk from", result.Deleted.Total())
	}
	if got := len(b.Keys()); got != before {
		t.Errorf("the bucket went from %d keys to %d", before, got)
	}
}

func TestASignatureSurvivesWithItsSubject(t *testing.T) {
	b := newBucket(t)

	image := b.image("layer-one")
	b.tag("1.4.2", image)
	signature := b.signature(image)

	result := run(t, b, true)

	if result.Deleted.Total() != 0 {
		t.Errorf("deleted %d objects, want none: a signature is reachable because its subject is", result.Deleted.Total())
	}
	if _, ok := b.Body(layout.ManifestKey(repository, signature)); !ok {
		t.Error("the signature manifest was deleted")
	}
	if _, ok := b.Body(layout.ReferrerKey(repository, image, signature)); !ok {
		t.Error("the referrer descriptor was deleted")
	}
	if result.Reachable.Referrers != 1 {
		t.Errorf("reached %d referrers, want 1", result.Reachable.Referrers)
	}
}

func TestASignatureGoesWithAnUntaggedSubject(t *testing.T) {
	b := newBucket(t)

	image := b.image("layer-one")
	b.tag("1.4.2", image)
	signature := b.signature(image)
	b.tag("keep", b.image("another-layer"))

	b.untag("1.4.2")

	run(t, b, true)

	if _, ok := b.Body(layout.ReferrerKey(repository, image, signature)); ok {
		t.Error("the referrer descriptor of an untagged image is still there")
	}
	if _, ok := b.Body(layout.ManifestKey(repository, signature)); ok {
		t.Error("the signature manifest of an untagged image is still there")
	}
}

func TestAnIndexKeepsEveryArchitecture(t *testing.T) {
	b := newBucket(t)

	amd64 := b.image("amd64-layer")
	arm64 := b.image("arm64-layer")
	b.tag("multi", b.index(amd64, arm64))

	result := run(t, b, true)

	if result.Deleted.Total() != 0 {
		t.Errorf("deleted %d objects from a multi-architecture image", result.Deleted.Total())
	}
	// The index and its two children.
	if result.Reachable.Manifests != 3 {
		t.Errorf("reached %d manifests, want 3", result.Reachable.Manifests)
	}
}

// Pruning a repository whose graph cannot be read would delete the blobs the
// missing manifest referenced, turning a half-finished publish into a lost
// image.
func TestAMissingManifestStopsTheRun(t *testing.T) {
	b := newBucket(t)

	image := b.image("layer-one")
	b.tag("1.4.2", image)
	if err := b.Delete(context.Background(), layout.ManifestKey(repository, image)); err != nil {
		t.Fatalf("delete: %v", err)
	}

	p := &pruner.Pruner{Store: b.Memory, Repository: repository, Apply: true, Now: func() time.Time { return now }}

	before := len(b.Keys())
	if _, err := p.Run(context.Background()); err == nil {
		t.Fatal("pruning a repository with a missing manifest was allowed")
	}
	if got := len(b.Keys()); got != before {
		t.Errorf("something was deleted anyway: %d keys, was %d", got, before)
	}
}

func TestAnUnknownKeyIsLeftAlone(t *testing.T) {
	b := newBucket(t)
	b.tag("1.4.2", b.image("layer-one"))
	b.put(repository+"/something-else", "written by something that is not derrick", "", nil)

	result := run(t, b, true)

	if len(result.Unknown) != 1 {
		t.Fatalf("reported %d unknown keys, want 1", len(result.Unknown))
	}
	if _, ok := b.Body(repository + "/something-else"); !ok {
		t.Error("a key this layout does not describe was deleted")
	}
}

// `dist/oasys` and `dist/oasys-internal` are two repositories, and one is a
// prefix of the other.
func TestPruningOneRepositoryDoesNotWalkItsNeighbour(t *testing.T) {
	b := newBucket(t)
	b.tag("1.4.2", b.image("layer-one"))

	neighbour := repository + "-internal"
	b.put(layout.BlobKey(neighbour, digestOf([]byte("neighbour-layer"))), "neighbour-layer", "", nil)

	run(t, b, true)

	if _, ok := b.Body(layout.BlobKey(neighbour, digestOf([]byte("neighbour-layer")))); !ok {
		t.Error("pruning a repository deleted from the one whose name it is a prefix of")
	}
}
