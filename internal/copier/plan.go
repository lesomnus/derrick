package copier

import (
	"context"
	"fmt"

	"github.com/lesomnus/derrick/internal/layout"
	"github.com/lesomnus/derrick/internal/source"
)

// maxReferrerRounds bounds how far a chain of referrers is followed. A
// signature can itself be signed, and an attestation can attest to an
// attestation, but a chain long enough to hit this bound is a loop or an
// abuse rather than a release.
const maxReferrerRounds = 8

// How hard to look for cosign's fallback tags.
const (
	// CosignTagsRoot probes only the manifest being published. This is the
	// default because `cosign sign <ref>` signs what the reference resolves
	// to, and that is the manifest being published.
	CosignTagsRoot = "root"

	// CosignTagsAll probes every manifest in the graph, which is what a
	// `cosign sign --recursive` over a multi-architecture image produces. It
	// costs three requests per manifest, almost all of which are misses, and
	// on a large index that is enough to trip a registry's rate limit.
	CosignTagsAll = "all"

	// CosignTagsNone skips the fallback entirely, leaving only the referrers
	// API. Safe when everything is signed by cosign v3 against a registry that
	// implements referrers.
	CosignTagsNone = "none"
)

// Plan is everything a copy will write, in the order it has to be written.
//
// The order is the whole point. Blobs land before any manifest that names
// them, a manifest lands after everything it references, and the tag lands
// last, so that a registry serving out of this bucket never sees a manifest
// whose blobs are missing. Until the tag object exists, nothing pulling by tag
// can see the new image at all.
type Plan struct {
	// Blobs are the layers and configs to upload, deduplicated.
	Blobs []layout.Descriptor

	// Manifests are in dependency order: children before the index that names
	// them, subjects before the referrers that attach to them.
	Manifests []*source.Manifest

	// Referrers are the descriptor objects that let the registry answer the
	// referrers API for a subject.
	Referrers []Referrer

	// Tags are the tag objects to write. The tag the caller asked for is last,
	// because writing it is what publishes the image.
	Tags []Tag

	// Foreign are referenced blobs that live outside any registry and are
	// therefore not copied.
	Foreign []layout.Descriptor
}

// Referrer is a stored descriptor attaching one manifest to its subject.
type Referrer struct {
	Subject    string
	Digest     string
	Descriptor layout.Descriptor
}

// Tag names a manifest that must also be reachable by tag.
type Tag struct {
	Name     string
	Manifest *source.Manifest
}

type planner struct {
	src Source

	plan          *Plan
	seenManifests map[string]bool
	seenBlobs     map[string]bool
	log           Logger
	withReferrers bool
	cosignTags    string
	root          string
}

// Plan walks the source image and everything attached to it, and returns what
// needs to be written.
func (c *Copier) Plan(ctx context.Context, root *source.Manifest, tag string) (*Plan, error) {
	cosignTags := c.CosignTags
	if cosignTags == "" {
		cosignTags = CosignTagsRoot
	}
	switch cosignTags {
	case CosignTagsRoot, CosignTagsAll, CosignTagsNone:
	default:
		return nil, fmt.Errorf("cosign-tags must be %q, %q or %q, not %q",
			CosignTagsRoot, CosignTagsAll, CosignTagsNone, cosignTags)
	}

	p := &planner{
		src:           c.Source,
		plan:          &Plan{},
		seenManifests: map[string]bool{},
		seenBlobs:     map[string]bool{},
		log:           c.logger(),
		withReferrers: !c.NoReferrers,
		cosignTags:    cosignTags,
		root:          root.Digest,
	}

	if err := p.visit(ctx, root); err != nil {
		return nil, err
	}

	if p.withReferrers {
		if err := p.collectAttachments(ctx); err != nil {
			return nil, err
		}
	}

	p.plan.Tags = append(p.plan.Tags, Tag{Name: tag, Manifest: root})

	return p.plan, nil
}

// visit adds a manifest and everything below it, children first.
func (p *planner) visit(ctx context.Context, m *source.Manifest) error {
	if p.seenManifests[m.Digest] {
		return nil
	}
	p.seenManifests[m.Digest] = true

	for _, child := range m.Parsed.Manifests {
		if child.Foreign() {
			// An index entry is a manifest in this registry or it is nothing.
			return fmt.Errorf("manifest %s: child %s is marked foreign, which an index entry may not be", m.Digest, child.Digest)
		}

		cm, err := p.src.Manifest(ctx, child.Digest)
		if err != nil {
			return err
		}
		if err := p.visit(ctx, cm); err != nil {
			return err
		}
	}

	for _, blob := range m.Parsed.Blobs() {
		if p.seenBlobs[blob.Digest] {
			continue
		}
		p.seenBlobs[blob.Digest] = true
		p.plan.Blobs = append(p.plan.Blobs, blob)
	}

	p.plan.Foreign = append(p.plan.Foreign, m.Parsed.ForeignBlobs()...)

	// Post-order: this manifest is only valid once everything it names is
	// planned ahead of it.
	p.plan.Manifests = append(p.plan.Manifests, m)

	if descriptor := m.Parsed.ReferrerDescriptor(m.Digest, m.Size); descriptor != nil {
		p.plan.Referrers = append(p.plan.Referrers, Referrer{
			Subject:    m.Parsed.Subject.Digest,
			Digest:     m.Digest,
			Descriptor: *descriptor,
		})
	}

	return nil
}

// collectAttachments finds signatures, attestations and SBOMs attached to
// anything already planned, and plans them too.
//
// Both places are searched. Cosign v3 attaches through the referrers API, v2
// through a tag named after the subject digest, and v3 still writes the tag as
// a fallback for registries without referrers. Which one a given image has
// depends on what signed it and what registry it was signed in, so a copy that
// only looked in one place would silently drop signatures.
func (p *planner) collectAttachments(ctx context.Context) error {
	for round := 0; round < maxReferrerRounds; round++ {
		subjects := make([]string, 0, len(p.seenManifests))
		for digest := range p.seenManifests {
			subjects = append(subjects, digest)
		}

		found := false
		for _, subject := range subjects {
			referrers, err := p.src.Referrers(ctx, subject)
			if err != nil {
				return err
			}
			for _, referrer := range referrers {
				if p.seenManifests[referrer.Digest] {
					continue
				}
				m, err := p.src.Manifest(ctx, referrer.Digest)
				if err != nil {
					return err
				}
				p.log("found referrer %s of %s", referrer.Digest, subject)
				if err := p.visit(ctx, m); err != nil {
					return err
				}
				found = true
			}

			if !p.probeCosignTags(subject) {
				continue
			}

			for _, tag := range source.CosignTags(subject) {
				m, ok, err := p.src.ManifestByTag(ctx, tag)
				if err != nil {
					return err
				}
				if !ok {
					continue
				}
				if !p.seenManifests[m.Digest] {
					p.log("found %s attached to %s", tag, subject)
					if err := p.visit(ctx, m); err != nil {
						return err
					}
					found = true
				}
				if !p.taggedAlready(tag) {
					// The tag is how a client without referrers support finds
					// this artifact, so it has to exist in the target too.
					p.plan.Tags = append(p.plan.Tags, Tag{Name: tag, Manifest: m})
					found = true
				}
			}
		}

		if !found {
			return nil
		}
	}

	return fmt.Errorf("referrer chain did not settle after %d rounds", maxReferrerRounds)
}

// probeCosignTags reports whether the fallback tags are worth asking about for
// this subject.
func (p *planner) probeCosignTags(subject string) bool {
	switch p.cosignTags {
	case CosignTagsNone:
		return false
	case CosignTagsAll:
		return true
	default:
		return subject == p.root
	}
}

func (p *planner) taggedAlready(tag string) bool {
	for _, t := range p.plan.Tags {
		if t.Name == tag {
			return true
		}
	}

	return false
}
