// Package source reads an image, and everything attached to it, out of a
// registry that speaks the OCI distribution API.
package source

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"

	"github.com/lesomnus/derrick/internal/layout"
)

// CosignTagSuffixes are the tags cosign writes when a registry does not
// implement the referrers API. Cosign v3 prefers referrers and keeps these as
// a fallback, and cosign v2 knows only these, so a copy that wants to carry
// signatures has to look in both places.
var CosignTagSuffixes = []string{".sig", ".att", ".sbom"}

// Options configures how the source registry is reached.
type Options struct {
	// Insecure allows a plain-http registry, which is worth having for a
	// registry on a private network that terminates TLS elsewhere.
	Insecure bool

	// Auth overrides the ambient docker credentials.
	Auth authn.Authenticator
}

func (o Options) nameOptions() []name.Option {
	if o.Insecure {
		return []name.Option{name.Insecure}
	}

	return nil
}

func (o Options) remoteOptions() []remote.Option {
	if o.Auth != nil {
		return []remote.Option{remote.WithAuth(o.Auth)}
	}

	// The docker config the operator already logged in with. A publisher runs
	// where builds run, so those credentials are usually right there.
	return []remote.Option{remote.WithAuthFromKeychain(authn.DefaultKeychain)}
}

// Source is one repository in a remote registry.
type Source struct {
	repo     name.Repository
	insecure bool
	opts     []remote.Option
}

// Open resolves reference and returns the repository it lives in along with
// the manifest it points at.
func Open(ctx context.Context, reference string, o Options) (*Source, *Manifest, error) {
	ref, err := name.ParseReference(reference, o.nameOptions()...)
	if err != nil {
		return nil, nil, fmt.Errorf("parse %q: %w", reference, err)
	}

	s := &Source{repo: ref.Context(), insecure: o.Insecure, opts: o.remoteOptions()}

	desc, err := remote.Get(ref, s.withContext(ctx)...)
	if err != nil {
		return nil, nil, fmt.Errorf("resolve %q: %w", reference, err)
	}

	m, err := newManifest(desc)
	if err != nil {
		return nil, nil, err
	}

	return s, m, nil
}

// Manifest is a manifest as it was found in the source, kept together with the
// bytes it was served as. The raw bytes are what gets copied: re-encoding a
// manifest changes its digest and invalidates every signature over it.
type Manifest struct {
	Digest    string
	MediaType string
	Size      int64
	Raw       []byte

	Parsed *layout.Manifest
}

func newManifest(desc *remote.Descriptor) (*Manifest, error) {
	parsed, err := layout.ParseManifest(desc.Manifest)
	if err != nil {
		return nil, fmt.Errorf("manifest %s: %w", desc.Digest.String(), err)
	}

	return &Manifest{
		Digest:    desc.Digest.String(),
		MediaType: string(desc.MediaType),
		Size:      desc.Size,
		Raw:       desc.Manifest,
		Parsed:    parsed,
	}, nil
}

// Repository is the source repository, for logging.
func (s *Source) Repository() string {
	return s.repo.String()
}

// Manifest fetches a manifest by digest.
func (s *Source) Manifest(ctx context.Context, digest string) (*Manifest, error) {
	ref, err := name.NewDigest(s.repo.String()+"@"+digest, s.nameOptions()...)
	if err != nil {
		return nil, fmt.Errorf("reference %s: %w", digest, err)
	}

	desc, err := remote.Get(ref, s.withContext(ctx)...)
	if err != nil {
		return nil, fmt.Errorf("fetch manifest %s: %w", digest, err)
	}

	return newManifest(desc)
}

// ManifestByTag fetches a manifest by tag. A tag that does not exist is not an
// error: it reports ok false, which is how the cosign fallback tags are probed.
func (s *Source) ManifestByTag(ctx context.Context, tag string) (*Manifest, bool, error) {
	ref, err := name.NewTag(s.repo.String()+":"+tag, s.nameOptions()...)
	if err != nil {
		return nil, false, fmt.Errorf("reference %s: %w", tag, err)
	}

	desc, err := remote.Get(ref, s.withContext(ctx)...)
	if err != nil {
		if isNotFound(err) {
			return nil, false, nil
		}

		return nil, false, fmt.Errorf("fetch manifest %s: %w", tag, err)
	}

	m, err := newManifest(desc)
	if err != nil {
		return nil, false, err
	}

	return m, true, nil
}

// Blob opens a blob for reading. The returned stream is verified against the
// digest as it is read, so a truncated or corrupted transfer fails the copy
// rather than landing in the bucket.
func (s *Source) Blob(ctx context.Context, digest string) (io.ReadCloser, error) {
	ref, err := name.NewDigest(s.repo.String()+"@"+digest, s.nameOptions()...)
	if err != nil {
		return nil, fmt.Errorf("reference %s: %w", digest, err)
	}

	layer, err := remote.Layer(ref, s.withContext(ctx)...)
	if err != nil {
		return nil, fmt.Errorf("fetch blob %s: %w", digest, err)
	}

	rc, err := layer.Compressed()
	if err != nil {
		return nil, fmt.Errorf("open blob %s: %w", digest, err)
	}

	return rc, nil
}

// Referrers lists the manifests that name digest as their subject. A registry
// that does not implement the referrers API reports none, which is not an
// error; the cosign fallback tags cover that case.
func (s *Source) Referrers(ctx context.Context, digest string) ([]layout.Descriptor, error) {
	ref, err := name.NewDigest(s.repo.String()+"@"+digest, s.nameOptions()...)
	if err != nil {
		return nil, fmt.Errorf("reference %s: %w", digest, err)
	}

	index, err := remote.Referrers(ref, s.withContext(ctx)...)
	if err != nil {
		if isNotFound(err) || isUnsupported(err) {
			return nil, nil
		}

		return nil, fmt.Errorf("list referrers of %s: %w", digest, err)
	}

	manifest, err := index.IndexManifest()
	if err != nil {
		return nil, fmt.Errorf("read referrers of %s: %w", digest, err)
	}

	descriptors := make([]layout.Descriptor, 0, len(manifest.Manifests))
	for _, d := range manifest.Manifests {
		descriptors = append(descriptors, toDescriptor(d))
	}

	return descriptors, nil
}

// CosignTags returns the fallback tags cosign would have used for artifacts
// attached to digest.
func CosignTags(digest string) []string {
	prefix := strings.Replace(digest, ":", "-", 1)

	tags := make([]string, 0, len(CosignTagSuffixes))
	for _, suffix := range CosignTagSuffixes {
		tags = append(tags, prefix+suffix)
	}

	return tags
}

func (s *Source) nameOptions() []name.Option {
	if s.insecure {
		return []name.Option{name.Insecure}
	}

	return nil
}

func (s *Source) withContext(ctx context.Context) []remote.Option {
	opts := make([]remote.Option, 0, len(s.opts)+1)
	opts = append(opts, s.opts...)
	opts = append(opts, remote.WithContext(ctx))

	return opts
}

func toDescriptor(d v1.Descriptor) layout.Descriptor {
	return layout.Descriptor{
		MediaType:    string(d.MediaType),
		Digest:       d.Digest.String(),
		Size:         d.Size,
		ArtifactType: d.ArtifactType,
		URLs:         d.URLs,
		Annotations:  d.Annotations,
	}
}

func isNotFound(err error) bool {
	var terr *transport.Error
	if errors.As(err, &terr) {
		if terr.StatusCode == http.StatusNotFound {
			return true
		}
		for _, e := range terr.Errors {
			switch e.Code {
			case transport.ManifestUnknownErrorCode, transport.NameUnknownErrorCode, transport.BlobUnknownErrorCode:
				return true
			}
		}
	}

	return false
}

func isUnsupported(err error) bool {
	var terr *transport.Error
	if errors.As(err, &terr) {
		return terr.StatusCode == http.StatusNotImplemented || terr.StatusCode == http.StatusMethodNotAllowed
	}

	return false
}
