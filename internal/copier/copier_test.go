package copier_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/lesomnus/derrick/internal/blobstore"
	"github.com/lesomnus/derrick/internal/copier"
	"github.com/lesomnus/derrick/internal/layout"
	"github.com/lesomnus/derrick/internal/source"
)

const repository = "robot/perception"

type fakeSource struct {
	byDigest  map[string]*source.Manifest
	byTag     map[string]*source.Manifest
	referrers map[string][]layout.Descriptor
	blobs     map[string][]byte
}

func newFakeSource() *fakeSource {
	return &fakeSource{
		byDigest:  map[string]*source.Manifest{},
		byTag:     map[string]*source.Manifest{},
		referrers: map[string][]layout.Descriptor{},
		blobs:     map[string][]byte{},
	}
}

func (f *fakeSource) Manifest(_ context.Context, digest string) (*source.Manifest, error) {
	m, ok := f.byDigest[digest]
	if !ok {
		return nil, fmt.Errorf("no manifest %s", digest)
	}

	return m, nil
}

func (f *fakeSource) ManifestByTag(_ context.Context, tag string) (*source.Manifest, bool, error) {
	m, ok := f.byTag[tag]

	return m, ok, nil
}

func (f *fakeSource) Blob(_ context.Context, digest string) (io.ReadCloser, error) {
	body, ok := f.blobs[digest]
	if !ok {
		return nil, fmt.Errorf("no blob %s", digest)
	}

	return io.NopCloser(bytes.NewReader(body)), nil
}

func (f *fakeSource) Referrers(_ context.Context, digest string) ([]layout.Descriptor, error) {
	return f.referrers[digest], nil
}

func (f *fakeSource) addBlob(t *testing.T, mediaType, content string) layout.Descriptor {
	t.Helper()

	digest := digestOf([]byte(content))
	f.blobs[digest] = []byte(content)

	return layout.Descriptor{MediaType: mediaType, Digest: digest, Size: int64(len(content))}
}

func (f *fakeSource) addManifest(t *testing.T, m layout.Manifest) *source.Manifest {
	t.Helper()

	m.SchemaVersion = 2
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	parsed, err := layout.ParseManifest(raw)
	if err != nil {
		t.Fatalf("parse manifest: %v", err)
	}

	sm := &source.Manifest{
		Digest:    digestOf(raw),
		MediaType: m.MediaType,
		Size:      int64(len(raw)),
		Raw:       raw,
		Parsed:    parsed,
	}
	f.byDigest[sm.Digest] = sm

	return sm
}

// image adds a single-architecture image and returns it.
func (f *fakeSource) image(t *testing.T, layers ...string) *source.Manifest {
	t.Helper()

	config := f.addBlob(t, "application/vnd.oci.image.config.v1+json", "config-"+strings.Join(layers, "-"))
	descriptors := make([]layout.Descriptor, 0, len(layers))
	for _, layer := range layers {
		descriptors = append(descriptors, f.addBlob(t, "application/vnd.oci.image.layer.v1.tar+gzip", layer))
	}

	return f.addManifest(t, layout.Manifest{
		MediaType: "application/vnd.oci.image.manifest.v1+json",
		Config:    &config,
		Layers:    descriptors,
	})
}

func digestOf(raw []byte) string {
	sum := sha256.Sum256(raw)

	return "sha256:" + hex.EncodeToString(sum[:])
}

func run(t *testing.T, src copier.Source, store blobstore.Store, root *source.Manifest, tag string) *copier.Result {
	t.Helper()

	c := &copier.Copier{
		Source:      src,
		Store:       store,
		Repository:  repository,
		Concurrency: 2,
	}

	result, err := c.Run(context.Background(), root, tag)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	return result
}

// The ordering is the contract: nothing may reference an object that is not
// there yet, and the tag is the commit.
func TestCopyWritesBlobsThenManifestThenTag(t *testing.T) {
	src := newFakeSource()
	store := blobstore.NewMemory()
	root := src.image(t, "layer-one", "layer-two")

	result := run(t, src, store, root, "1.4.2")

	manifestAt := store.WriteOrder(layout.ManifestKey(repository, root.Digest))
	tagAt := store.WriteOrder(layout.ManifestKey(repository, "1.4.2"))
	if manifestAt < 0 || tagAt < 0 {
		t.Fatalf("manifest or tag was never written: %v", store.Keys())
	}

	for _, blob := range result.Plan.Blobs {
		at := store.WriteOrder(layout.BlobKey(repository, blob.Digest))
		if at < 0 {
			t.Fatalf("blob %s was never written", blob.Digest)
		}
		if at > manifestAt {
			t.Errorf("blob %s was written after the manifest that names it", blob.Digest)
		}
	}
	if manifestAt > tagAt {
		t.Error("the tag was written before the manifest it points at")
	}
	if tagAt != len(store.Writes)-1 {
		t.Error("the tag is the commit and must be written last")
	}

	if result.BlobsUploaded != 3 {
		t.Errorf("BlobsUploaded = %d, want 3 (a config and two layers)", result.BlobsUploaded)
	}
}

// A tagged manifest is the only object whose key does not carry its digest, so
// the digest has to travel in metadata or the registry serves an unparseable
// Docker-Content-Digest.
func TestCopyTagCarriesDigestAndContentType(t *testing.T) {
	src := newFakeSource()
	store := blobstore.NewMemory()
	root := src.image(t, "layer")

	run(t, src, store, root, "1.4.2")

	obj, err := store.Stat(context.Background(), layout.ManifestKey(repository, "1.4.2"))
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if obj == nil {
		t.Fatal("the tag object is missing")
	}
	if got := obj.Metadata[layout.DigestMetadataKey]; got != root.Digest {
		t.Errorf("tag records digest %q, want %q", got, root.Digest)
	}
	if obj.ContentType != root.MediaType {
		t.Errorf("tag content type = %q, want %q", obj.ContentType, root.MediaType)
	}

	// Byte-for-byte, or every signature over the manifest is invalidated.
	body, _ := store.Body(layout.ManifestKey(repository, "1.4.2"))
	if !bytes.Equal(body, root.Raw) {
		t.Error("the tagged copy is not byte-identical to the manifest")
	}
}

func TestCopyIndexWritesChildrenFirst(t *testing.T) {
	src := newFakeSource()
	store := blobstore.NewMemory()

	amd64 := src.image(t, "amd64-layer")
	arm64 := src.image(t, "arm64-layer")
	index := src.addManifest(t, layout.Manifest{
		MediaType: "application/vnd.oci.image.index.v1+json",
		Manifests: []layout.Descriptor{
			{MediaType: amd64.MediaType, Digest: amd64.Digest, Size: amd64.Size},
			{MediaType: arm64.MediaType, Digest: arm64.Digest, Size: arm64.Size},
		},
	})

	run(t, src, store, index, "1.4.2")

	indexAt := store.WriteOrder(layout.ManifestKey(repository, index.Digest))
	for _, child := range []*source.Manifest{amd64, arm64} {
		at := store.WriteOrder(layout.ManifestKey(repository, child.Digest))
		if at < 0 {
			t.Fatalf("child %s was never written", child.Digest)
		}
		if at > indexAt {
			t.Errorf("child %s was written after the index that names it", child.Digest)
		}
	}
}

func TestCopySkipsBlobsAlreadyPresent(t *testing.T) {
	src := newFakeSource()
	store := blobstore.NewMemory()
	root := src.image(t, "layer-one", "layer-two")

	// Stand in for a previous release that shared a layer.
	shared := root.Parsed.Layers[0]
	body := src.blobs[shared.Digest]
	if err := store.Put(context.Background(), layout.BlobKey(repository, shared.Digest), bytes.NewReader(body), blobstore.PutOptions{}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	writesBefore := len(store.Writes)

	result := run(t, src, store, root, "1.4.2")

	if result.BlobsSkipped != 1 {
		t.Errorf("BlobsSkipped = %d, want 1", result.BlobsSkipped)
	}
	if result.BlobsUploaded != 2 {
		t.Errorf("BlobsUploaded = %d, want 2", result.BlobsUploaded)
	}

	for _, key := range store.Writes[writesBefore:] {
		if key == layout.BlobKey(repository, shared.Digest) {
			t.Error("a blob that was already present was uploaded again")
		}
	}
}

// A signature found through the referrers API has to arrive with a descriptor
// object, or the registry cannot answer the referrers API for it and cosign
// cannot find it.
func TestCopyReferrer(t *testing.T) {
	src := newFakeSource()
	store := blobstore.NewMemory()

	root := src.image(t, "layer")

	sigConfig := src.addBlob(t, "application/vnd.oci.empty.v1+json", "{}")
	sigLayer := src.addBlob(t, "application/vnd.dev.cosign.simplesigning.v1+json", "signature-payload")
	signature := src.addManifest(t, layout.Manifest{
		MediaType:    "application/vnd.oci.image.manifest.v1+json",
		ArtifactType: "application/vnd.dev.cosign.artifact.sig.v1+json",
		Config:       &sigConfig,
		Layers:       []layout.Descriptor{sigLayer},
		Subject:      &layout.Descriptor{MediaType: root.MediaType, Digest: root.Digest, Size: root.Size},
		Annotations:  map[string]string{"dev.cosigndev": "yes"},
	})
	src.referrers[root.Digest] = []layout.Descriptor{
		{MediaType: signature.MediaType, Digest: signature.Digest, Size: signature.Size},
	}

	result := run(t, src, store, root, "1.4.2")

	if result.ReferrersWritten != 1 {
		t.Fatalf("ReferrersWritten = %d, want 1", result.ReferrersWritten)
	}

	key := layout.ReferrerKey(repository, root.Digest, signature.Digest)
	body, ok := store.Body(key)
	if !ok {
		t.Fatalf("%s is missing; stored keys: %v", key, store.Keys())
	}

	var descriptor layout.Descriptor
	if err := json.Unmarshal(body, &descriptor); err != nil {
		t.Fatalf("stored descriptor is not JSON: %v", err)
	}
	if descriptor.Digest != signature.Digest {
		t.Errorf("descriptor digest = %q, want the referring manifest %q", descriptor.Digest, signature.Digest)
	}
	if descriptor.Size != signature.Size {
		t.Errorf("descriptor size = %d, want %d", descriptor.Size, signature.Size)
	}
	if descriptor.ArtifactType != "application/vnd.dev.cosign.artifact.sig.v1+json" {
		t.Errorf("descriptor artifactType = %q", descriptor.ArtifactType)
	}

	// The signature's own blobs must come along, or verifying it fails.
	if _, ok := store.Body(layout.BlobKey(repository, sigLayer.Digest)); !ok {
		t.Error("the signature payload was not copied")
	}

	subjectAt := store.WriteOrder(layout.ManifestKey(repository, root.Digest))
	signatureAt := store.WriteOrder(layout.ManifestKey(repository, signature.Digest))
	if signatureAt < subjectAt {
		t.Error("a referrer was written before its subject")
	}
}

// Cosign v2 attaches through a tag rather than the referrers API, and v3 keeps
// writing that tag as a fallback. A copy that only consulted the referrers API
// would drop those signatures without saying anything.
func TestCopyCosignFallbackTag(t *testing.T) {
	src := newFakeSource()
	store := blobstore.NewMemory()

	root := src.image(t, "layer")

	sigConfig := src.addBlob(t, "application/vnd.oci.empty.v1+json", "{}")
	signature := src.addManifest(t, layout.Manifest{
		MediaType: "application/vnd.oci.image.manifest.v1+json",
		Config:    &sigConfig,
		Layers:    []layout.Descriptor{src.addBlob(t, "application/vnd.dev.cosign.simplesigning.v1+json", "legacy-signature")},
	})

	tags := source.CosignTags(root.Digest)
	src.byTag[tags[0]] = signature

	run(t, src, store, root, "1.4.2")

	if _, ok := store.Body(layout.ManifestKey(repository, tags[0])); !ok {
		t.Fatalf("%s was not written; stored keys: %v", tags[0], store.Keys())
	}
	if _, ok := store.Body(layout.ManifestKey(repository, signature.Digest)); !ok {
		t.Error("the signature manifest itself was not written")
	}

	// The requested tag still lands last: it is what publishes the image.
	if got := store.Writes[len(store.Writes)-1]; got != layout.ManifestKey(repository, "1.4.2") {
		t.Errorf("last write was %q, want the requested tag", got)
	}
}

func TestCopyNoReferrers(t *testing.T) {
	src := newFakeSource()
	store := blobstore.NewMemory()

	root := src.image(t, "layer")
	sigConfig := src.addBlob(t, "application/vnd.oci.empty.v1+json", "{}")
	signature := src.addManifest(t, layout.Manifest{
		MediaType: "application/vnd.oci.image.manifest.v1+json",
		Config:    &sigConfig,
		Subject:   &layout.Descriptor{MediaType: root.MediaType, Digest: root.Digest, Size: root.Size},
	})
	src.referrers[root.Digest] = []layout.Descriptor{
		{MediaType: signature.MediaType, Digest: signature.Digest, Size: signature.Size},
	}

	c := &copier.Copier{Source: src, Store: store, Repository: repository, NoReferrers: true}
	if _, err := c.Run(context.Background(), root, "1.4.2"); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if _, ok := store.Body(layout.ManifestKey(repository, signature.Digest)); ok {
		t.Error("--no-referrers still copied the signature")
	}
}

func TestCopyLeavesForeignLayersBehind(t *testing.T) {
	src := newFakeSource()
	store := blobstore.NewMemory()

	config := src.addBlob(t, "application/vnd.oci.image.config.v1+json", "config")
	local := src.addBlob(t, "application/vnd.oci.image.layer.v1.tar+gzip", "local-layer")
	foreign := layout.Descriptor{
		MediaType: "application/vnd.docker.image.rootfs.foreign.diff.tar.gzip",
		Digest:    digestOf([]byte("foreign")),
		Size:      7,
		URLs:      []string{"https://example.test/foreign"},
	}
	root := src.addManifest(t, layout.Manifest{
		MediaType: "application/vnd.oci.image.manifest.v1+json",
		Config:    &config,
		Layers:    []layout.Descriptor{local, foreign},
	})

	result := run(t, src, store, root, "1.4.2")

	if len(result.Plan.Foreign) != 1 {
		t.Errorf("Plan.Foreign has %d entries, want 1", len(result.Plan.Foreign))
	}
	if _, ok := store.Body(layout.BlobKey(repository, foreign.Digest)); ok {
		t.Error("a foreign layer was copied; it is not in the source registry to copy")
	}
	if _, ok := store.Body(layout.BlobKey(repository, local.Digest)); !ok {
		t.Error("the distributable layer was not copied")
	}
}

func TestDryRunWritesNothing(t *testing.T) {
	src := newFakeSource()
	store := blobstore.NewMemory()
	root := src.image(t, "layer")

	c := &copier.Copier{Source: src, Store: store, Repository: repository, DryRun: true}
	result, err := c.Run(context.Background(), root, "1.4.2")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(store.Writes) != 0 {
		t.Errorf("a dry run wrote %v", store.Writes)
	}
	if result.BlobsUploaded != 2 {
		t.Errorf("BlobsUploaded = %d, want it to still report what it would do", result.BlobsUploaded)
	}
}

func TestVerifyAfterCopy(t *testing.T) {
	src := newFakeSource()
	store := blobstore.NewMemory()
	root := src.image(t, "layer-one", "layer-two")

	c := &copier.Copier{Source: src, Store: store, Repository: repository, Verify: true}
	if _, err := c.Run(context.Background(), root, "1.4.2"); err != nil {
		t.Fatalf("Run with Verify: %v", err)
	}
}

func TestValidate(t *testing.T) {
	for _, repository := range []string{"", "/leading", "trailing/", "a//b", "has:colon"} {
		if err := copier.ValidateRepository(repository); err == nil {
			t.Errorf("ValidateRepository(%q) succeeded, want an error", repository)
		}
	}
	if err := copier.ValidateRepository("robot/perception"); err != nil {
		t.Errorf("ValidateRepository: %v", err)
	}

	for _, tag := range []string{"", "a/b", "sha256:" + strings.Repeat("a", 64)} {
		if err := copier.ValidateTag(tag); err == nil {
			t.Errorf("ValidateTag(%q) succeeded, want an error", tag)
		}
	}
	if err := copier.ValidateTag("1.4.2"); err != nil {
		t.Errorf("ValidateTag: %v", err)
	}
}

// Probing cosign's fallback tags costs three requests per subject and almost
// all of them miss, so by default only the manifest being published is asked
// about. On a large index the difference is enough to trip a registry's rate
// limit.
func TestCosignTagScope(t *testing.T) {
	build := func(t *testing.T) (*fakeSource, *source.Manifest, *source.Manifest) {
		t.Helper()

		src := newFakeSource()
		child := src.image(t, "child-layer")
		index := src.addManifest(t, layout.Manifest{
			MediaType: "application/vnd.oci.image.index.v1+json",
			Manifests: []layout.Descriptor{
				{MediaType: child.MediaType, Digest: child.Digest, Size: child.Size},
			},
		})

		// Attached to the child, which is the case only --cosign-tags=all finds.
		sigConfig := src.addBlob(t, "application/vnd.oci.empty.v1+json", "{}")
		signature := src.addManifest(t, layout.Manifest{
			MediaType: "application/vnd.oci.image.manifest.v1+json",
			Config:    &sigConfig,
		})
		src.byTag[source.CosignTags(child.Digest)[0]] = signature

		return src, index, signature
	}

	t.Run("root only, by default", func(t *testing.T) {
		src, index, signature := build(t)
		store := blobstore.NewMemory()

		c := &copier.Copier{Source: src, Store: store, Repository: repository}
		if _, err := c.Run(context.Background(), index, "1.4.2"); err != nil {
			t.Fatalf("Run: %v", err)
		}

		if _, ok := store.Body(layout.ManifestKey(repository, signature.Digest)); ok {
			t.Error("the default scope reached past the root")
		}
	})

	t.Run("all", func(t *testing.T) {
		src, index, signature := build(t)
		store := blobstore.NewMemory()

		c := &copier.Copier{
			Source: src, Store: store, Repository: repository,
			CosignTags: copier.CosignTagsAll,
		}
		if _, err := c.Run(context.Background(), index, "1.4.2"); err != nil {
			t.Fatalf("Run: %v", err)
		}

		if _, ok := store.Body(layout.ManifestKey(repository, signature.Digest)); !ok {
			t.Errorf("--cosign-tags=all missed a signature attached to a child; stored: %v", store.Keys())
		}
	})

	t.Run("none", func(t *testing.T) {
		src, index, _ := build(t)
		store := blobstore.NewMemory()

		c := &copier.Copier{
			Source: src, Store: store, Repository: repository,
			CosignTags: copier.CosignTagsNone,
		}
		result, err := c.Run(context.Background(), index, "1.4.2")
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if len(result.Plan.Tags) != 1 {
			t.Errorf("Plan.Tags has %d entries, want only the requested tag", len(result.Plan.Tags))
		}
	})

	t.Run("rejects an unknown scope", func(t *testing.T) {
		src, index, _ := build(t)
		c := &copier.Copier{
			Source: src, Store: blobstore.NewMemory(), Repository: repository,
			CosignTags: "sometimes",
		}
		if _, err := c.Run(context.Background(), index, "1.4.2"); err == nil {
			t.Error("an unknown scope was accepted")
		}
	})
}

// What the log says is what an operator reads while a fleet waits, so it is
// worth pinning: every blob accounts for itself, whether or not it moved.
func TestTheLogAccountsForEveryBlob(t *testing.T) {
	src := newFakeSource()
	store := blobstore.NewMemory()

	root := src.image(t, "layer-one", "layer-two")
	src.byTag["1.4.2"] = root

	var lines []string
	newCopier := func() *copier.Copier {
		return &copier.Copier{
			Source:     src,
			Store:      store,
			Repository: repository,
			Log:        func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) },
		}
	}

	if _, err := newCopier().Run(context.Background(), root, "1.4.2"); err != nil {
		t.Fatalf("Run: %v", err)
	}

	uploaded := count(lines, "uploaded in")
	if uploaded != 3 {
		t.Errorf("the first copy reported %d uploads, want 3 (a config and two layers):\n%s", uploaded, strings.Join(lines, "\n"))
	}
	if got := count(lines, " present"); got != 0 {
		t.Errorf("the first copy reported %d objects already present, want 0", got)
	}

	lines = nil
	if _, err := newCopier().Run(context.Background(), root, "1.4.2"); err != nil {
		t.Fatalf("Run again: %v", err)
	}

	// The point of saying so: a re-run that copies nothing should look like a
	// re-run that copied nothing, not like one that did not happen.
	if got := count(lines, "uploaded in"); got != 0 {
		t.Errorf("the second copy uploaded %d blobs, want 0:\n%s", got, strings.Join(lines, "\n"))
	}
	if got := count(lines, "blob sha256:"); got != 3 {
		t.Errorf("the second copy accounted for %d blobs, want 3:\n%s", got, strings.Join(lines, "\n"))
	}
	if got := count(lines, " present"); got < 3 {
		t.Errorf("the second copy reported %d objects already present, want at least the 3 blobs:\n%s", got, strings.Join(lines, "\n"))
	}
}

func TestADryRunSaysItWouldUpload(t *testing.T) {
	src := newFakeSource()
	store := blobstore.NewMemory()

	root := src.image(t, "layer-one")
	src.byTag["1.4.2"] = root

	var lines []string
	c := &copier.Copier{
		Source:     src,
		Store:      store,
		Repository: repository,
		DryRun:     true,
		Log:        func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) },
	}
	if _, err := c.Run(context.Background(), root, "1.4.2"); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got := count(lines, "would upload"); got != 2 {
		t.Errorf("a dry run reported %d blobs it would upload, want 2:\n%s", got, strings.Join(lines, "\n"))
	}
	if got := count(lines, "uploaded in"); got != 0 {
		t.Errorf("a dry run claimed to have uploaded %d blobs", got)
	}
}

func count(lines []string, substring string) int {
	n := 0
	for _, line := range lines {
		if strings.Contains(line, substring) {
			n++
		}
	}

	return n
}

func TestHumanBytes(t *testing.T) {
	for _, tc := range []struct {
		in   int64
		want string
	}{
		{in: 0, want: "0 B"},
		{in: 512, want: "512 B"},
		{in: 1024, want: "1.0 KiB"},
		{in: 1536, want: "1.5 KiB"},
		{in: 1024 * 1024, want: "1.0 MiB"},
		{in: 3 * 1024 * 1024 * 1024, want: "3.0 GiB"},
	} {
		if got := copier.HumanBytes(tc.in); got != tc.want {
			t.Errorf("HumanBytes(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
