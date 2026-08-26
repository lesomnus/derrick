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
derrick verify [flags] s3://<bucket>/<repository>:<tag>
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
images, multi-architecture indexes, republishing, and moving a tag.

The whole path has been run once for real: a signed multi-architecture image
copied from `cgr.dev` into a Cloudflare R2 bucket, then pulled back out through
a deployed serverless-registry and checked with `validate.Index`, which
verifies every manifest, layer and diffID. The cosign signature and attestation
came across with it and resolve by their fallback tags.

Still unexercised: **layers large enough to go multipart**. The largest blob in
that test was 600 KB, well under the uploader's part size, so the multipart
path has not actually run against R2. That is the next thing to try, and it is
the one most likely to surface a checksum-header problem.
