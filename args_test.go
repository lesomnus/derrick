package main

import (
	"flag"
	"slices"
	"testing"
)

func TestReorderPutsFlagsFirst(t *testing.T) {
	newSet := func() *flag.FlagSet {
		fs := flag.NewFlagSet("copy", flag.ContinueOnError)
		fs.String("endpoint", "", "")
		fs.Int("concurrency", 4, "")
		fs.Bool("dry-run", false, "")

		return fs
	}

	for _, tc := range []struct {
		name string
		in   []string
		want []string
	}{
		{
			// How anyone actually writes a copy.
			name: "flags after positionals",
			in:   []string{"src", "dst", "--endpoint", "https://r2", "--dry-run"},
			want: []string{"--endpoint", "https://r2", "--dry-run", "src", "dst"},
		},
		{
			name: "inline values",
			in:   []string{"src", "--endpoint=https://r2", "dst"},
			want: []string{"--endpoint=https://r2", "src", "dst"},
		},
		{
			name: "a bool flag does not swallow the next argument",
			in:   []string{"--dry-run", "src", "dst"},
			want: []string{"--dry-run", "src", "dst"},
		},
		{
			name: "already in order",
			in:   []string{"-concurrency", "8", "src", "dst"},
			want: []string{"-concurrency", "8", "src", "dst"},
		},
		{
			name: "everything after -- is positional",
			in:   []string{"--dry-run", "--", "-weird-src", "dst"},
			want: []string{"--dry-run", "-weird-src", "dst"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := reorder(newSet(), tc.in); !slices.Equal(got, tc.want) {
				t.Errorf("reorder = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestReorderParses(t *testing.T) {
	fs := flag.NewFlagSet("copy", flag.ContinueOnError)
	endpoint := fs.String("endpoint", "", "")
	dryRun := fs.Bool("dry-run", false, "")

	if err := fs.Parse(reorder(fs, []string{"src", "dst", "--endpoint", "https://r2", "--dry-run"})); err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if *endpoint != "https://r2" {
		t.Errorf("endpoint = %q", *endpoint)
	}
	if !*dryRun {
		t.Error("dry-run was not set")
	}
	if fs.NArg() != 2 || fs.Arg(0) != "src" || fs.Arg(1) != "dst" {
		t.Errorf("positionals = %q", fs.Args())
	}
}
