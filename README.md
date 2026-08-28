# derrick

Copies container images from a registry into an S3-compatible bucket laid out
for [serverless-registry](https://github.com/cloudflare/serverless-registry) to
serve — signatures, attestations and SBOMs included.

```bash
derrick copy registry.internal/perception:1.4.2 \
  s3://my-registry-bucket/robot/perception:1.4.2 \
  --endpoint https://<account>.r2.cloudflarestorage.com
```

## Why this exists

serverless-registry serves images straight out of object storage. Publishing
into it by pushing through the Worker works, but every layer then travels in a
request body, and Workers cap those by plan — 100 MB on Free and Pro, 500 MB on
Enterprise. Real layers are bigger than that.

Writing to the bucket directly removes the cap, and takes the registry off the
publish path entirely. What it costs is that something has to lay the objects
out the way the registry reads them, and no existing tool does: `crane`,
`skopeo`, `oras` and `regclient` all speak the registry API, not object
storage. That is the gap derrick fills.

## What it copies

Everything reachable from the image:

- every layer and config blob, deduplicated and skipped when already present
- every manifest, including each architecture of a multi-architecture index
- signatures, attestations and SBOMs, found **both** through the referrers API
  and through the `sha256-<digest>.sig` tags cosign falls back to

By default the fallback tags are only looked for on the manifest being
published, because that is what `cosign sign <ref>` signs. Probing them costs
three requests per subject and nearly all of them miss, so asking about every
manifest in a large index is enough on its own to trip a registry's rate limit.
`--cosign-tags=all` asks about all of them, which is what a
`cosign sign --recursive` over a multi-architecture image needs.
- the referrer descriptor objects that let the registry answer the referrers
  API, so `cosign verify` finds what it is looking for

derrick does not sign anything. Sign in the source registry with cosign, and
derrick carries the result across intact — manifests are copied byte for byte,
so digests, and every signature made over them, survive.

Foreign (non-distributable) layers are not copied, because they are not in the
source registry to copy. derrick warns when it meets one: an image carrying
them is not wholly served by your registry.

## Ordering

The order objects are written is the contract, not an implementation detail:

1. **blobs** — nothing may reference an object that is not there yet
2. **manifests**, children before the index that names them, subjects before
   the referrers attached to them
3. **referrer descriptors**, once their subjects exist
4. **the tag**

Writing the tag is the commit. Until that object exists, the new image is
invisible to anything pulling by tag, and writing it is a single object write,
so a tag move is atomic from a client's point of view. That is what makes it
safe to publish while a fleet is pulling.

## Install

```bash
go install github.com/lesomnus/derrick@latest
```

## Usage

```
derrick copy [flags] <source-image> s3://<bucket>/<repository>:<tag>
derrick mirror [flags] <source-registry>[/<prefix>] s3://<bucket>
derrick verify [flags] s3://<bucket>/<repository>:<tag>
derrick untag [flags] s3://<bucket>/<repository>:<tag>
derrick prune [flags] s3://<bucket>/<repository>
derrick version
```

| flag | |
| --- | --- |
| `--endpoint` | S3 endpoint, e.g. `https://<account>.r2.cloudflarestorage.com` |
| `--region` | default `auto` |
| `--concurrency` | objects in flight at once, default 4 |
| `--dry-run` | plan and report, write nothing |
| `--verify` | re-read every object after writing it |
| `--no-referrers` | skip signatures and attestations (debugging only) |
| `--cosign-tags` | how hard to look for cosign's fallback tags: `root` (default), `all`, `none` |
| `--src-insecure` | allow a plain-http source registry |
| `--quiet` | only report the outcome |

Credentials come from the environment: `AWS_ACCESS_KEY_ID` and
`AWS_SECRET_ACCESS_KEY` for the bucket, the ambient docker login for the source
registry.

### verify

`verify` walks a published tag reading only the bucket, and reports what a pull
would trip over:

```bash
$ derrick verify s3://my-registry-bucket/robot/perception:1.4.2 --endpoint ...
s3://my-registry-bucket/robot/perception:1.4.2 is complete at sha256:9f86d0…
  3 manifests, 14 blobs, 1 referrers
```

A publish that stopped halfway looks healthy from the outside until something
pulls the layer that is missing. On a robot fleet that is a bad place to find
out.

### mirror

`mirror` is `copy` with the references filled in by the source registry instead
of by an operator: it lists the repositories under a prefix, lists the tags in
each, and copies the ones that are not already published.

```bash
derrick mirror cr.hday.io/dist s3://my-registry-bucket --endpoint ...
```

The repository name is carried across unchanged, so what a client pulls from
the mirror differs from what it pulls from the source only in the hostname.
There is deliberately no way to rewrite it: a mirror whose names do not match
its source is a mirror nobody can reason about. That is also why the
destination is a bucket and not a reference — there is nothing else to say.

| flag | |
| --- | --- |
| `--parallel` | tags of one repository examined at once, default 4 |
| `--exclude-repository` | `path.Match` pattern not to mirror; repeatable |
| `--exclude-tag` | same, for tags |
| `--prune` | remove destination tags the source no longer has |

plus everything `copy` takes.

Tags named after a digest — `sha256-<hex>.sig` and friends — are skipped as
images of their own. They are not lost: copying an image already carries its
signatures and attestations across, so mirroring them separately would copy
the same objects twice and publish tags that only mean something relative to a
subject in the source registry.

One repository failing does not stop the rest. A mirror of a hundred
repositories should not abandon ninety-nine of them because one image is
broken, so failures are collected, reported at the end, and the exit code is
non-zero.

### What it skips

A tag is copied when the bucket does not already have it at the digest the
source has it at. That is two `HEAD` requests — one asking the source where the
tag points, one asking the bucket what is under that tag — and no more: the
image behind an unchanged tag is never fetched, and neither is anything below
it.

There is deliberately no record kept between runs. An earlier version of this
wrote down what it had copied, on the theory that it saved a walk of the
destination; it did not, because the check above was never a walk. What a
record would save is the second of those two `HEAD`s, against a bucket, which
is the cheaper half of an already cheap question — and it would cost the thing
that makes this simple. A record can be wrong. Something deletes an object by
hand, a run dies between the copy and the save, and now there are two opinions
about what is published and a flag to reconcile them. Asking the bucket has one
opinion, and it is the one that serves the pull.

So a tag that moved, an object someone removed, and a run that was interrupted
all come out right on the next run without anyone deciding anything.

What that costs is one bucket `HEAD` per tag per run: at ten thousand tags,
well inside what an object store gives away, and a second or two of wall clock
at any sane parallelism. The request that cannot be avoided either way is the
one against the source, and it is the expensive one.

Every tag is said out loud either way — copied, already present at a digest,
attached to something else, or excluded by a pattern. A run that copies nothing
should account for what it decided rather than report that it decided, and the
log of one is a complete list of what is in the bucket and how it got there.

A copy prints as a block: what happened to the tag, and under it, indented,
every object that moved or did not.

```
dist/perception:1.4.2 copied at sha256:d7f3a1… in 1.4s — 3 blobs 41.2 MiB uploaded, 11 already present, 4 manifests, 1 referrers
  blob sha256:2cd0e7… 18.1 MiB uploaded in 640ms (28.3 MiB/s)
  blob sha256:1f6ea5… 4.2 MiB present
  manifest sha256:b724a0… (application/vnd.oci.image.manifest.v1+json)
  tag 1.4.2 -> sha256:d7f3a1…
```

Several tags copy at once, and the blocks stay whole: each copy's log is
collected while it runs and printed when it finishes, so the interleaving
happens between blocks rather than inside them.

### untag and prune

Removing an image is two operations, because it is two questions.

`untag` answers the first: this tag should not be pullable any more.

```bash
derrick untag s3://my-registry-bucket/dist/perception:1.4.1 --endpoint ...
```

It deletes the one object the tag is, which is atomic from a client's point of
view in the same way publishing it was. The image stays pullable by digest and
by any other tag pointing at it. Nothing else has to be told: the next mirror
asks the bucket, and the bucket no longer has the tag.

`prune` answers the second: what in this repository is now holding up nothing.

```bash
derrick prune s3://my-registry-bucket/dist/perception --endpoint ...      # reports
derrick prune s3://my-registry-bucket/dist/perception --endpoint ... --apply
```

It walks out from every tag — through indexes to their architectures, through
manifests to their blobs, and through referrers to the signatures hanging off
what it reached — and deletes what it never arrived at. It reports without
`--apply`, because it is the one command here that can lose an image.

Three of its rules are the reason it is a command rather than a shell loop:

**Unreferenced is not the same as garbage.** A publish writes blobs, then
manifests, then the tag; halfway through, an image arriving looks exactly like
one abandoned. So an object is deleted only once it has been unreferenced for
longer than a publish plausibly takes — `--older-than`, a day by default.

**A repository with no tags is left alone.** Everything in it is unreachable by
definition, and a repository whose images are pulled by digest is a thing
people have on purpose. Deleting a repository is not something to arrive at by
inference.

**A manifest that is referenced but missing stops the run.** Its blobs look
like garbage from the outside, and deleting them would turn a half-finished
publish into a lost image. Fix it — `derrick verify` says what is missing — and
prune afterwards.

Keys the layout does not describe are never deleted. Something else wrote them,
and this does not know what for.

`mirror --prune` is the first of these two, applied to what the source no
longer has: a tag in the bucket that the source registry does not list is
removed. It compares against everything the source listed rather than against
what the run copied, because an attachment tag was published by the copier
without being walked, and an excluded tag is one you chose not to publish
rather than one to take away. A source repository that lists no tags at all
prunes nothing — a registry answering with nothing looks exactly like a
repository that is empty, and one of those is a reason to delete every tag you
have.

## The bucket layout

```
<repository>/blobs/sha256:<hex>                  a layer or a config, stored raw
<repository>/manifests/sha256:<hex>              a manifest or an index, as JSON
<repository>/manifests/<tag>                     a copy of the manifest a tag points at
<repository>/_referrers/<subject>/<digest>       a referrer descriptor, as JSON
```

Two details are easy to get wrong and expensive to debug:

**A manifest stored under a tag carries its digest in `x-amz-meta-digest`.**
The registry resolves a digest from the object key, which works for everything
content-addressed. A tagged manifest is the one object whose key does not carry
one. Without the metadata the registry serves `Docker-Content-Digest: sha256:`
with nothing after it, and the pull fails on an unparseable digest with nothing
pointing at the cause.

**Do not supply an S3 checksum instead.** R2's SHA-256 for a multipart upload
is a hash over the part hashes rather than over the object, and any S3 client
goes multipart well below the size of a real layer, so the stored value would
be a plausible-looking digest that is not the image's.

Manifests also need their `Content-Type` set to their own `mediaType`: the
registry returns it verbatim and a client uses it to decide how to parse the
body.

## Status

The layout, ordering and referrer rules are covered by unit tests, and the read
side runs end to end in `go test` against a registry started in-process:
images, multi-architecture indexes, republishing, and moving a tag. The mirror
runs there too — cataloguing a prefix, skipping what the bucket already has —
and its skip, adopt, moved-tag and keep-going-after-a-failure rules are unit
tested against a stand-in registry. The prune rules — shared layers, signatures,
the grace period, the refusals — are tested against a bucket written by hand,
so that what is exercised is the keys and the metadata rather than an agreement
between the pruner and the copier.

The whole path has been run once for real: a signed multi-architecture image
copied from `cgr.dev` into a Cloudflare R2 bucket, then pulled back out through
a deployed serverless-registry and checked with `validate.Index`, which
verifies every manifest, layer and diffID. The cosign signature and attestation
came across with it and resolve by their fallback tags.

Still unexercised: **layers large enough to go multipart**. The largest blob in
that test was 600 KB, well under the uploader's part size, so the multipart
path has not actually run against R2. That is the next thing to try, and it is
the one most likely to surface a checksum-header problem.
