package layout_test

import (
	"encoding/json"
	"testing"

	"github.com/lesomnus/derrick/internal/layout"
)

const (
	digestA = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	digestB = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func TestKeys(t *testing.T) {
	if got, want := layout.BlobKey("robot/perception", digestA), "robot/perception/blobs/"+digestA; got != want {
		t.Errorf("BlobKey = %q, want %q", got, want)
	}
	if got, want := layout.ManifestKey("robot/perception", "1.4.2"), "robot/perception/manifests/1.4.2"; got != want {
		t.Errorf("ManifestKey = %q, want %q", got, want)
	}
	if got, want := layout.ReferrerKey("robot/perception", digestA, digestB), "robot/perception/_referrers/"+digestA+"/"+digestB; got != want {
		t.Errorf("ReferrerKey = %q, want %q", got, want)
	}
}

func TestIsDigest(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want bool
	}{
		{digestA, true},
		{"1.4.2", false},
		{"sha256:short", false},
		{"sha256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", false},
	} {
		if got := layout.IsDigest(tc.in); got != tc.want {
			t.Errorf("IsDigest(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestParseManifestRejectsSchemaVersion1(t *testing.T) {
	// Schema 1 manifests are signed differently and are not something the
	// registry serves, so failing here beats writing them into a bucket.
	if _, err := layout.ParseManifest([]byte(`{"schemaVersion":1}`)); err == nil {
		t.Fatal("ParseManifest accepted schemaVersion 1")
	}
}

func TestBlobsExcludesForeignLayers(t *testing.T) {
	raw := []byte(`{
	  "schemaVersion": 2,
	  "mediaType": "application/vnd.oci.image.manifest.v1+json",
	  "config": {"mediaType": "application/vnd.oci.image.config.v1+json", "digest": "` + digestA + `", "size": 7},
	  "layers": [
	    {"mediaType": "application/vnd.oci.image.layer.v1.tar+gzip", "digest": "` + digestB + `", "size": 11},
	    {"mediaType": "application/vnd.docker.image.rootfs.foreign.diff.tar.gzip", "digest": "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc", "size": 13, "urls": ["https://example.test/layer"]}
	  ]
	}`)

	m, err := layout.ParseManifest(raw)
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}

	blobs := m.Blobs()
	if len(blobs) != 2 {
		t.Fatalf("Blobs returned %d entries, want 2 (config and the one distributable layer)", len(blobs))
	}
	if blobs[0].Digest != digestA || blobs[1].Digest != digestB {
		t.Errorf("Blobs = %v, want config then layer", blobs)
	}

	foreign := m.ForeignBlobs()
	if len(foreign) != 1 {
		t.Fatalf("ForeignBlobs returned %d entries, want 1", len(foreign))
	}
}

func TestEffectiveArtifactType(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "explicit artifactType wins",
			raw:  `{"schemaVersion":2,"artifactType":"application/vnd.dev.cosign.artifact.sig.v1+json","config":{"mediaType":"application/vnd.oci.empty.v1+json","digest":"` + digestA + `","size":2}}`,
			want: "application/vnd.dev.cosign.artifact.sig.v1+json",
		},
		{
			name: "image manifest falls back to its config media type",
			raw:  `{"schemaVersion":2,"config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":"` + digestA + `","size":2}}`,
			want: "application/vnd.oci.image.config.v1+json",
		},
		{
			name: "an index has none",
			raw:  `{"schemaVersion":2,"manifests":[{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"` + digestA + `","size":2}]}`,
			want: "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, err := layout.ParseManifest([]byte(tc.raw))
			if err != nil {
				t.Fatalf("ParseManifest: %v", err)
			}
			if got := m.EffectiveArtifactType(); got != tc.want {
				t.Errorf("EffectiveArtifactType = %q, want %q", got, tc.want)
			}
		})
	}
}

// The registry validates a stored referrer descriptor when it reads it, and
// drops one it cannot parse. A descriptor that encodes to anything other than
// this loses the signature silently.
func TestReferrerDescriptorEncoding(t *testing.T) {
	raw := []byte(`{
	  "schemaVersion": 2,
	  "mediaType": "application/vnd.oci.image.manifest.v1+json",
	  "artifactType": "application/vnd.dev.cosign.artifact.sig.v1+json",
	  "config": {"mediaType": "application/vnd.oci.empty.v1+json", "digest": "` + digestA + `", "size": 2},
	  "subject": {"mediaType": "application/vnd.oci.image.index.v1+json", "digest": "` + digestB + `", "size": 42},
	  "annotations": {"org.opencontainers.image.created": "2026-08-26T00:00:00Z"}
	}`)

	m, err := layout.ParseManifest(raw)
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}

	descriptor := m.ReferrerDescriptor(digestA, 123)
	if descriptor == nil {
		t.Fatal("ReferrerDescriptor returned nil for a manifest with a subject")
	}

	encoded, err := json.Marshal(descriptor)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if decoded["mediaType"] != "application/vnd.oci.image.manifest.v1+json" {
		t.Errorf("mediaType = %v", decoded["mediaType"])
	}
	if decoded["digest"] != digestA {
		t.Errorf("digest = %v, want the referring manifest's own digest", decoded["digest"])
	}
	if decoded["size"] != float64(123) {
		t.Errorf("size = %v, want 123", decoded["size"])
	}
	if decoded["artifactType"] != "application/vnd.dev.cosign.artifact.sig.v1+json" {
		t.Errorf("artifactType = %v", decoded["artifactType"])
	}
	if _, ok := decoded["annotations"]; !ok {
		t.Error("annotations were dropped")
	}
	if _, ok := decoded["urls"]; ok {
		t.Error("urls should be omitted when empty; the registry rejects unexpected shapes")
	}
}

func TestReferrerDescriptorNilWithoutSubject(t *testing.T) {
	m, err := layout.ParseManifest([]byte(`{"schemaVersion":2,"config":{"mediaType":"x","digest":"` + digestA + `","size":1}}`))
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	if m.ReferrerDescriptor(digestA, 1) != nil {
		t.Error("a manifest with no subject refers to nothing and needs no descriptor")
	}
}

func TestContentAddressed(t *testing.T) {
	const digest = "sha256:9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"

	for _, tc := range []struct {
		key  string
		want bool
	}{
		{key: layout.BlobKey("dist/perception", digest), want: true},
		{key: layout.ManifestKey("dist/perception", digest), want: true},
		{key: layout.ReferrerKey("dist/perception", digest, digest), want: true},
		// A tag is the one object whose bytes are not decided by its key.
		{key: layout.ManifestKey("dist/perception", "1.4.2"), want: false},
		{key: layout.ManifestKey("dist/perception", "latest"), want: false},
	} {
		if got := layout.ContentAddressed(tc.key); got != tc.want {
			t.Errorf("ContentAddressed(%q) = %v, want %v", tc.key, got, tc.want)
		}
	}
}
