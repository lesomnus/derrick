package ledger_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/lesomnus/derrick/internal/blobstore"
	"github.com/lesomnus/derrick/internal/ledger"
)

var when = time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)

func TestLoadWithoutObject(t *testing.T) {
	store := blobstore.NewMemory()

	book, err := ledger.Load(context.Background(), store, ledger.DefaultKey)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if book.Len() != 0 {
		t.Fatalf("a bucket with no ledger holds %d entries, want 0", book.Len())
	}
}

func TestRoundTrip(t *testing.T) {
	ctx := context.Background()
	store := blobstore.NewMemory()

	book := ledger.New()
	book.Record("dist/perception", "1.4.2", "sha256:aa", when)
	book.Record("dist/control", "2.0.0", "sha256:bb", when)

	saved, err := book.Save(ctx, store, ledger.DefaultKey)
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if !saved {
		t.Fatal("a ledger with new entries reported nothing to save")
	}

	// Saving again writes nothing, so a run that copied nothing does not churn
	// the object.
	saved, err = book.Save(ctx, store, ledger.DefaultKey)
	if err != nil {
		t.Fatalf("save again: %v", err)
	}
	if saved {
		t.Fatal("an unchanged ledger was written again")
	}

	back, err := ledger.Load(ctx, store, ledger.DefaultKey)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	entry, ok := back.Lookup("dist/perception", "1.4.2")
	if !ok {
		t.Fatal("dist/perception:1.4.2 did not survive the round trip")
	}
	if entry.Digest != "sha256:aa" {
		t.Fatalf("digest is %q, want sha256:aa", entry.Digest)
	}
	if !entry.At.Equal(when) {
		t.Fatalf("recorded at %s, want %s", entry.At, when)
	}
}

func TestEntriesAreSorted(t *testing.T) {
	book := ledger.New()
	book.Record("dist/perception", "1.4.2", "sha256:aa", when)
	book.Record("dist/control", "2.0.0", "sha256:bb", when)
	book.Record("dist/perception", "1.4.1", "sha256:cc", when)

	var got []string
	for _, entry := range book.Entries() {
		got = append(got, entry.Repository+":"+entry.Tag)
	}

	want := []string{"dist/control:2.0.0", "dist/perception:1.4.1", "dist/perception:1.4.2"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestForget(t *testing.T) {
	book := ledger.New()
	book.Record("dist/perception", "1.4.2", "sha256:aa", when)
	book.Forget("dist/perception", "1.4.2")

	if _, ok := book.Lookup("dist/perception", "1.4.2"); ok {
		t.Fatal("a forgotten entry is still there")
	}
}

func TestParseRejectsAMalformedLine(t *testing.T) {
	_, err := ledger.Parse(strings.NewReader(`{"repository":"dist/a","tag":"1","digest":"sha256:aa"}` + "\nnot json\n"))
	if err == nil {
		t.Fatal("a malformed ledger parsed without complaint")
	}
	if !strings.Contains(err.Error(), "line 2") {
		t.Fatalf("the error does not say which line: %v", err)
	}
}

func TestParseRejectsAnIncompleteEntry(t *testing.T) {
	_, err := ledger.Parse(strings.NewReader(`{"repository":"dist/a","tag":"1"}` + "\n"))
	if err == nil {
		t.Fatal("an entry without a digest parsed without complaint")
	}
}
