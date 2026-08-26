package target_test

import (
	"testing"

	"github.com/lesomnus/derrick/internal/target"
)

func TestParse(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want target.Reference
	}{
		{
			in:   "s3://registry-hday-io/robot/perception:1.4.2",
			want: target.Reference{Bucket: "registry-hday-io", Repository: "robot/perception", Tag: "1.4.2"},
		},
		{
			in:   "s3://bucket/single:latest",
			want: target.Reference{Bucket: "bucket", Repository: "single", Tag: "latest"},
		},
		{
			// Cosign's fallback tags carry a colon-free digest and a suffix,
			// and they have to survive a round trip.
			in:   "s3://bucket/robot/perception:sha256-aaaa.sig",
			want: target.Reference{Bucket: "bucket", Repository: "robot/perception", Tag: "sha256-aaaa.sig"},
		},
		{
			in:   "s3://bucket/a/b/c/d:v1",
			want: target.Reference{Bucket: "bucket", Repository: "a/b/c/d", Tag: "v1"},
		},
	} {
		t.Run(tc.in, func(t *testing.T) {
			got, err := target.Parse(tc.in)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if got != tc.want {
				t.Errorf("Parse = %+v, want %+v", got, tc.want)
			}
			if round := got.String(); round != tc.in {
				t.Errorf("String = %q, want %q", round, tc.in)
			}
		})
	}
}

func TestParseRejects(t *testing.T) {
	for _, in := range []string{
		"registry-hday-io/robot:1.4.2",     // no scheme
		"s3://robot:1.4.2",                 // no repository
		"s3:///robot:1.4.2",                // no bucket
		"s3://bucket/robot/perception",     // no tag
		"s3://bucket/robot:tag/with/slash", // slash in tag
	} {
		t.Run(in, func(t *testing.T) {
			if _, err := target.Parse(in); err == nil {
				t.Errorf("Parse(%q) succeeded, want an error", in)
			}
		})
	}
}
