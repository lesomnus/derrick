package verify_test

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/lesomnus/derrick/internal/blobstore"
	"github.com/lesomnus/derrick/internal/layout"
	"github.com/lesomnus/derrick/internal/verify"
)

// publish writes one image and one tag pointing at it, the way a copy would.
func publish(t *testing.T, store *blobstore.Memory, repository, tag string, layers ...string) string {
	t.Helper()

	ctx := context.Background()
	put := func(key string, body []byte, opts blobstore.PutOptions) {
		t.Helper()
		if err := store.Put(ctx, key, bytes.NewReader(body), opts); err != nil {
			t.Fatalf("put %q: %v", key, err)
		}
	}
	blob := func(content string) layout.Descriptor {
		digest := digestOf([]byte(content))
		put(layout.BlobKey(repository, digest), []byte(content), blobstore.PutOptions{})

		return layout.Descriptor{
			MediaType: "application/vnd.oci.image.layer.v1.tar+gzip",
			Digest:    digest,
			Size:      int64(len(content)),
		}
	}

	config := blob("config-" + strings.Join(layers, "-"))
	config.MediaType = "application/vnd.oci.image.config.v1+json"

	descriptors := make([]layout.Descriptor, 0, len(layers))
	for _, layer := range layers {
		descriptors = append(descriptors, blob(layer))
	}

	const mediaType = "application/vnd.oci.image.manifest.v1+json"
	raw, err := json.Marshal(layout.Manifest{
		SchemaVersion: 2,
		MediaType:     mediaType,
		Config:        &config,
		Layers:        descriptors,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	digest := digestOf(raw)
	put(layout.ManifestKey(repository, digest), raw, blobstore.PutOptions{ContentType: mediaType})
	put(layout.ManifestKey(repository, tag), raw, blobstore.PutOptions{
		ContentType: mediaType,
		Metadata:    map[string]string{layout.DigestMetadataKey: digest},
	})

	return digest
}

func survey(t *testing.T, store *blobstore.Memory, prefix string) *verify.SurveyReport {
	t.Helper()

	s := &verify.Survey{
		Store:    store,
		Prefix:   prefix,
		Parallel: 3,
		Log:      func(format string, args ...any) { t.Logf(format, args...) },
	}

	report, err := s.Run(context.Background())
	if err != nil {
		t.Fatalf("survey: %v", err)
	}

	return report
}

func TestSurveyWalksEveryTagUnderThePrefix(t *testing.T) {
	store := blobstore.NewMemory()
	publish(t, store, "dist/perception", "1.4.1", "layer-one")
	publish(t, store, "dist/perception", "1.4.2", "layer-two")
	publish(t, store, "dist/nested/control", "2.0.0", "layer-three")
	publish(t, store, "scratch/wip", "latest", "layer-four")

	report := survey(t, store, "dist")

	if report.Repositories != 2 {
		t.Errorf("walked %d repositories, want 2", report.Repositories)
	}
	if report.Tags != 3 {
		t.Errorf("walked %d tags, want 3", report.Tags)
	}
	if report.Complete != 3 || !report.OK() {
		t.Errorf("%d of %d tags complete, problems %v", report.Complete, report.Tags, report.Incomplete)
	}
}

// `dist/oasys` and `dist/oasys-internal` are two repositories, and one is a
// prefix of the other.
func TestSurveyMatchesOnRepositoryBoundaries(t *testing.T) {
	store := blobstore.NewMemory()
	publish(t, store, "dist/oasys", "v0.1.8", "layer-one")
	publish(t, store, "dist/oasys-internal", "internal", "layer-two")

	report := survey(t, store, "dist/oasys")

	if report.Tags != 1 {
		t.Errorf("walked %d tags, want only the one in dist/oasys", report.Tags)
	}
}

func TestSurveyFindsAMissingBlob(t *testing.T) {
	store := blobstore.NewMemory()
	publish(t, store, "dist/perception", "1.4.2", "layer-one")
	publish(t, store, "dist/control", "2.0.0", "layer-two")

	missing := layout.BlobKey("dist/perception", digestOf([]byte("layer-one")))
	if err := store.Delete(context.Background(), missing); err != nil {
		t.Fatalf("delete: %v", err)
	}

	report := survey(t, store, "dist")

	if report.OK() {
		t.Fatal("a repository with a missing layer surveyed clean")
	}
	if len(report.Incomplete) != 1 {
		t.Fatalf("reported %d incomplete tags, want 1", len(report.Incomplete))
	}
	if got := report.Incomplete[0].Tag.String(); got != "dist/perception:1.4.2" {
		t.Errorf("the incomplete tag is %q", got)
	}
	if report.Complete != 1 {
		t.Errorf("%d tags complete, want the untouched one", report.Complete)
	}

	found := false
	for _, problem := range report.Incomplete[0].Problems {
		if strings.Contains(problem, missing) {
			found = true
		}
	}
	if !found {
		t.Errorf("the problems do not name the missing blob: %v", report.Incomplete[0].Problems)
	}
}

// Tags of one repository share nearly all of their layers, so a walk that
// asked per reference would ask about the same object over and over.
func TestSurveyAsksAboutAnObjectOnce(t *testing.T) {
	store := blobstore.NewMemory()
	publish(t, store, "dist/perception", "1.4.2", "shared-layer", "another-layer")
	publish(t, store, "dist/perception", "stable", "shared-layer", "another-layer")

	before := store.StatCount()
	report := survey(t, store, "dist")

	if !report.OK() {
		t.Fatalf("problems: %v", report.Incomplete)
	}

	// A config, two layers and the manifest they belong to. Both tags are the
	// same image, so that is everything under both of them.
	if report.Objects != 4 {
		t.Errorf("asked about %d distinct objects, want 4", report.Objects)
	}

	// Those four, plus the two tag objects, which are not content-addressed
	// and so are asked about every time. Ten without the cache.
	if got := store.StatCount() - before; got != 6 {
		t.Errorf("made %d Stat calls, want 6", got)
	}
}
