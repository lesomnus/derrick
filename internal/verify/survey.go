package verify

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/lesomnus/derrick/internal/blobstore"
	"github.com/lesomnus/derrick/internal/copier"
	"github.com/lesomnus/derrick/internal/layout"
)

// Survey verifies every tag under a prefix.
//
// One tag answers "did this publish land". A prefix answers the question that
// is actually worth asking of a registry a fleet pulls from: is everything
// that is tagged still pullable — which is not something a publisher can know,
// because the ways it stops being true happen afterwards.
type Survey struct {
	Store blobstore.Bucket

	// Prefix selects the repositories to walk. Empty means all of them. It is
	// matched on repository boundaries, so `dist/oasys` does not draw in
	// `dist/oasys-internal`.
	Prefix string

	// Parallel is how many tags are walked at once. Zero means four.
	Parallel int

	Log copier.Logger

	logMu sync.Mutex
}

// Tag names one published tag.
type Tag struct {
	Repository string
	Name       string
}

func (t Tag) String() string {
	return t.Repository + ":" + t.Name
}

// Incomplete is a tag that something is missing from.
type Incomplete struct {
	Tag      Tag
	Problems []string
}

// SurveyReport is what a survey found.
type SurveyReport struct {
	Repositories int
	Tags         int
	Complete     int

	// Objects is how many distinct content-addressed objects were asked
	// about, which is the real size of what was checked; the per-tag counts
	// overlap heavily.
	Objects int

	Incomplete []Incomplete
}

// OK reports whether everything under the prefix is pullable.
func (r *SurveyReport) OK() bool {
	return len(r.Incomplete) == 0
}

// Run walks the bucket.
func (s *Survey) Run(ctx context.Context) (*SurveyReport, error) {
	if s.Store == nil {
		return nil, fmt.Errorf("a store is required")
	}

	tags, repositories, err := s.tags(ctx)
	if err != nil {
		return nil, err
	}

	report := &SurveyReport{Repositories: repositories, Tags: len(tags)}
	s.say("%d tags in %d repositories", len(tags), repositories)

	// Tags of one repository share nearly all of their blobs, so the same
	// object is otherwise asked about once per tag that references it.
	store := blobstore.NewCache(s.Store, layout.ContentAddressed)

	results := make([]*Report, len(tags))
	errs := make([]error, len(tags))

	eachParallel(ctx, tags, s.parallel(), func(ctx context.Context, i int, tag Tag) {
		v := &Verifier{Store: store, Repository: tag.Repository}
		results[i], errs[i] = v.Tag(ctx, tag.Name)
	})

	for i, tag := range tags {
		switch {
		case errs[i] != nil:
			report.Incomplete = append(report.Incomplete, Incomplete{
				Tag:      tag,
				Problems: []string{errs[i].Error()},
			})
			s.say("%s could not be read: %v", tag, errs[i])

		case results[i] == nil:
			// Not walked, which means the run was cancelled.
			report.Incomplete = append(report.Incomplete, Incomplete{
				Tag:      tag,
				Problems: []string{"not checked"},
			})

		case results[i].OK():
			report.Complete++
			s.say("%s complete at %s — %d manifests, %d blobs, %d referrers",
				tag, results[i].Digest, results[i].Manifests, results[i].Blobs, results[i].Referrers)

		default:
			report.Incomplete = append(report.Incomplete, Incomplete{Tag: tag, Problems: results[i].Problems})
			s.emit(fmt.Sprintf("%s is not servable", tag), results[i].Problems)
		}
	}

	report.Objects = store.Distinct()

	return report, nil
}

// tags reads the published tags out of the bucket, and how many repositories
// they are spread over.
func (s *Survey) tags(ctx context.Context) ([]Tag, int, error) {
	const marker = "/manifests/"

	prefix := strings.Trim(s.Prefix, "/")

	var tags []Tag
	repositories := map[string]bool{}

	err := s.Store.List(ctx, prefix, func(entry blobstore.Entry) error {
		index := strings.LastIndex(entry.Key, marker)
		if index < 0 {
			return nil
		}

		repository := entry.Key[:index]
		reference := entry.Key[index+len(marker):]

		// A prefix is matched on repository boundaries, or verifying
		// `dist/oasys` would walk `dist/oasys-internal` as well.
		if prefix != "" && repository != prefix && !strings.HasPrefix(repository, prefix+"/") {
			return nil
		}
		if strings.Contains(reference, "/") || layout.IsDigest(reference) {
			return nil
		}

		repositories[repository] = true
		tags = append(tags, Tag{Repository: repository, Name: reference})

		return nil
	})
	if err != nil {
		return nil, 0, err
	}

	sort.Slice(tags, func(i, j int) bool {
		if tags[i].Repository != tags[j].Repository {
			return tags[i].Repository < tags[j].Repository
		}

		return tags[i].Name < tags[j].Name
	})

	return tags, len(repositories), nil
}

func (s *Survey) say(format string, args ...any) {
	s.logMu.Lock()
	defer s.logMu.Unlock()

	s.logger()(format, args...)
}

func (s *Survey) emit(header string, body []string) {
	s.logMu.Lock()
	defer s.logMu.Unlock()

	log := s.logger()
	log("%s", header)
	for _, line := range body {
		log("  %s", line)
	}
}

func (s *Survey) parallel() int {
	if s.Parallel > 0 {
		return s.Parallel
	}

	return 4
}

func (s *Survey) logger() copier.Logger {
	if s.Log == nil {
		return func(string, ...any) {}
	}

	return s.Log
}

// eachParallel runs fn over items with at most workers running at once, and
// does not stop at the first failure: a survey exists to find every problem,
// not the first one.
func eachParallel[T any](ctx context.Context, items []T, workers int, fn func(context.Context, int, T)) {
	if len(items) == 0 {
		return
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

feed:
	for i := range items {
		select {
		case queue <- i:
		case <-ctx.Done():
			break feed
		}
	}
	close(queue)
	wg.Wait()
}
