package source

import (
	"context"
	"fmt"
	"regexp"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// Registry is a remote registry that can be asked what it holds.
//
// Copying one image never needs this: a reference names its repository. A
// mirror does, because the whole point is to find the repositories nobody
// wrote down.
type Registry struct {
	registry name.Registry
	opts     Options
}

// OpenRegistry prepares registry, which is a host and optionally a port.
func OpenRegistry(registry string, o Options) (*Registry, error) {
	reg, err := name.NewRegistry(registry, o.nameOptions()...)
	if err != nil {
		return nil, fmt.Errorf("parse registry %q: %w", registry, err)
	}

	return &Registry{registry: reg, opts: o}, nil
}

// Name is the registry host, for logging.
func (r *Registry) Name() string {
	return r.registry.RegistryStr()
}

// Catalog lists every repository the registry will admit to holding, following
// the pagination the distribution specification describes.
//
// A registry may answer with less than everything — the specification lets it
// filter by what the caller may read, and some registries do not implement the
// endpoint at all. That is worth knowing about a mirror: it copies what it was
// shown, and being shown nothing looks exactly like there being nothing.
func (r *Registry) Catalog(ctx context.Context) ([]string, error) {
	repositories, err := remote.Catalog(ctx, r.registry, r.opts.remoteOptions()...)
	if err != nil {
		return nil, fmt.Errorf("catalog %s: %w", r.Name(), err)
	}

	return repositories, nil
}

// Repository opens one repository of this registry by name, without resolving
// anything in it.
func (r *Registry) Repository(_ context.Context, repository string) (*Source, error) {
	repo, err := name.NewRepository(r.registry.RegistryStr()+"/"+repository, r.opts.nameOptions()...)
	if err != nil {
		return nil, fmt.Errorf("parse repository %q: %w", repository, err)
	}

	return &Source{repo: repo, insecure: r.opts.Insecure, opts: r.opts.remoteOptions()}, nil
}

// Tags lists the tags in this repository.
func (s *Source) Tags(ctx context.Context) ([]string, error) {
	tags, err := remote.ListWithContext(ctx, s.repo, s.opts...)
	if err != nil {
		return nil, fmt.Errorf("list tags of %s: %w", s.repo, err)
	}

	return tags, nil
}

// Digest resolves a tag to the digest of the manifest it points at, without
// fetching the manifest.
//
// This is the request a mirror makes for every tag it has already seen, so it
// is deliberately the cheapest one that can answer "has this moved": a HEAD,
// whose response is a digest and nothing else. A tag that does not exist
// reports ok false rather than an error, since a tag can be deleted between
// the listing and the question.
func (s *Source) Digest(ctx context.Context, tag string) (string, bool, error) {
	ref, err := name.NewTag(s.repo.String()+":"+tag, s.nameOptions()...)
	if err != nil {
		return "", false, fmt.Errorf("reference %s: %w", tag, err)
	}

	desc, err := remote.Head(ref, s.withContext(ctx)...)
	if err != nil {
		if isNotFound(err) {
			return "", false, nil
		}

		return "", false, fmt.Errorf("head manifest %s: %w", tag, err)
	}

	return desc.Digest.String(), true, nil
}

// cosignTagPattern matches the tags cosign writes for an artifact attached to
// a manifest, which are named after the digest of their subject.
var cosignTagPattern = regexp.MustCompile(`^sha256-[a-f0-9]{64}\..+$`)

// IsAttachmentTag reports whether a tag is one an artifact was attached under
// rather than one anybody would pull.
//
// A mirror has to skip these, and not because they are unwanted: copying an
// image already carries its signatures, attestations and SBOMs across, so
// treating each fallback tag as an image of its own would copy the same
// objects a second time and publish tags that only make sense relative to a
// subject in the source registry.
//
// The suffix is not checked against the ones cosign uses today. Anything named
// after a digest is an attachment by convention, and a mirror that guessed
// wrong in the other direction would silently duplicate work.
func IsAttachmentTag(tag string) bool {
	return cosignTagPattern.MatchString(tag)
}
