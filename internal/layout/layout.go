// Package layout describes how a serverless-registry bucket is laid out.
//
// A serverless-registry deployment serves images straight out of object
// storage, so the bucket is an interface rather than an implementation detail:
// get a key or a piece of metadata wrong and the registry either cannot find
// an image or cannot describe it to a client. Everything here mirrors what the
// registry actually reads.
//
// The layout is:
//
//	<repository>/blobs/sha256:<hex>                     a layer or a config, stored raw
//	<repository>/manifests/sha256:<hex>                 a manifest or an index, as JSON
//	<repository>/manifests/<tag>                        a copy of the manifest a tag points at
//	<repository>/_referrers/<subject>/<digest>          a referrer descriptor, as JSON
package layout

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// DigestMetadataKey is the object metadata key under which the digest of a
// tagged manifest is recorded.
//
// The registry resolves a digest from the object key first, which works for
// everything content-addressed. A manifest stored under a tag is the one
// object whose key does not carry its digest, so it carries it here instead.
// The key is lowercase because S3 normalises metadata keys to lowercase, and a
// mixed-case key would not survive the round trip.
const DigestMetadataKey = "digest"

// ReferrerContentType is the content type of a stored referrer descriptor.
const ReferrerContentType = "application/json"

var digestPattern = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)

// IsDigest reports whether reference is a digest rather than a tag.
func IsDigest(reference string) bool {
	return digestPattern.MatchString(reference)
}

// ContentAddressed reports whether the object at key is determined by the key:
// a blob, a manifest stored under its digest, or a referrer descriptor. A
// manifest stored under a tag is not, which is the whole reason a tag can
// move.
func ContentAddressed(key string) bool {
	index := strings.LastIndex(key, "/")

	return IsDigest(key[index+1:])
}

// BlobKey returns the key a blob is stored under.
func BlobKey(repository, digest string) string {
	return fmt.Sprintf("%s/blobs/%s", repository, digest)
}

// ManifestKey returns the key a manifest is stored under, for a reference that
// is either a digest or a tag.
func ManifestKey(repository, reference string) string {
	return fmt.Sprintf("%s/manifests/%s", repository, reference)
}

// ReferrerKey returns the key the descriptor of a referring manifest is stored
// under.
func ReferrerKey(repository, subject, digest string) string {
	return fmt.Sprintf("%s/_referrers/%s/%s", repository, subject, digest)
}

// Descriptor is an OCI content descriptor. It is also, field for field, what
// the registry expects to find in a stored referrer descriptor.
type Descriptor struct {
	MediaType    string            `json:"mediaType"`
	Digest       string            `json:"digest"`
	Size         int64             `json:"size"`
	ArtifactType string            `json:"artifactType,omitempty"`
	URLs         []string          `json:"urls,omitempty"`
	Annotations  map[string]string `json:"annotations,omitempty"`
}

// Foreign reports whether the described blob lives somewhere other than this
// registry. Foreign (or "non-distributable") layers name the places a client
// should fetch them from, and a registry is not supposed to hold them, so
// copying one is neither possible nor wanted.
func (d Descriptor) Foreign() bool {
	if len(d.URLs) > 0 {
		return true
	}

	return strings.Contains(d.MediaType, "foreign") || strings.Contains(d.MediaType, "nondistributable")
}

// Manifest is the subset of an image manifest and an image index that this
// tool needs. One type covers both because the two differ only in which
// fields are populated, and telling them apart is what Index reports.
type Manifest struct {
	SchemaVersion int               `json:"schemaVersion"`
	MediaType     string            `json:"mediaType"`
	ArtifactType  string            `json:"artifactType,omitempty"`
	Config        *Descriptor       `json:"config,omitempty"`
	Layers        []Descriptor      `json:"layers,omitempty"`
	Manifests     []Descriptor      `json:"manifests,omitempty"`
	Subject       *Descriptor       `json:"subject,omitempty"`
	Annotations   map[string]string `json:"annotations,omitempty"`
}

// ParseManifest reads the fields this tool needs out of a raw manifest.
func ParseManifest(raw []byte) (*Manifest, error) {
	m := &Manifest{}
	if err := json.Unmarshal(raw, m); err != nil {
		return nil, fmt.Errorf("parse manifest: %w", err)
	}
	if m.SchemaVersion != 2 {
		return nil, fmt.Errorf("unsupported manifest schemaVersion %d: only 2 is a thing this registry serves", m.SchemaVersion)
	}

	return m, nil
}

// Index reports whether the manifest points at other manifests rather than at
// a config and layers.
func (m *Manifest) Index() bool {
	return m.Manifests != nil
}

// Blobs returns the blobs this manifest references, which is nothing for an
// index. Foreign layers are excluded: they are not in the source registry and
// do not belong in the target bucket.
func (m *Manifest) Blobs() []Descriptor {
	if m.Index() {
		return nil
	}

	blobs := make([]Descriptor, 0, len(m.Layers)+1)
	if m.Config != nil {
		blobs = append(blobs, *m.Config)
	}
	for _, layer := range m.Layers {
		if layer.Foreign() {
			continue
		}
		blobs = append(blobs, layer)
	}

	return blobs
}

// ForeignBlobs returns the referenced blobs that were left behind by Blobs.
// They are worth reporting: an image that carries them pulls partly from
// somewhere this registry does not control.
func (m *Manifest) ForeignBlobs() []Descriptor {
	var foreign []Descriptor
	for _, layer := range m.Layers {
		if layer.Foreign() {
			foreign = append(foreign, layer)
		}
	}

	return foreign
}

// EffectiveArtifactType reports the artifact type the registry would record
// for this manifest in a referrer descriptor.
//
// This follows the registry's own rule rather than the specification's: an
// explicit artifactType wins, an index has none, and an image manifest falls
// back to the media type of its config. Deviating would produce descriptors
// that the registry filters differently than it filters its own.
func (m *Manifest) EffectiveArtifactType() string {
	if m.ArtifactType != "" {
		return m.ArtifactType
	}
	if m.Index() {
		return ""
	}
	if m.Config != nil {
		return m.Config.MediaType
	}

	return ""
}

// ReferrerDescriptor returns the descriptor to store for this manifest under
// its subject, or nil when the manifest refers to nothing.
func (m *Manifest) ReferrerDescriptor(digest string, size int64) *Descriptor {
	if m.Subject == nil {
		return nil
	}

	d := &Descriptor{
		MediaType:    m.MediaType,
		Digest:       digest,
		Size:         size,
		ArtifactType: m.EffectiveArtifactType(),
		Annotations:  m.Annotations,
	}

	return d
}
