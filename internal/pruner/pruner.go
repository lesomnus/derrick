// Package pruner reclaims the objects in a repository that nothing points at.
//
// # Why this is not just a delete
//
// A registry bucket is a graph with one kind of root. Tags point at manifests,
// manifests point at other manifests and at blobs, and referrers hang off the
// manifests they describe. Removing an image means removing a tag; everything
// underneath is shared, and whether any of it is now garbage is a question
// about the whole repository rather than about the image that was untagged.
//
// # Why unreferenced is not the same as garbage
//
// A publish writes blobs, then manifests, then the tag. Halfway through, an
// image that is arriving looks exactly like an image that has been abandoned:
// blobs nothing points at. So an object is only deleted once it has been
// unreferenced for longer than a publish can plausibly take, which is what
// Grace is for. A day is generous and costs nothing, because storage is not
// what makes a registry expensive.
//
// # What it will not do
//
// It reasons about one repository. Upstream's cross-repository blob mounts —
// objects whose body is the key of a blob in another repository — would make
// that unsound, because a blob here could be reachable from a manifest there.
// derrick never writes one, and this refuses to delete any key it does not
// recognise, so a bucket that has them is not silently mispruned.
package pruner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/lesomnus/derrick/internal/blobstore"
	"github.com/lesomnus/derrick/internal/copier"
	"github.com/lesomnus/derrick/internal/layout"
)

// DefaultGrace is how long an object must have been unreferenced before it is
// treated as garbage rather than as a publish in flight.
const DefaultGrace = 24 * time.Hour

// Pruner removes what is unreachable in one repository of one bucket.
type Pruner struct {
	Store      blobstore.Bucket
	Repository string

	// Grace is how old an unreachable object must be before it is deleted.
	// Zero means none, which is only safe when nothing is publishing; the
	// command line defaults it to DefaultGrace, and a caller that does not
	// choose is choosing the dangerous one.
	Grace time.Duration

	// Apply actually deletes. Without it the run reports and writes nothing,
	// which is the default because this is the one command that can lose an
	// image.
	Apply bool

	Log copier.Logger

	// Now is the clock the grace period is measured against.
	Now func() time.Time
}

// Counts is a tally by kind of object.
type Counts struct {
	Manifests int
	Blobs     int
	Referrers int
}

// Total is how many objects the tally covers.
func (c Counts) Total() int {
	return c.Manifests + c.Blobs + c.Referrers
}

// Result reports what a run found and did.
type Result struct {
	// Tags is how many tags were found to walk from.
	Tags int

	Reachable Counts

	// Deleted is what was removed, or what would have been removed when Apply
	// is false.
	Deleted Counts

	// Withheld is unreachable but still inside the grace period, which is
	// what an in-flight publish looks like.
	Withheld Counts

	BytesFreed int64

	// Unknown is keys that do not belong to the layout. They are never
	// deleted: something else wrote them, and this does not know what for.
	Unknown []string
}

// Run walks the repository and reclaims what nothing points at.
func (p *Pruner) Run(ctx context.Context) (*Result, error) {
	if p.Store == nil {
		return nil, errors.New("a store is required")
	}
	if err := copier.ValidateRepository(p.Repository); err != nil {
		return nil, err
	}

	log := p.logger()

	inventory, err := p.inventory(ctx)
	if err != nil {
		return nil, err
	}

	result := &Result{Tags: len(inventory.tags), Unknown: inventory.unknown}
	for _, key := range inventory.unknown {
		log("warning: %s is not a key this layout describes; leaving it alone", key)
	}

	if len(inventory.tags) == 0 {
		// Not an error, and not a licence to delete the repository either. A
		// repository with no tags is one whose images are pullable by digest
		// only, which is a thing people do on purpose.
		log("%s has no tags; everything in it is unreachable and nothing will be deleted without one", p.Repository)

		return result, nil
	}

	reachable, err := p.reachable(ctx, inventory)
	if err != nil {
		return nil, err
	}
	result.Reachable = Counts{
		Manifests: len(reachable.manifests),
		Blobs:     len(reachable.blobs),
		Referrers: len(reachable.referrers),
	}

	log("%s: %d tags reach %d manifests, %d blobs and %d referrers",
		p.Repository, len(inventory.tags), len(reachable.manifests), len(reachable.blobs), len(reachable.referrers))

	cutoff := p.now().Add(-p.Grace)

	// Referrers first, then manifests, then blobs: at no point does something
	// still present point at something already gone.
	for _, ref := range inventory.referrers {
		if reachable.referrers[ref.key] {
			continue
		}
		p.consider(ctx, ref.entry, cutoff, &result.Deleted.Referrers, &result.Withheld.Referrers, result)
	}
	for _, m := range inventory.manifests {
		if reachable.manifests[m.digest] {
			continue
		}
		p.consider(ctx, m.entry, cutoff, &result.Deleted.Manifests, &result.Withheld.Manifests, result)
	}
	for _, b := range inventory.blobs {
		if reachable.blobs[b.digest] {
			continue
		}
		p.consider(ctx, b.entry, cutoff, &result.Deleted.Blobs, &result.Withheld.Blobs, result)
	}

	return result, nil
}

// consider deletes one unreachable object, or explains why it did not.
func (p *Pruner) consider(ctx context.Context, entry blobstore.Entry, cutoff time.Time, deleted, withheld *int, result *Result) {
	log := p.logger()

	if entry.Modified.After(cutoff) {
		// Young and unreferenced is what a publish in flight looks like from
		// here, and there is no way to tell the two apart from the bucket.
		log("holding %s: unreferenced, but written %s ago", entry.Key, p.now().Sub(entry.Modified).Round(time.Second))
		*withheld++

		return
	}

	if !p.Apply {
		log("would delete %s (%s)", entry.Key, copier.HumanBytes(entry.Size))
		*deleted++
		result.BytesFreed += entry.Size

		return
	}

	if err := p.Store.Delete(ctx, entry.Key); err != nil {
		log("warning: could not delete %s: %v", entry.Key, err)

		return
	}

	log("deleted %s (%s)", entry.Key, copier.HumanBytes(entry.Size))
	*deleted++
	result.BytesFreed += entry.Size
}

type object struct {
	entry  blobstore.Entry
	digest string
}

type referrer struct {
	entry   blobstore.Entry
	key     string
	subject string
	digest  string
}

type inventory struct {
	tags      []object // digest is empty; resolved during the walk
	manifests []object
	blobs     []object
	referrers []referrer

	// bySubject indexes referrers by the manifest they hang off.
	bySubject map[string][]referrer

	unknown []string
}

// inventory reads every key in the repository and sorts it by what it is.
func (p *Pruner) inventory(ctx context.Context) (*inventory, error) {
	inv := &inventory{bySubject: map[string][]referrer{}}

	// The trailing slash matters: without it, pruning `dist/oasys` would walk
	// `dist/oasys-internal` too.
	prefix := p.Repository + "/"

	err := p.Store.List(ctx, prefix, func(entry blobstore.Entry) error {
		rest := strings.TrimPrefix(entry.Key, prefix)

		switch {
		case strings.HasPrefix(rest, "blobs/"):
			digest := strings.TrimPrefix(rest, "blobs/")
			if !layout.IsDigest(digest) {
				inv.unknown = append(inv.unknown, entry.Key)

				return nil
			}
			inv.blobs = append(inv.blobs, object{entry: entry, digest: digest})

		case strings.HasPrefix(rest, "manifests/"):
			reference := strings.TrimPrefix(rest, "manifests/")
			switch {
			case strings.Contains(reference, "/"):
				inv.unknown = append(inv.unknown, entry.Key)
			case layout.IsDigest(reference):
				inv.manifests = append(inv.manifests, object{entry: entry, digest: reference})
			default:
				inv.tags = append(inv.tags, object{entry: entry})
			}

		case strings.HasPrefix(rest, "_referrers/"):
			subject, digest, ok := strings.Cut(strings.TrimPrefix(rest, "_referrers/"), "/")
			if !ok || !layout.IsDigest(subject) || !layout.IsDigest(digest) {
				inv.unknown = append(inv.unknown, entry.Key)

				return nil
			}
			r := referrer{entry: entry, key: entry.Key, subject: subject, digest: digest}
			inv.referrers = append(inv.referrers, r)
			inv.bySubject[subject] = append(inv.bySubject[subject], r)

		default:
			inv.unknown = append(inv.unknown, entry.Key)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	sort.Strings(inv.unknown)

	return inv, nil
}

type reachable struct {
	manifests map[string]bool
	blobs     map[string]bool
	referrers map[string]bool
}

// reachable walks out from every tag.
func (p *Pruner) reachable(ctx context.Context, inv *inventory) (*reachable, error) {
	r := &reachable{
		manifests: map[string]bool{},
		blobs:     map[string]bool{},
		referrers: map[string]bool{},
	}

	for _, tag := range inv.tags {
		digest, err := p.rootDigest(ctx, tag.entry.Key)
		if err != nil {
			return nil, err
		}
		if err := p.walk(ctx, digest, inv, r); err != nil {
			return nil, err
		}
	}

	return r, nil
}

// walk marks a manifest and everything below it.
func (p *Pruner) walk(ctx context.Context, digest string, inv *inventory, r *reachable) error {
	if r.manifests[digest] {
		return nil
	}
	r.manifests[digest] = true

	raw, err := p.read(ctx, layout.ManifestKey(p.Repository, digest))
	if err != nil {
		// A tag or an index pointing at a manifest that is not here means the
		// repository is already broken. Pruning it would finish the job: the
		// blobs that manifest referenced look like garbage from here, and
		// deleting them turns a recoverable half-publish into a lost image.
		return fmt.Errorf("%s is referenced but not in the bucket, so what it references cannot be known; run `derrick verify` and republish before pruning: %w", digest, err)
	}

	m, err := layout.ParseManifest(raw)
	if err != nil {
		return fmt.Errorf("manifest %s: %w", digest, err)
	}

	for _, child := range m.Manifests {
		if err := p.walk(ctx, child.Digest, inv, r); err != nil {
			return err
		}
	}
	for _, blob := range m.Blobs() {
		r.blobs[blob.Digest] = true
	}

	// A signature is reachable because its subject is. Losing one to a prune
	// would leave an image that verifies nowhere, which is worse than the
	// space it takes.
	for _, ref := range inv.bySubject[digest] {
		r.referrers[ref.key] = true
		if err := p.walk(ctx, ref.digest, inv, r); err != nil {
			return err
		}
	}

	return nil
}

// rootDigest is the manifest a tag object points at.
func (p *Pruner) rootDigest(ctx context.Context, key string) (string, error) {
	obj, err := p.Store.Stat(ctx, key)
	if err != nil {
		return "", err
	}
	if obj == nil {
		// Listed a moment ago and gone now; treat it as no longer a root.
		return "", fmt.Errorf("tag object %s disappeared while it was being read", key)
	}

	if digest := obj.Metadata[layout.DigestMetadataKey]; layout.IsDigest(digest) {
		return digest, nil
	}

	// The metadata is the contract and something else wrote this object. Its
	// body is still the manifest, though, and hashing it is the safe reading:
	// it can only find more things to keep.
	raw, err := p.read(ctx, key)
	if err != nil {
		return "", err
	}

	sum := sha256.Sum256(raw)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	p.logger()("warning: %s carries no digest metadata; treating it as %s", key, digest)

	return digest, nil
}

func (p *Pruner) read(ctx context.Context, key string) ([]byte, error) {
	rc, err := p.Store.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	defer rc.Close()

	return io.ReadAll(rc)
}

func (p *Pruner) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}

	return time.Now()
}

func (p *Pruner) logger() copier.Logger {
	if p.Log == nil {
		return func(string, ...any) {}
	}

	return p.Log
}
