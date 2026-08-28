// Package ledger records which tags have already been mirrored, so a later run
// can decide to skip one without walking the image behind it.
//
// # It is a cache, and never the truth
//
// The bucket is the truth: a tag is published when the objects are there, and
// nothing this package records changes that. What the ledger buys is the cost
// of asking. Deciding whether a tag needs copying without one means reading
// the destination — the tag object, the manifests under it, the blobs under
// those — which is a walk per tag, and tags are the thing that grows. With
// one, a tag that has not moved costs a single HEAD against the source.
//
// Losing the ledger therefore costs requests and never correctness: the next
// run re-examines everything and copies nothing that is already there, because
// every object below a tag is content-addressed and skipped when present.
//
// # Entries are keyed by digest, not by name
//
// Recording "dist/perception:1.4.2 is done" would be wrong the moment that tag
// moves, and it would stay wrong forever. An entry records the digest the tag
// had when it was copied, so the skip is conditional on the source still
// pointing there, and a moved tag re-copies on its own.
package ledger

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/lesomnus/derrick/internal/blobstore"
)

// DefaultKey is where the ledger lives in the bucket it describes.
//
// It sits beside the repositories rather than somewhere else on purpose: the
// thing it is a cache of is this bucket, and a cache that can be separated
// from what it describes will eventually describe something else.
//
// The leading underscore is what keeps it out of the way. A repository name
// may not begin with one — the distribution specification requires a path
// component to start with a letter or a digit — so no image can ever be
// published to a key that collides with this one, and no route the registry
// serves can be made to read it.
const DefaultKey = "_derrick/ledger.jsonl"

// ContentType is what the ledger object is stored as. It is newline-delimited
// JSON so that it stays readable, greppable and diffable as it grows, which a
// single JSON document of the same content would not.
const ContentType = "application/x-ndjson"

// Entry is one mirrored tag.
type Entry struct {
	Repository string    `json:"repository"`
	Tag        string    `json:"tag"`
	Digest     string    `json:"digest"`
	At         time.Time `json:"at"`
}

// Ledger is a set of entries, safe to record into from several goroutines.
type Ledger struct {
	mu      sync.Mutex
	entries map[string]Entry
	dirty   bool
}

// New returns an empty ledger.
func New() *Ledger {
	return &Ledger{entries: map[string]Entry{}}
}

// Load reads the ledger stored at key. A key that does not exist is not an
// error: a bucket that has never been mirrored has no ledger, and that is the
// same situation as one whose ledger was deleted.
func Load(ctx context.Context, store blobstore.Store, key string) (*Ledger, error) {
	obj, err := store.Stat(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("stat ledger %q: %w", key, err)
	}
	if obj == nil {
		return New(), nil
	}

	rc, err := store.Get(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("read ledger %q: %w", key, err)
	}
	defer rc.Close()

	l, err := Parse(rc)
	if err != nil {
		return nil, fmt.Errorf("%q: %w", key, err)
	}

	return l, nil
}

// Parse reads newline-delimited entries.
func Parse(r io.Reader) (*Ledger, error) {
	l := New()

	scanner := bufio.NewScanner(r)
	// A line is one entry, and an entry is small; the default limit would
	// still be generous, but a ledger is machine-written and a surprise here
	// would look like truncation rather than an error.
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for line := 1; scanner.Scan(); line++ {
		text := strings.TrimSpace(scanner.Text())
		if text == "" {
			continue
		}

		var entry Entry
		if err := json.Unmarshal([]byte(text), &entry); err != nil {
			return nil, fmt.Errorf("line %d: %w (the ledger is only a cache; deleting it costs a slower run and nothing else)", line, err)
		}
		if entry.Repository == "" || entry.Tag == "" || entry.Digest == "" {
			return nil, fmt.Errorf("line %d: an entry needs a repository, a tag and a digest", line)
		}

		l.entries[index(entry.Repository, entry.Tag)] = entry
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read ledger: %w", err)
	}

	return l, nil
}

// Lookup returns what was recorded for a tag.
func (l *Ledger) Lookup(repository, tag string) (Entry, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()

	entry, ok := l.entries[index(repository, tag)]

	return entry, ok
}

// Record notes that a tag was mirrored at the digest it then had.
func (l *Ledger) Record(repository, tag, digest string, at time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.entries[index(repository, tag)] = Entry{
		Repository: repository,
		Tag:        tag,
		Digest:     digest,
		At:         at.UTC(),
	}
	l.dirty = true
}

// Forget drops what was recorded for a tag, so the next run examines it again.
func (l *Ledger) Forget(repository, tag string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	key := index(repository, tag)
	if _, ok := l.entries[key]; !ok {
		return
	}

	delete(l.entries, key)
	l.dirty = true
}

// Len is how many tags the ledger has recorded.
func (l *Ledger) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()

	return len(l.entries)
}

// Entries returns every entry, ordered by repository and then by tag, which is
// the order the object is written in so that two runs that changed nothing
// produce the same bytes.
func (l *Ledger) Entries() []Entry {
	l.mu.Lock()
	defer l.mu.Unlock()

	entries := make([]Entry, 0, len(l.entries))
	for _, entry := range l.entries {
		entries = append(entries, entry)
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Repository != entries[j].Repository {
			return entries[i].Repository < entries[j].Repository
		}

		return entries[i].Tag < entries[j].Tag
	})

	return entries
}

// WriteTo renders the ledger as newline-delimited JSON.
func (l *Ledger) WriteTo(w io.Writer) (int64, error) {
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	for _, entry := range l.Entries() {
		if err := encoder.Encode(entry); err != nil {
			return 0, fmt.Errorf("encode entry %s:%s: %w", entry.Repository, entry.Tag, err)
		}
	}

	n, err := w.Write(buf.Bytes())

	return int64(n), err
}

// Save writes the ledger back to key, and reports whether it wrote anything.
//
// The whole object is rewritten rather than appended to, because an object
// store has no append and a ledger of even a hundred thousand tags is a few
// megabytes. A run saves after each repository, so an interrupted run keeps
// the progress it made.
func (l *Ledger) Save(ctx context.Context, store blobstore.Store, key string) (bool, error) {
	l.mu.Lock()
	dirty := l.dirty
	l.mu.Unlock()

	if !dirty {
		return false, nil
	}

	var buf bytes.Buffer
	if _, err := l.WriteTo(&buf); err != nil {
		return false, err
	}

	if err := store.Put(ctx, key, bytes.NewReader(buf.Bytes()), blobstore.PutOptions{
		ContentType: ContentType,
	}); err != nil {
		return false, fmt.Errorf("write ledger %q: %w", key, err)
	}

	l.mu.Lock()
	l.dirty = false
	l.mu.Unlock()

	return true, nil
}

// index is the map key for a tag. A repository may contain slashes and a tag
// may not contain a colon, so joining on one cannot collide.
func index(repository, tag string) string {
	return repository + ":" + tag
}
