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

func TestParseMirrorSource(t *testing.T) {
	for _, tc := range []struct {
		in       string
		registry string
		prefix   string
		bad      bool
	}{
		{in: "cr.hday.io/dist", registry: "cr.hday.io", prefix: "dist"},
		{in: "cr.hday.io/dist/", registry: "cr.hday.io", prefix: "dist"},
		{in: "cr.hday.io", registry: "cr.hday.io"},
		{in: "cr.hday.io/dist/nested", registry: "cr.hday.io", prefix: "dist/nested"},
		{in: "localhost:5000/dist", registry: "localhost:5000", prefix: "dist"},
		{in: "https://cr.hday.io/dist", bad: true},
		{in: "cr.hday.io/dist/perception:1.4.2", bad: true},
		{in: "", bad: true},
	} {
		t.Run(tc.in, func(t *testing.T) {
			registry, prefix, err := parseMirrorSource(tc.in)
			if tc.bad {
				if err == nil {
					t.Fatalf("parseMirrorSource(%q) was accepted", tc.in)
				}

				return
			}
			if err != nil {
				t.Fatalf("parseMirrorSource(%q): %v", tc.in, err)
			}
			if registry != tc.registry || prefix != tc.prefix {
				t.Errorf("parseMirrorSource(%q) = %q, %q, want %q, %q", tc.in, registry, prefix, tc.registry, tc.prefix)
			}
		})
	}
}

func TestParseBucket(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want string
		bad  bool
	}{
		{in: "s3://registry-hday-io", want: "registry-hday-io"},
		{in: "s3://registry-hday-io/", want: "registry-hday-io"},
		// A mirror carries the repository name across unchanged, so a
		// destination that names one is a misunderstanding worth refusing.
		{in: "s3://registry-hday-io/dist", bad: true},
		{in: "registry-hday-io", bad: true},
		{in: "s3://", bad: true},
	} {
		t.Run(tc.in, func(t *testing.T) {
			got, err := parseBucket(tc.in)
			if tc.bad {
				if err == nil {
					t.Fatalf("parseBucket(%q) was accepted as %q", tc.in, got)
				}

				return
			}
			if err != nil {
				t.Fatalf("parseBucket(%q): %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("parseBucket(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestParseRepository(t *testing.T) {
	for _, tc := range []struct {
		in         string
		bucket     string
		repository string
		bad        bool
	}{
		{in: "s3://registry-hday-io/dist/perception", bucket: "registry-hday-io", repository: "dist/perception"},
		{in: "s3://registry-hday-io/dist/perception/", bucket: "registry-hday-io", repository: "dist/perception"},
		{in: "s3://registry-hday-io/dist", bucket: "registry-hday-io", repository: "dist"},
		// Pruning is a question about a repository; removing a tag is `untag`.
		{in: "s3://registry-hday-io/dist/perception:1.4.2", bad: true},
		{in: "s3://registry-hday-io", bad: true},
		{in: "registry-hday-io/dist", bad: true},
	} {
		t.Run(tc.in, func(t *testing.T) {
			bucket, repository, err := parseRepository(tc.in)
			if tc.bad {
				if err == nil {
					t.Fatalf("parseRepository(%q) was accepted as %q, %q", tc.in, bucket, repository)
				}

				return
			}
			if err != nil {
				t.Fatalf("parseRepository(%q): %v", tc.in, err)
			}
			if bucket != tc.bucket || repository != tc.repository {
				t.Errorf("parseRepository(%q) = %q, %q, want %q, %q", tc.in, bucket, repository, tc.bucket, tc.repository)
			}
		})
	}
}
