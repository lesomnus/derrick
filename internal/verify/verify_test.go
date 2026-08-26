package verify_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"github.com/lesomnus/derrick/internal/blobstore"
	"github.com/lesomnus/derrick/internal/layout"
	"github.com/lesomnus/derrick/internal/verify"
)

const repository = "robot/perception"

type bucket struct {
	t     *testing.T
	store *blobstore.Memory
}

func newBucket(t *testing.T) *bucket {
	t.Helper()

	return &bucket{t: t, store: blobstore.NewMemory()}
}

func (b *bucket) put(key string, body []byte, opts blobstore.PutOptions) {
	b.t.Helper()

	if err := b.store.Put(context.Background(), key, bytes.NewReader(body), opts); err != nil {
		b.t.Fatalf("put %q: %v", key, err)
	}
}

func (b *bucket) blob(content string) layout.Descriptor {
	b.t.Helper()

	digest := digestOf([]byte(content))
	b.put(layout.BlobKey(repository, digest), []byte(content), blobstore.PutOptions{})

	return layout.Descriptor{
		MediaType: "application/vnd.oci.image.layer.v1.tar+gzip",
		Digest:    digest,
		Size:      int64(len(content)),
	}
}

// manifest stores a manifest under its digest and returns its descriptor.
func (b *bucket) manifest(m layout.Manifest) layout.Descriptor {
	b.t.Helper()

	m.SchemaVersion = 2
	raw, err := json.Marshal(m)
	if err != nil {
		b.t.Fatalf("marshal: %v", err)
	}
	digest := digestOf(raw)
	b.put(layout.ManifestKey(repository, digest), raw, blobstore.PutOptions{ContentType: m.MediaType})

	return layout.Descriptor{MediaType: m.MediaType, Digest: digest, Size: int64(len(raw))}
}

// tag stores the tagged copy of a manifest that is already in the bucket.
func (b *bucket) tag(name string, descriptor layout.Descriptor, metadata map[string]string) {
	b.t.Helper()

	raw, ok := b.store.Body(layout.ManifestKey(repository, descriptor.Digest))
	if !ok {
		b.t.Fatalf("manifest %s is not in the bucket", descriptor.Digest)
	}
	b.put(layout.ManifestKey(repository, name), raw, blobstore.PutOptions{
		ContentType: descriptor.MediaType,
		Metadata:    metadata,
	})
}

func (b *bucket) verify(tag string) *verify.Report {
	b.t.Helper()

	v := &verify.Verifier{Store: b.store, Repository: repository}
	report, err := v.Tag(context.Background(), tag)
	if err != nil {
		b.t.Fatalf("Tag: %v", err)
	}

	return report
}

func digestOf(raw []byte) string {
	sum := sha256.Sum256(raw)

	return "sha256:" + hex.EncodeToString(sum[:])
}

func image(b *bucket, layers ...string) layout.Descriptor {
	config := b.blob("config-" + strings.Join(layers, "-"))
	config.MediaType = "application/vnd.oci.image.config.v1+json"

	descriptors := make([]layout.Descriptor, 0, len(layers))
	for _, layer := range layers {
		descriptors = append(descriptors, b.blob(layer))
	}

	return b.manifest(layout.Manifest{
		MediaType: "application/vnd.oci.image.manifest.v1+json",
		Config:    &config,
		Layers:    descriptors,
	})
}

func TestCompleteImage(t *testing.T) {
	b := newBucket(t)
	root := image(b, "layer-one", "layer-two")
	b.tag("1.4.2", root, map[string]string{layout.DigestMetadataKey: root.Digest})

	report := b.verify("1.4.2")
	if !report.OK() {
		t.Fatalf("a complete image reported problems: %v", report.Problems)
	}
	if report.Digest != root.Digest {
		t.Errorf("Digest = %q, want %q", report.Digest, root.Digest)
	}
	if report.Blobs != 3 {
		t.Errorf("Blobs = %d, want 3", report.Blobs)
	}
	if report.Manifests != 1 {
		t.Errorf("Manifests = %d, want 1", report.Manifests)
	}
}

func TestCompleteIndex(t *testing.T) {
	b := newBucket(t)
	amd64 := image(b, "amd64")
	arm64 := image(b, "arm64")
	index := b.manifest(layout.Manifest{
		MediaType: "application/vnd.oci.image.index.v1+json",
		Manifests: []layout.Descriptor{amd64, arm64},
	})
	b.tag("1.4.2", index, map[string]string{layout.DigestMetadataKey: index.Digest})

	report := b.verify("1.4.2")
	if !report.OK() {
		t.Fatalf("a complete index reported problems: %v", report.Problems)
	}
	if report.Manifests != 3 {
		t.Errorf("Manifests = %d, want 3 (an index and two children)", report.Manifests)
	}
}

func TestUnpublishedTag(t *testing.T) {
	b := newBucket(t)
	image(b, "layer")

	report := b.verify("1.4.2")
	if report.OK() {
		t.Fatal("a tag that was never written reported no problems")
	}
}

// The failure this whole metadata key exists to prevent. Without it the
// registry serves "sha256:" with nothing after it and the pull fails on an
// unparseable digest, with nothing pointing at the cause.
func TestTagWithoutDigestMetadata(t *testing.T) {
	b := newBucket(t)
	root := image(b, "layer")
	b.tag("1.4.2", root, nil)

	report := b.verify("1.4.2")
	if report.OK() {
		t.Fatal("a tag with no digest metadata reported no problems")
	}
	if !strings.Contains(strings.Join(report.Problems, "\n"), layout.DigestMetadataKey) {
		t.Errorf("the problem does not mention the missing metadata: %v", report.Problems)
	}
}

func TestTagWithWrongDigestMetadata(t *testing.T) {
	b := newBucket(t)
	root := image(b, "layer")
	other := image(b, "something-else")
	b.tag("1.4.2", root, map[string]string{layout.DigestMetadataKey: other.Digest})

	report := b.verify("1.4.2")
	if report.OK() {
		t.Fatal("a tag recording the wrong digest reported no problems")
	}
}

// A publish that stopped between blobs and manifests, or a blob deleted by an
// over-eager collection, looks healthy from the outside until something pulls.
func TestMissingBlob(t *testing.T) {
	b := newBucket(t)

	config := layout.Descriptor{
		MediaType: "application/vnd.oci.image.config.v1+json",
		Digest:    digestOf([]byte("absent-config")),
		Size:      13,
	}
	root := b.manifest(layout.Manifest{
		MediaType: "application/vnd.oci.image.manifest.v1+json",
		Config:    &config,
		Layers:    []layout.Descriptor{b.blob("present-layer")},
	})
	b.tag("1.4.2", root, map[string]string{layout.DigestMetadataKey: root.Digest})

	report := b.verify("1.4.2")
	if report.OK() {
		t.Fatal("a missing config blob reported no problems")
	}
	if !strings.Contains(strings.Join(report.Problems, "\n"), config.Digest) {
		t.Errorf("the problem does not name the missing blob: %v", report.Problems)
	}
}

func TestTruncatedBlob(t *testing.T) {
	b := newBucket(t)
	layer := b.blob("a-layer")
	config := b.blob("config")
	config.MediaType = "application/vnd.oci.image.config.v1+json"

	// Claim a size the stored object does not have, which is what a transfer
	// that died halfway leaves behind.
	layer.Size += 100

	root := b.manifest(layout.Manifest{
		MediaType: "application/vnd.oci.image.manifest.v1+json",
		Config:    &config,
		Layers:    []layout.Descriptor{layer},
	})
	b.tag("1.4.2", root, map[string]string{layout.DigestMetadataKey: root.Digest})

	report := b.verify("1.4.2")
	if report.OK() {
		t.Fatal("a truncated blob reported no problems")
	}
}

func TestMissingChildManifest(t *testing.T) {
	b := newBucket(t)
	amd64 := image(b, "amd64")
	absent := layout.Descriptor{
		MediaType: "application/vnd.oci.image.manifest.v1+json",
		Digest:    digestOf([]byte("never-published")),
		Size:      99,
	}
	index := b.manifest(layout.Manifest{
		MediaType: "application/vnd.oci.image.index.v1+json",
		Manifests: []layout.Descriptor{amd64, absent},
	})
	b.tag("1.4.2", index, map[string]string{layout.DigestMetadataKey: index.Digest})

	report := b.verify("1.4.2")
	if report.OK() {
		t.Fatal("an index naming a manifest that is not there reported no problems")
	}
}

func TestMissingReferrerDescriptor(t *testing.T) {
	b := newBucket(t)
	root := image(b, "layer")

	config := b.blob("{}")
	config.MediaType = "application/vnd.oci.empty.v1+json"
	signature := b.manifest(layout.Manifest{
		MediaType:    "application/vnd.oci.image.manifest.v1+json",
		ArtifactType: "application/vnd.dev.cosign.artifact.sig.v1+json",
		Config:       &config,
		Subject:      &root,
	})
	// The signature manifest is present but nothing attaches it to its
	// subject, so the referrers API will not report it and cosign will not
	// find it.
	b.tag("sig", signature, map[string]string{layout.DigestMetadataKey: signature.Digest})

	report := b.verify("sig")
	if report.OK() {
		t.Fatal("a referrer with no descriptor object reported no problems")
	}
	if !strings.Contains(strings.Join(report.Problems, "\n"), "_referrers") {
		t.Errorf("the problem does not name the missing descriptor: %v", report.Problems)
	}
}
