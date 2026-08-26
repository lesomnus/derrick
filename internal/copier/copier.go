// Package copier copies an image out of a registry and into a bucket laid out
// for serverless-registry to serve.
package copier

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/lesomnus/derrick/internal/blobstore"
	"github.com/lesomnus/derrick/internal/layout"
	"github.com/lesomnus/derrick/internal/source"
)

// Logger receives progress. A nil Logger on a Copier discards it.
type Logger func(format string, args ...any)

// Source is the registry a copy reads from.
//
// It is an interface because the ordering rules this package enforces are the
// part that has to be right, and testing them against a real registry would
// mean not testing them.
type Source interface {
	// Manifest fetches a manifest by digest.
	Manifest(ctx context.Context, digest string) (*source.Manifest, error)

	// ManifestByTag fetches a manifest by tag, reporting false when the tag
	// does not exist.
	ManifestByTag(ctx context.Context, tag string) (*source.Manifest, bool, error)

	// Blob opens a blob, verified against its digest as it is read.
	Blob(ctx context.Context, digest string) (io.ReadCloser, error)

	// Referrers lists manifests that name digest as their subject.
	Referrers(ctx context.Context, digest string) ([]layout.Descriptor, error)
}

// Copier moves one image, with everything attached to it, into one repository
// of one bucket.
type Copier struct {
	Source Source
	Store  blobstore.Store

	// Repository is the target repository, which may contain slashes.
	Repository string

	// Concurrency is how many blobs upload at once. Zero means four.
	Concurrency int

	// DryRun plans and reports without writing anything.
	DryRun bool

	// NoReferrers skips signatures, attestations and SBOMs. Copying an image
	// without them leaves it unverifiable, so this is for debugging rather
	// than for releases.
	NoReferrers bool

	// Verify re-reads every object after writing and checks it is the size and
	// shape that was intended.
	Verify bool

	Log Logger
}

// Result reports what a run did.
type Result struct {
	Plan *Plan

	BlobsUploaded    int
	BlobsSkipped     int
	BytesUploaded    int64
	ManifestsWritten int
	ManifestsSkipped int
	ReferrersWritten int
	TagsWritten      int
}

// Run plans the copy and carries it out.
func (c *Copier) Run(ctx context.Context, root *source.Manifest, tag string) (*Result, error) {
	if err := ValidateRepository(c.Repository); err != nil {
		return nil, err
	}
	if err := ValidateTag(tag); err != nil {
		return nil, err
	}

	log := c.logger()

	plan, err := c.Plan(ctx, root, tag)
	if err != nil {
		return nil, err
	}

	result := &Result{Plan: plan}

	for _, foreign := range plan.Foreign {
		// Not an error: a foreign layer is fetched from its own URLs by the
		// client, by design. It does mean this image is not wholly served by
		// this registry, which is worth knowing before it ships to a fleet.
		log("warning: %s is a foreign layer and is not being copied; pulls will fetch it from %s",
			foreign.Digest, strings.Join(foreign.URLs, ", "))
	}

	log("copying %d blobs, %d manifests, %d referrers, %d tags",
		len(plan.Blobs), len(plan.Manifests), len(plan.Referrers), len(plan.Tags))

	// 1. Blobs. Nothing may reference an object that is not there yet.
	var mu sync.Mutex
	err = forEach(ctx, plan.Blobs, c.concurrency(), func(ctx context.Context, blob layout.Descriptor) error {
		uploaded, err := c.copyBlob(ctx, blob)
		if err != nil {
			return err
		}

		mu.Lock()
		defer mu.Unlock()
		if uploaded {
			result.BlobsUploaded++
			result.BytesUploaded += blob.Size
		} else {
			result.BlobsSkipped++
		}

		return nil
	})
	if err != nil {
		return result, err
	}

	// 2. Manifests, in the order Plan established.
	for _, m := range plan.Manifests {
		written, err := c.copyManifest(ctx, m)
		if err != nil {
			return result, err
		}
		if written {
			result.ManifestsWritten++
		} else {
			result.ManifestsSkipped++
		}
	}

	// 3. Referrer descriptors, once their subjects exist.
	for _, referrer := range plan.Referrers {
		if err := c.writeReferrer(ctx, referrer); err != nil {
			return result, err
		}
		result.ReferrersWritten++
	}

	// 4. Tags. The last one is the commit.
	for _, t := range plan.Tags {
		if err := c.writeTag(ctx, t); err != nil {
			return result, err
		}
		result.TagsWritten++
	}

	if c.Verify && !c.DryRun {
		if err := c.verify(ctx, plan); err != nil {
			return result, err
		}
		log("verified %d objects", len(plan.Blobs)+len(plan.Manifests)+len(plan.Referrers)+len(plan.Tags))
	}

	return result, nil
}

func (c *Copier) copyBlob(ctx context.Context, blob layout.Descriptor) (bool, error) {
	key := layout.BlobKey(c.Repository, blob.Digest)

	// Blobs are content-addressed, so an object already at this key is already
	// the right bytes. Across releases of the same image this skips nearly
	// everything, which is what makes republishing cheap.
	existing, err := c.Store.Stat(ctx, key)
	if err != nil {
		return false, err
	}
	if existing != nil && existing.Size == blob.Size {
		return false, nil
	}
	if existing != nil {
		c.logger()("re-uploading %s: stored size %d does not match %d", blob.Digest, existing.Size, blob.Size)
	}

	c.logger()("blob %s (%d bytes)", blob.Digest, blob.Size)
	if c.DryRun {
		return true, nil
	}

	// The stream is verified against its digest as it is read, so a truncated
	// or altered transfer fails here instead of landing in the bucket.
	rc, err := c.Source.Blob(ctx, blob.Digest)
	if err != nil {
		return false, err
	}
	defer rc.Close()

	if err := c.Store.Put(ctx, key, rc, blobstore.PutOptions{}); err != nil {
		return false, err
	}

	return true, nil
}

func (c *Copier) copyManifest(ctx context.Context, m *source.Manifest) (bool, error) {
	key := layout.ManifestKey(c.Repository, m.Digest)

	existing, err := c.Store.Stat(ctx, key)
	if err != nil {
		return false, err
	}
	if existing != nil && existing.Size == int64(len(m.Raw)) && existing.ContentType == m.MediaType {
		return false, nil
	}

	c.logger()("manifest %s (%s)", m.Digest, m.MediaType)
	if c.DryRun {
		return true, nil
	}

	// The bytes are copied verbatim. Re-encoding would change the digest and
	// invalidate every signature made over it.
	if err := c.Store.Put(ctx, key, bytesReader(m.Raw), blobstore.PutOptions{
		ContentType: m.MediaType,
	}); err != nil {
		return false, err
	}

	return true, nil
}

func (c *Copier) writeReferrer(ctx context.Context, referrer Referrer) error {
	body, err := json.Marshal(referrer.Descriptor)
	if err != nil {
		return fmt.Errorf("encode referrer descriptor %s: %w", referrer.Digest, err)
	}

	c.logger()("referrer %s -> %s", referrer.Digest, referrer.Subject)
	if c.DryRun {
		return nil
	}

	key := layout.ReferrerKey(c.Repository, referrer.Subject, referrer.Digest)

	return c.Store.Put(ctx, key, bytesReader(body), blobstore.PutOptions{
		ContentType: layout.ReferrerContentType,
	})
}

func (c *Copier) writeTag(ctx context.Context, t Tag) error {
	c.logger()("tag %s -> %s", t.Name, t.Manifest.Digest)
	if c.DryRun {
		return nil
	}

	key := layout.ManifestKey(c.Repository, t.Name)

	// A tagged manifest is the one object whose key does not carry its digest,
	// so the digest travels in metadata. Without it the registry serves an
	// unparseable Docker-Content-Digest and the pull fails with nothing to go
	// on. The tag is always rewritten, since moving a tag is the point.
	return c.Store.Put(ctx, key, bytesReader(t.Manifest.Raw), blobstore.PutOptions{
		ContentType: t.Manifest.MediaType,
		Metadata:    map[string]string{layout.DigestMetadataKey: t.Manifest.Digest},
	})
}

func (c *Copier) verify(ctx context.Context, plan *Plan) error {
	check := func(key string, size int64) error {
		obj, err := c.Store.Stat(ctx, key)
		if err != nil {
			return err
		}
		if obj == nil {
			return fmt.Errorf("verify: %s is missing", key)
		}
		if size >= 0 && obj.Size != size {
			return fmt.Errorf("verify: %s is %d bytes, expected %d", key, obj.Size, size)
		}

		return nil
	}

	for _, blob := range plan.Blobs {
		if err := check(layout.BlobKey(c.Repository, blob.Digest), blob.Size); err != nil {
			return err
		}
	}
	for _, m := range plan.Manifests {
		if err := check(layout.ManifestKey(c.Repository, m.Digest), int64(len(m.Raw))); err != nil {
			return err
		}
	}
	for _, referrer := range plan.Referrers {
		if err := check(layout.ReferrerKey(c.Repository, referrer.Subject, referrer.Digest), -1); err != nil {
			return err
		}
	}
	for _, t := range plan.Tags {
		key := layout.ManifestKey(c.Repository, t.Name)
		if err := check(key, int64(len(t.Manifest.Raw))); err != nil {
			return err
		}

		obj, err := c.Store.Stat(ctx, key)
		if err != nil {
			return err
		}
		if got := obj.Metadata[layout.DigestMetadataKey]; got != t.Manifest.Digest {
			return fmt.Errorf("verify: %s records digest %q, expected %q", key, got, t.Manifest.Digest)
		}
	}

	return nil
}

func (c *Copier) concurrency() int {
	if c.Concurrency > 0 {
		return c.Concurrency
	}

	return 4
}

func (c *Copier) logger() Logger {
	if c.Log == nil {
		return func(string, ...any) {}
	}

	return c.Log
}

// ValidateRepository rejects names that would produce keys the registry cannot
// map back to a repository.
func ValidateRepository(repository string) error {
	switch {
	case repository == "":
		return errors.New("repository is required")
	case strings.HasPrefix(repository, "/"), strings.HasSuffix(repository, "/"):
		return fmt.Errorf("repository %q may not begin or end with a slash", repository)
	case strings.Contains(repository, "//"):
		return fmt.Errorf("repository %q may not contain an empty path segment", repository)
	case strings.Contains(repository, ":"):
		return fmt.Errorf("repository %q may not contain a colon", repository)
	}

	return nil
}

// ValidateTag rejects a tag that would be mistaken for a digest, since the
// registry resolves a digest-shaped reference from the key alone.
func ValidateTag(tag string) error {
	if tag == "" {
		return errors.New("tag is required")
	}
	if strings.Contains(tag, "/") {
		return fmt.Errorf("tag %q may not contain a slash", tag)
	}
	if layout.IsDigest(tag) {
		return fmt.Errorf("tag %q is a digest", tag)
	}

	return nil
}
