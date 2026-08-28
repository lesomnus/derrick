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

	// Prune removes destination tags the source no longer has.
	//
	// It removes the tag object and nothing else. What the tag was holding up
	// may still be reachable from another tag, and deciding that is a question
	// about the whole repository — which is `derrick prune`, deliberately a
	// separate command with its own grace period.
	Prune bool

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

	// Log is called from several goroutines at once, for the same reason
	// copier.Logger is, and with the tags of one repository in flight on top
	// of that. Everything this package writes goes through say or emit, which
	// serialise it.
	Log copier.Logger

	logMu sync.Mutex
}

// say writes one line.
func (m *Mirror) say(format string, args ...any) {
	m.logMu.Lock()
	defer m.logMu.Unlock()

	m.logger()(format, args...)
}

// emit writes a heading and the lines belonging under it, indented, with
// nothing else allowed in between.
//
// This is why a mirror can copy several tags at once and still read like it
// did them one at a time: each copy's own log is collected while it runs and
// printed when it finishes, so the interleaving happens between blocks rather
// than inside them.
func (m *Mirror) emit(header string, body []string) {
	m.logMu.Lock()
	defer m.logMu.Unlock()

	// Not through say: the lock is already held, and taking it again would
	// deadlock rather than interleave.
	log := m.logger()
	log("%s", header)
	for _, line := range body {
		log("  %s", line)
	}
}

// group collects what the copier says about one tag.
type group struct {
	mu    sync.Mutex
	lines []string
}

func (g *group) Log(format string, args ...any) {
	g.mu.Lock()
	defer g.mu.Unlock()

	g.lines = append(g.lines, fmt.Sprintf(format, args...))
}

func (g *group) body() []string {
	g.mu.Lock()
	defer g.mu.Unlock()

	return g.lines
}

// Result reports what a run did.
type Result struct {
	Repositories int
	Tags         int

	Copied  int
	Skipped int

	// Pruned counts destination tags removed because the source no longer has
	// them.
	Pruned int

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
	if _, ok := m.Store.(blobstore.Bucket); m.Prune && !ok {
		return nil, errors.New("--prune needs a store that can list and delete")
	}

	result := &Result{}

	catalog, err := m.Registry.Catalog(ctx)
	if err != nil {
		return nil, err
	}

	repositories, excluded := m.selectRepositories(catalog)
	result.Excluded += excluded
	result.Repositories = len(repositories)

	m.say("%s holds %d repositories, %d under %s", m.Registry.Name(), len(catalog), len(repositories), m.prefixDescription())

	for _, repository := range repositories {
		if err := ctx.Err(); err != nil {
			return result, err
		}

		if err := m.mirrorRepository(ctx, repository, result); err != nil {
			return result, err
		}
	}

	return result, nil
}

// mirrorRepository copies the tags of one repository. Only a failure that
// makes the rest of the run pointless is returned; a tag that could not be
// copied is recorded in the result.
func (m *Mirror) mirrorRepository(ctx context.Context, repository string, result *Result) error {
	if err := copier.ValidateRepository(repository); err != nil {
		m.say("warning: skipping repository %q: %v", repository, err)

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

	m.say("%s: %d tags listed", repository, len(listed))

	tags, excluded := m.selectTags(repository, listed)
	result.Tags += len(tags)
	result.Excluded += excluded

	if len(tags) == 0 {
		m.say("%s: nothing left to mirror", repository)

		return nil
	}

	outcomes := make([]outcome, len(tags))
	attempted := eachParallel(ctx, tags, m.parallel(), func(ctx context.Context, i int, tag string) {
		outcomes[i] = m.mirrorTag(ctx, repo, repository, tag)
	})

	if m.Prune {
		m.pruneVanishedTags(ctx, repository, listed, result)
	}

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

// pruneVanishedTags removes destination tags the source no longer lists.
//
// The comparison is against everything the source listed, not against the tags
// this run copied. An attachment tag was published by the copier rather than
// walked, and an excluded tag is one we chose not to publish rather than one
// that should be taken away; pruning against the filtered list would delete
// every signature in the repository on the first run.
func (m *Mirror) pruneVanishedTags(ctx context.Context, repository string, listed []string, result *Result) {
	if len(listed) == 0 {
		// A registry that answers with nothing looks exactly like a registry
		// whose repository is empty, and one of those is a reason to delete
		// every tag we have. Refuse to tell them apart.
		m.say("warning: %s listed no tags at all, so nothing is being pruned from it", repository)

		return
	}

	bucket, ok := m.Store.(blobstore.Bucket)
	if !ok {
		return
	}

	source := make(map[string]bool, len(listed))
	for _, tag := range listed {
		source[tag] = true
	}

	prefix := layout.ManifestKey(repository, "")

	var stale []string
	if err := bucket.List(ctx, prefix, func(entry blobstore.Entry) error {
		reference := strings.TrimPrefix(entry.Key, prefix)
		if strings.Contains(reference, "/") || layout.IsDigest(reference) || source[reference] {
			return nil
		}

		stale = append(stale, reference)

		return nil
	}); err != nil {
		result.Failures = append(result.Failures, Failure{Repository: repository, Err: err})

		return
	}

	for _, tag := range stale {
		m.say("%s:%s is no longer in the source; removing the tag", repository, tag)
		result.Pruned++

		if m.DryRun {
			continue
		}
		if err := bucket.Delete(ctx, layout.ManifestKey(repository, tag)); err != nil {
			result.Failures = append(result.Failures, Failure{Repository: repository, Tag: tag, Err: err})
		}
	}
}

type outcome struct {
	err      error
	copied   *copier.Result
	vanished bool
}

// mirrorTag copies one tag, unless it is already published.
func (m *Mirror) mirrorTag(ctx context.Context, repo Repository, repository, tag string) outcome {
	// The cheapest question that can be asked of the source: where does this
	// tag point now. Everything below is decided from the answer.
	digest, ok, err := repo.Digest(ctx, tag)
	if err != nil {
		return outcome{err: err}
	}
	if !ok {
		m.say("%s:%s vanished before it could be resolved", repository, tag)

		return outcome{vanished: true}
	}

	published, err := m.published(ctx, repository, tag, digest)
	if err != nil {
		return outcome{err: err}
	}
	if published {
		// Said out loud, and not only counted. A run that copies nothing
		// should account for every tag it decided not to copy, or the log of
		// a steady-state run says only that the mirror ran.
		m.say("%s:%s present at %s", repository, tag, digest)

		return outcome{}
	}

	root, ok, err := repo.ManifestByTag(ctx, tag)
	if err != nil {
		return outcome{err: err}
	}
	if !ok {
		return outcome{vanished: true}
	}

	body := &group{}

	c := &copier.Copier{
		Source:      repo,
		Store:       m.Store,
		Repository:  repository,
		Concurrency: m.Concurrency,
		DryRun:      m.DryRun,
		NoReferrers: m.NoReferrers,
		CosignTags:  m.CosignTags,
		Verify:      m.Verify,
		Log:         body.Log,
	}

	start := time.Now()
	copied, err := c.Run(ctx, root, tag)
	if err != nil {
		// What it got through before it failed is the most useful thing in
		// the log, so it is printed rather than dropped on the way out.
		m.emit(fmt.Sprintf("%s:%s failed after %s: %v",
			repository, tag, time.Since(start).Round(time.Millisecond), err), body.body())

		return outcome{err: err}
	}

	m.emit(fmt.Sprintf("%s:%s copied at %s in %s — %d blobs %s uploaded, %d already present, %d manifests, %d referrers",
		repository, tag, root.Digest, time.Since(start).Round(time.Millisecond),
		copied.BlobsUploaded, copier.HumanBytes(copied.BytesUploaded), copied.BlobsSkipped,
		copied.ManifestsWritten+copied.ManifestsSkipped, copied.ReferrersWritten), body.body())

	return outcome{copied: copied}
}

// published reports whether this tag is already in the bucket at this digest.
//
// This is the whole skip decision, and it is deliberately a question rather
// than a record. The bucket is what serves a pull, so the bucket is what gets
// asked: it costs one HEAD, it cannot go stale, and a tag that moved, an
// object someone deleted by hand and a run that died halfway all come out of
// it correctly with nothing to reconcile.
//
// It reads the tag object alone, which is enough to tell that this tag was
// published at this digest and not enough to tell that everything below it
// survived. `derrick verify` is the walk that answers that, and it is a
// different and much larger question than the one a mirror asks per tag.
func (m *Mirror) published(ctx context.Context, repository, tag, digest string) (bool, error) {
	obj, err := m.Store.Stat(ctx, layout.ManifestKey(repository, tag))
	if err != nil {
		return false, err
	}
	if obj == nil {
		return false, nil
	}

	return obj.Metadata[layout.DigestMetadataKey] == digest, nil
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
			m.say("%s is excluded by pattern", repository)
			excluded++

			continue
		}

		kept = append(kept, repository)
	}
	sort.Strings(kept)

	return kept, excluded
}

// selectTags keeps the tags worth copying as images of their own, saying out
// loud what it passes over. A tag nobody can find in the log is one somebody
// will eventually go looking for in the bucket.
func (m *Mirror) selectTags(repository string, listed []string) ([]string, int) {
	var (
		kept     []string
		excluded int
	)
	for _, tag := range listed {
		// An artifact attached under a digest-named tag comes across with its
		// subject, so copying it again as a top-level image would duplicate
		// the work and publish a tag that means nothing here.
		if source.IsAttachmentTag(tag) {
			// Not lost: it comes across attached to the manifest it names.
			m.say("%s:%s is an attachment; it travels with its subject", repository, tag)
			excluded++

			continue
		}
		if err := copier.ValidateTag(tag); err != nil {
			m.say("warning: %s:%s is not a tag this bucket can hold: %v", repository, tag, err)
			excluded++

			continue
		}
		if matchesAny(m.ExcludeTags, tag) {
			m.say("%s:%s is excluded by pattern", repository, tag)
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
