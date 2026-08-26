// Package verify checks that a published tag is complete and self-consistent,
// reading only the bucket.
//
// This answers the question a publish leaves open: did everything land? A
// half-finished copy looks exactly like a healthy one from the outside until
// something pulls the layer that is missing, which on a robot fleet is a bad
// place to find out.
package verify

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"

	"github.com/lesomnus/derrick/internal/blobstore"
	"github.com/lesomnus/derrick/internal/layout"
)

// Verifier walks one repository in one bucket.
type Verifier struct {
	Store      blobstore.Store
	Repository string
}

// Report is what a walk found.
type Report struct {
	Digest    string
	Manifests int
	Blobs     int
	Referrers int

	// Problems is empty when the tag is servable.
	Problems []string
}

// OK reports whether the tag can be pulled.
func (r *Report) OK() bool {
	return len(r.Problems) == 0
}

// Tag walks everything reachable from a tag.
func (v *Verifier) Tag(ctx context.Context, tag string) (*Report, error) {
	report := &Report{}

	key := layout.ManifestKey(v.Repository, tag)
	obj, err := v.Store.Stat(ctx, key)
	if err != nil {
		return nil, err
	}
	if obj == nil {
		report.problem("tag %q is not published: %s is missing", tag, key)

		return report, nil
	}

	raw, err := v.read(ctx, key)
	if err != nil {
		return nil, err
	}

	digest := digestOf(raw)
	report.Digest = digest

	// The tagged copy is the one object whose key does not carry its digest,
	// so this is the only place the recorded digest can be checked against the
	// bytes it describes.
	recorded := obj.Metadata[layout.DigestMetadataKey]
	switch {
	case recorded == "":
		report.problem("%s has no %q metadata, so the registry will serve an empty Docker-Content-Digest", key, layout.DigestMetadataKey)
	case recorded != digest:
		report.problem("%s records digest %s but its bytes are %s", key, recorded, digest)
	}
	if obj.ContentType == "" {
		report.problem("%s has no content type, so a client cannot tell how to parse it", key)
	}

	seen := map[string]bool{}
	if err := v.walk(ctx, digest, raw, seen, report); err != nil {
		return nil, err
	}

	return report, nil
}

func (v *Verifier) walk(ctx context.Context, digest string, raw []byte, seen map[string]bool, report *Report) error {
	if seen[digest] {
		return nil
	}
	seen[digest] = true
	report.Manifests++

	m, err := layout.ParseManifest(raw)
	if err != nil {
		report.problem("manifest %s is unreadable: %v", digest, err)

		return nil
	}

	// Every manifest must also exist under its own digest, which is how a
	// client fetches it after resolving a tag.
	key := layout.ManifestKey(v.Repository, digest)
	stored, err := v.Store.Stat(ctx, key)
	if err != nil {
		return err
	}
	if stored == nil {
		report.problem("%s is missing", key)
	} else if stored.Size != int64(len(raw)) {
		report.problem("%s is %d bytes, expected %d", key, stored.Size, len(raw))
	}

	if m.Subject != nil {
		referrerKey := layout.ReferrerKey(v.Repository, m.Subject.Digest, digest)
		obj, err := v.Store.Stat(ctx, referrerKey)
		if err != nil {
			return err
		}
		if obj == nil {
			report.problem("%s is missing, so the referrers API will not report %s", referrerKey, digest)
		} else {
			report.Referrers++
		}
	}

	for _, blob := range m.Blobs() {
		blobKey := layout.BlobKey(v.Repository, blob.Digest)
		obj, err := v.Store.Stat(ctx, blobKey)
		if err != nil {
			return err
		}
		switch {
		case obj == nil:
			report.problem("%s is missing", blobKey)
		case obj.Size != blob.Size:
			report.problem("%s is %d bytes, expected %d", blobKey, obj.Size, blob.Size)
		default:
			report.Blobs++
		}
	}

	for _, child := range m.Manifests {
		childKey := layout.ManifestKey(v.Repository, child.Digest)
		obj, err := v.Store.Stat(ctx, childKey)
		if err != nil {
			return err
		}
		if obj == nil {
			report.problem("%s is missing", childKey)

			continue
		}

		childRaw, err := v.read(ctx, childKey)
		if err != nil {
			return err
		}
		if got := digestOf(childRaw); got != child.Digest {
			report.problem("%s holds %s, not the %s the index names", childKey, got, child.Digest)

			continue
		}
		if err := v.walk(ctx, child.Digest, childRaw, seen, report); err != nil {
			return err
		}
	}

	return nil
}

func (v *Verifier) read(ctx context.Context, key string) ([]byte, error) {
	rc, err := v.Store.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	defer rc.Close()

	raw, err := io.ReadAll(rc)
	if err != nil {
		return nil, fmt.Errorf("read %q: %w", key, err)
	}

	return raw, nil
}

func (r *Report) problem(format string, args ...any) {
	r.Problems = append(r.Problems, fmt.Sprintf(format, args...))
}

func digestOf(raw []byte) string {
	sum := sha256.Sum256(raw)

	return "sha256:" + hex.EncodeToString(sum[:])
}
