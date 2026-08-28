// Package mirror copies every tag of every repository under a prefix into a
// bucket laid out for serverless-registry to serve.
//
// It is the copier with the references filled in by the source registry rather
// than by an operator, plus the bookkeeping that makes running it repeatedly
// cheap. Nothing about how an image is copied lives here: a mirror that
// diverged from a promote would be a second, less-exercised publisher.
package mirror

import (
	"context"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/lesomnus/derrick/internal/blobstore"
	"github.com/lesomnus/derrick/internal/copier"
	"github.com/lesomnus/derrick/internal/layout"
	"github.com/lesomnus/derrick/internal/ledger"
	"github.com/lesomnus/derrick/internal/source"
)

// Repository is one repository of the source registry.
type Repository interface {
	copier.Source

	// Tags lists the tags in the repository.
	Tags(ctx context.Context) ([]string, error)

	// Digest resolves a tag to a digest without fetching the manifest.
	Digest(ctx context.Context, tag string) (string, bool, error)
}

// Registry is the registry a mirror reads from.
//
// It is an interface for the same reason copier.Source is: what has to be
// right here is which tags get copied and which get skipped, and testing that
// against a real registry would mean not testing it.
type Registry interface {
	// Name is the registry host, for messages.
	Name() string

	// Catalog lists every repository the registry holds.
	Catalog(ctx context.Context) ([]string, error)

	// Repository opens one repository by name.
	Repository(ctx context.Context, repository string) (Repository, error)
}

// FromSource adapts a registry reached over the distribution API.
func FromSource(r *source.Registry) Registry {
	return sourceRegistry{r}
}

type sourceRegistry struct {
	*source.Registry
}

func (r sourceRegistry) Repository(ctx context.Context, repository string) (Repository, error) {
	repo, err := r.Registry.Repository(ctx, repository)
	if err != nil {
		return nil, err
	}

	return repo, nil
}

// Mirror copies a prefix of one registry into one bucket.
type Mirror struct {
	Registry Registry
	Store    blobstore.Store

	// Bucket is the destination bucket, for messages.
	Bucket string

	// Prefix selects the repositories to copy. Empty means all of them.
	//
	// The repository name is carried across unchanged, so what a client pulls
	// from the mirror differs from what it pulls from the source only in the
	// hostname. There is deliberately no way to rewrite it: a mirror whose
	// names do not match its source is a mirror nobody can reason about.
	Prefix string

	// Ledger records what has been copied. A nil Ledger examines every tag
	// against the destination on every run, which is correct and slow.
	Ledger *ledger.Ledger

	// Recheck verifies a ledger entry against the bucket instead of trusting
	// it. The ledger is a cache of what this tool did, and something else can
	// have happened to the bucket since — an object deleted by hand, a
	// half-finished run before the ledger was saved. This is how a mirror is
	// reconciled with what is actually servable, at the cost of one HEAD
	// against the bucket per already-known tag.
	Recheck bool

	// ExcludeRepositories and ExcludeTags are path.Match patterns. A pattern
	// is matched against the whole repository name or the whole tag, and `*`
	// does not cross a slash.
	ExcludeRepositories []string
	ExcludeTags         []string

	// Parallel is how many tags of one repository are examined at once. Zero
	// means four.
	Parallel int

	// Concurrency is how many blobs of one image upload at once, as in a copy.
	Concurrency int

	DryRun      bool
	NoReferrers bool
	CosignTags  string
	Verify      bool

	Log copier.Logger
}

// Result reports what a run did.
type Result struct {
	Repositories int
	Tags         int

	Copied  int
	Skipped int

	// Excluded counts what was passed over rather than examined: repositories
	// an exclusion pattern matched, and tags that were excluded, were named
	// after a digest, or could not be a tag in this bucket at all.
	Excluded int

	// Vanished counts tags that were listed but gone by the time they were
	// resolved, which is what a tag deleted mid-run looks like.
	Vanished int

	BlobsUploaded int
	BytesUploaded int64

	Failures []Failure
}

// Failure is one tag that could not be copied. A mirror does not stop at the
// first one: the run after a fixed problem should have less to do, not the
// same amount.
type Failure struct {
	Repository string
	Tag        string
	Err        error
}

func (f Failure) Error() string {
	return fmt.Sprintf("%s:%s: %v", f.Repository, f.Tag, f.Err)
}

// Run walks the registry and copies what is not already published.
func (m *Mirror) Run(ctx context.Context) (*Result, error) {
	if m.Registry == nil {
		return nil, errors.New("a source registry is required")
	}
	if m.Store == nil {
		return nil, errors.New("a destination store is required")
	}
	if err := validatePatterns(m.ExcludeRepositories); err != nil {
		return nil, fmt.Errorf("--exclude-repository: %w", err)
	}
	if err := validatePatterns(m.ExcludeTags); err != nil {
		return nil, fmt.Errorf("--exclude-tag: %w", err)
	}

	log := m.logger()
	result := &Result{}

	catalog, err := m.Registry.Catalog(ctx)
	if err != nil {
		return nil, err
	}

	repositories, excluded := m.selectRepositories(catalog)
	result.Excluded += excluded
	result.Repositories = len(repositories)

	log("%s holds %d repositories, %d under %s", m.Registry.Name(), len(catalog), len(repositories), m.prefixDescription())

	for _, repository := range repositories {
		if err := ctx.Err(); err != nil {
			return result, err
		}

		if err := m.mirrorRepository(ctx, repository, result); err != nil {
			return result, err
		}

		// Saved per repository rather than once at the end, so that a run that
		// is interrupted — and a fleet-wide mirror is a long thing to hold
		// open — keeps the progress it made.
		if err := m.saveLedger(ctx); err != nil {
			return result, err
		}
	}

	return result, nil
}

// mirrorRepository copies the tags of one repository. Only a failure that
// makes the rest of the run pointless is returned; a tag that could not be
// copied is recorded in the result.
func (m *Mirror) mirrorRepository(ctx context.Context, repository string, result *Result) error {
	log := m.logger()

	if err := copier.ValidateRepository(repository); err != nil {
		log("warning: skipping repository %q: %v", repository, err)

		return nil
	}

	repo, err := m.Registry.Repository(ctx, repository)
	if err != nil {
		result.Failures = append(result.Failures, Failure{Repository: repository, Err: err})

		return nil
	}

	listed, err := repo.Tags(ctx)
	if err != nil {
		result.Failures = append(result.Failures, Failure{Repository: repository, Err: err})

		return nil
	}

	tags, excluded := m.selectTags(listed)
	result.Tags += len(tags)
	result.Excluded += excluded

	if len(tags) == 0 {
		log("%s: nothing to mirror (%d tags listed)", repository, len(listed))

		return nil
	}

	log("%s: %d tags", repository, len(tags))

	outcomes := make([]outcome, len(tags))
	attempted := eachParallel(ctx, tags, m.parallel(), func(ctx context.Context, i int, tag string) {
		outcomes[i] = m.mirrorTag(ctx, repo, repository, tag)
	})

	for i, o := range outcomes[:attempted] {
		switch {
		case o.err != nil:
			result.Failures = append(result.Failures, Failure{Repository: repository, Tag: tags[i], Err: o.err})
		case o.vanished:
			result.Vanished++
		case o.copied != nil:
			result.Copied++
			result.BlobsUploaded += o.copied.BlobsUploaded
			result.BytesUploaded += o.copied.BytesUploaded
		default:
			result.Skipped++
		}
	}

	// Tags left unattempted mean the run was cancelled. Reporting them as
	// skipped would say the mirror is up to date, which is the one thing it is
	// not.
	if attempted < len(tags) {
		return ctx.Err()
	}

	return nil
}

type outcome struct {
	err      error
	copied   *copier.Result
	vanished bool
}

// mirrorTag copies one tag, unless it is already published.
func (m *Mirror) mirrorTag(ctx context.Context, repo Repository, repository, tag string) outcome {
	log := m.logger()

	// The cheapest question that can be asked of the source: where does this
	// tag point now. Everything below is decided from the answer.
	digest, ok, err := repo.Digest(ctx, tag)
	if err != nil {
		return outcome{err: err}
	}
	if !ok {
		log("%s:%s vanished before it could be resolved", repository, tag)

		return outcome{vanished: true}
	}

	published, err := m.published(ctx, repository, tag, digest)
	if err != nil {
		return outcome{err: err}
	}
	if published {
		return outcome{}
	}

	root, ok, err := repo.ManifestByTag(ctx, tag)
	if err != nil {
		return outcome{err: err}
	}
	if !ok {
		return outcome{vanished: true}
	}

	// Everything the copier logs is prefixed with the tag it is copying. A
	// mirror has several copies in flight, and an interleaved log of bare
	// digests says nothing about which image is slow or which one is stuck.
	logTag := func(format string, args ...any) {
		log("%s:%s "+format, append([]any{repository, tag}, args...)...)
	}

	c := &copier.Copier{
		Source:      repo,
		Store:       m.Store,
		Repository:  repository,
		Concurrency: m.Concurrency,
		DryRun:      m.DryRun,
		NoReferrers: m.NoReferrers,
		CosignTags:  m.CosignTags,
		Verify:      m.Verify,
		Log:         logTag,
	}

	start := time.Now()
	copied, err := c.Run(ctx, root, tag)
	if err != nil {
		return outcome{err: err}
	}

	logTag("copied at %s in %s (%d blobs %s uploaded, %d already present)",
		root.Digest, time.Since(start).Round(time.Millisecond),
		copied.BlobsUploaded, copier.HumanBytes(copied.BytesUploaded), copied.BlobsSkipped)

	// The digest recorded is the manifest that was actually copied, not the
	// one the HEAD above reported. They differ when the tag moved in between,
	// and recording what was copied is what keeps the ledger honest.
	if m.Ledger != nil && !m.DryRun {
		m.Ledger.Record(repository, tag, root.Digest, time.Now())
	}

	return outcome{copied: copied}
}

// published reports whether the tag at digest is already in the bucket.
func (m *Mirror) published(ctx context.Context, repository, tag, digest string) (bool, error) {
	if m.Ledger == nil {
		return m.publishedInBucket(ctx, repository, tag, digest)
	}

	entry, recorded := m.Ledger.Lookup(repository, tag)
	switch {
	case recorded && entry.Digest != digest:
		// The tag moved. Nothing has to be undone: the old manifest and its
		// blobs stay where they are, and rewriting the tag object is what
		// moves it here too.
		m.logger()("%s:%s moved from %s to %s", repository, tag, entry.Digest, digest)

	case recorded && !m.Recheck:
		return true, nil
	}

	// Nothing is recorded for this tag, or the record is stale, or --recheck
	// asked for it to be confirmed. All three are answered by the bucket.
	published, err := m.publishedInBucket(ctx, repository, tag, digest)
	if err != nil {
		return false, err
	}

	switch {
	case published && (!recorded || entry.Digest != digest):
		// Already there, and the ledger did not know. This is what a bucket
		// that was published into before it had a ledger looks like, and
		// recording it now is what makes the first run the only slow one.
		m.Ledger.Record(repository, tag, digest, time.Now())

	case !published && recorded:
		m.logger()("%s:%s is recorded as mirrored but the bucket disagrees; copying it again", repository, tag)
		m.Ledger.Forget(repository, tag)
	}

	return published, nil
}

// publishedInBucket asks the destination rather than the ledger.
//
// This reads the tag object alone, which is enough to tell that this tag was
// published at this digest, and not enough to tell that everything below it
// survived. `derrick verify` is the walk that answers that, and it is a
// different and much more expensive question than the one a mirror asks per
// tag.
func (m *Mirror) publishedInBucket(ctx context.Context, repository, tag, digest string) (bool, error) {
	obj, err := m.Store.Stat(ctx, layout.ManifestKey(repository, tag))
	if err != nil {
		return false, err
	}
	if obj == nil {
		return false, nil
	}

	return obj.Metadata[layout.DigestMetadataKey] == digest, nil
}

func (m *Mirror) saveLedger(ctx context.Context) error {
	if m.Ledger == nil || m.DryRun {
		return nil
	}

	saved, err := m.Ledger.Save(ctx, m.Store, ledger.DefaultKey)
	if err != nil {
		return err
	}
	if saved {
		m.logger()("ledger: %d tags recorded", m.Ledger.Len())
	}

	return nil
}

// selectRepositories keeps the repositories under the prefix that no exclusion
// pattern matches, sorted so that a run is reproducible and its log readable.
func (m *Mirror) selectRepositories(catalog []string) ([]string, int) {
	prefix := strings.Trim(m.Prefix, "/")

	var (
		kept     []string
		excluded int
	)
	for _, repository := range catalog {
		if prefix != "" && repository != prefix && !strings.HasPrefix(repository, prefix+"/") {
			continue
		}
		if matchesAny(m.ExcludeRepositories, repository) {
			excluded++

			continue
		}

		kept = append(kept, repository)
	}
	sort.Strings(kept)

	return kept, excluded
}

// selectTags keeps the tags worth copying as images of their own.
func (m *Mirror) selectTags(listed []string) ([]string, int) {
	var (
		kept     []string
		excluded int
	)
	for _, tag := range listed {
		// An artifact attached under a digest-named tag comes across with its
		// subject, so copying it again as a top-level image would duplicate
		// the work and publish a tag that means nothing here.
		if source.IsAttachmentTag(tag) {
			excluded++

			continue
		}
		if err := copier.ValidateTag(tag); err != nil {
			m.logger()("warning: skipping tag %q: %v", tag, err)
			excluded++

			continue
		}
		if matchesAny(m.ExcludeTags, tag) {
			excluded++

			continue
		}

		kept = append(kept, tag)
	}
	sort.Strings(kept)

	return kept, excluded
}

func (m *Mirror) prefixDescription() string {
	if prefix := strings.Trim(m.Prefix, "/"); prefix != "" {
		return prefix + "/"
	}

	return "no prefix"
}

func (m *Mirror) parallel() int {
	if m.Parallel > 0 {
		return m.Parallel
	}

	return 4
}

func (m *Mirror) logger() copier.Logger {
	if m.Log == nil {
		return func(string, ...any) {}
	}

	return m.Log
}

func validatePatterns(patterns []string) error {
	for _, pattern := range patterns {
		if _, err := path.Match(pattern, ""); err != nil {
			return fmt.Errorf("%q: %w", pattern, err)
		}
	}

	return nil
}

func matchesAny(patterns []string, s string) bool {
	for _, pattern := range patterns {
		// The pattern was validated before the run started, so a match error
		// here is not possible; ignoring it keeps the caller from having to
		// carry one through every filter.
		if ok, _ := path.Match(pattern, s); ok {
			return true
		}
	}

	return false
}

// eachParallel runs fn over items with at most workers running at once.
//
// Unlike the copier's, this one does not stop at the first failure. A copy
// that has gone wrong should stop pushing objects at a store that is refusing
// them; a mirror of a hundred repositories should not abandon the other
// ninety-nine because one image is broken.
// It reports how many items it handed to a worker, which is all of them
// unless ctx was cancelled.
func eachParallel[T any](ctx context.Context, items []T, workers int, fn func(context.Context, int, T)) int {
	if len(items) == 0 {
		return 0
	}
	if workers < 1 {
		workers = 1
	}
	if workers > len(items) {
		workers = len(items)
	}

	var wg sync.WaitGroup
	queue := make(chan int)

	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range queue {
				fn(ctx, i, items[i])
			}
		}()
	}

	fed := 0
feed:
	for i := range items {
		select {
		case queue <- i:
			fed++
		case <-ctx.Done():
			break feed
		}
	}
	close(queue)
	wg.Wait()

	return fed
}
