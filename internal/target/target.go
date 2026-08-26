// Package target parses where a copy is going.
package target

import (
	"fmt"
	"strings"
)

// Scheme is the only destination scheme derrick understands.
const Scheme = "s3://"

// Reference is a repository and tag inside a bucket.
type Reference struct {
	Bucket     string
	Repository string
	Tag        string
}

// Parse reads a destination of the form s3://<bucket>/<repository>:<tag>.
//
// The bucket is part of the reference rather than a flag on purpose: derrick
// writes into any bucket a serverless-registry deployment serves, and which
// one is a property of the destination, not of the tool.
func Parse(s string) (Reference, error) {
	rest, ok := strings.CutPrefix(s, Scheme)
	if !ok {
		return Reference{}, fmt.Errorf("destination %q must begin with %s", s, Scheme)
	}

	bucket, path, ok := strings.Cut(rest, "/")
	if !ok || bucket == "" {
		return Reference{}, fmt.Errorf("destination %q must name a bucket and a repository, as %s<bucket>/<repository>:<tag>", s, Scheme)
	}

	index := strings.LastIndex(path, ":")
	if index < 0 {
		return Reference{}, fmt.Errorf("destination %q must end in a tag, as %s<bucket>/<repository>:<tag>", s, Scheme)
	}

	repository, tag := path[:index], path[index+1:]
	if strings.Contains(tag, "/") {
		return Reference{}, fmt.Errorf("destination %q has a slash in its tag", s)
	}

	return Reference{Bucket: bucket, Repository: repository, Tag: tag}, nil
}

// String renders the reference back to the form Parse accepts.
func (r Reference) String() string {
	return fmt.Sprintf("%s%s/%s:%s", Scheme, r.Bucket, r.Repository, r.Tag)
}
