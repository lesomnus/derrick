// Command derrick copies container images from a registry into an
// S3-compatible bucket laid out for serverless-registry to serve.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"syscall"

	"github.com/lesomnus/derrick/internal/blobstore"
	"github.com/lesomnus/derrick/internal/copier"
	"github.com/lesomnus/derrick/internal/layout"
	"github.com/lesomnus/derrick/internal/mirror"
	"github.com/lesomnus/derrick/internal/pruner"
	"github.com/lesomnus/derrick/internal/source"
	"github.com/lesomnus/derrick/internal/target"
	"github.com/lesomnus/derrick/internal/verify"
)

const usage = `derrick copies container images into a serverless-registry bucket.

usage:
  derrick copy [flags] <source-image> s3://<bucket>/<repository>:<tag>
  derrick mirror [flags] <source-registry>[/<prefix>] s3://<bucket>
  derrick verify [flags] s3://<bucket>/<repository>:<tag>
  derrick untag [flags] s3://<bucket>/<repository>:<tag>
  derrick prune [flags] s3://<bucket>/<repository>
  derrick version

Signatures, attestations and SBOMs attached to the image are copied with it,
found through the referrers API and through the tags cosign falls back to.

A mirror copies every tag of every repository under a prefix, carrying the
repository name across unchanged, and copies only what the bucket does not
already have at the digest the source has it at.

Credentials come from the environment: AWS_ACCESS_KEY_ID and
AWS_SECRET_ACCESS_KEY for the bucket, and the ambient docker login for the
source registry.

examples:
  derrick copy registry.internal/perception:1.4.2 \
    s3://registry-hday-io/robot/perception:1.4.2 \
    --endpoint https://<account>.r2.cloudflarestorage.com

  derrick mirror cr.hday.io/dist s3://registry-hday-io \
    --endpoint https://<account>.r2.cloudflarestorage.com

  derrick verify s3://registry-hday-io/robot/perception:1.4.2 \
    --endpoint https://<account>.r2.cloudflarestorage.com
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, os.Args[1:]); err != nil {
		if errors.Is(err, context.Canceled) {
			fmt.Fprintln(os.Stderr, "derrick: interrupted")
			os.Exit(130)
		}
		fmt.Fprintf(os.Stderr, "derrick: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		fmt.Print(usage)

		return errors.New("no command given")
	}

	switch args[0] {
	case "copy":
		return runCopy(ctx, args[1:])
	case "mirror":
		return runMirror(ctx, args[1:])
	case "verify":
		return runVerify(ctx, args[1:])
	case "untag":
		return runUntag(ctx, args[1:])
	case "prune":
		return runPrune(ctx, args[1:])
	case "version":
		fmt.Println(version())

		return nil
	case "help", "-h", "--help":
		fmt.Print(usage)

		return nil
	default:
		fmt.Print(usage)

		return fmt.Errorf("unknown command %q", args[0])
	}
}

// reorder moves flags ahead of positional arguments.
//
// Go's flag package stops parsing at the first non-flag argument, so
// `derrick copy src dst --endpoint …` would silently ignore the endpoint. That
// is the order everyone writes a copy in, so accept it: pull the flags out,
// taking the value with each one that has a separate value, and hand the rest
// back as positionals.
func reorder(fs *flag.FlagSet, args []string) []string {
	var flags, positional []string

	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			positional = append(positional, args[i+1:]...)

			break
		}
		if len(arg) < 2 || arg[0] != '-' {
			positional = append(positional, arg)

			continue
		}

		name, value, hasValue := strings.Cut(strings.TrimLeft(arg, "-"), "=")
		flags = append(flags, arg)
		if hasValue {
			_ = value

			continue
		}

		// A flag that takes a value and did not carry one inline consumes the
		// next argument. Booleans do not, which is what IsBoolFlag reports.
		found := fs.Lookup(name)
		if found == nil {
			continue
		}
		if boolFlag, ok := found.Value.(interface{ IsBoolFlag() bool }); ok && boolFlag.IsBoolFlag() {
			continue
		}
		if i+1 < len(args) {
			i++
			flags = append(flags, args[i])
		}
	}

	return append(flags, positional...)
}

type storeFlags struct {
	endpoint    string
	region      string
	concurrency int
}

func (f *storeFlags) bind(fs *flag.FlagSet) {
	fs.StringVar(&f.endpoint, "endpoint", "", "S3 endpoint of the bucket, for example https://<account>.r2.cloudflarestorage.com")
	fs.StringVar(&f.region, "region", "auto", "S3 region")
	fs.IntVar(&f.concurrency, "concurrency", 4, "how many objects to transfer at once")
}

func (f *storeFlags) open(ctx context.Context, bucket string) (blobstore.Bucket, error) {
	return blobstore.NewS3(ctx, blobstore.S3Config{
		Bucket:      bucket,
		Endpoint:    f.endpoint,
		Region:      f.region,
		Concurrency: f.concurrency,
	})
}

func runCopy(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("copy", flag.ContinueOnError)
	var (
		store       storeFlags
		dryRun      = fs.Bool("dry-run", false, "report what would be written without writing it")
		noReferrers = fs.Bool("no-referrers", false, "skip signatures, attestations and SBOMs")
		doVerify    = fs.Bool("verify", false, "re-read every object after writing it")
		cosignTags  = fs.String("cosign-tags", copier.CosignTagsRoot, `how hard to look for cosign's fallback tags: "root", "all" or "none"`)
		insecure    = fs.Bool("src-insecure", false, "allow a plain-http source registry")
		quiet       = fs.Bool("quiet", false, "only report the outcome")
	)
	store.bind(fs)
	fs.Usage = func() { fmt.Print(usage); fs.PrintDefaults() }

	if err := fs.Parse(reorder(fs, args)); err != nil {
		return err
	}
	if fs.NArg() != 2 {
		fs.Usage()

		return errors.New("copy takes a source image and a destination")
	}

	dst, err := target.Parse(fs.Arg(1))
	if err != nil {
		return err
	}

	log := logger(*quiet)

	src, root, err := source.Open(ctx, fs.Arg(0), source.Options{Insecure: *insecure})
	if err != nil {
		return err
	}
	log("resolved %s to %s", fs.Arg(0), root.Digest)

	st, err := store.open(ctx, dst.Bucket)
	if err != nil {
		return err
	}

	c := &copier.Copier{
		Source:      src,
		Store:       st,
		Repository:  dst.Repository,
		Concurrency: store.concurrency,
		DryRun:      *dryRun,
		NoReferrers: *noReferrers,
		CosignTags:  *cosignTags,
		Verify:      *doVerify,
		Log:         log,
	}

	result, err := c.Run(ctx, root, dst.Tag)
	if err != nil {
		return err
	}

	verb := "published"
	if *dryRun {
		verb = "would publish"
	}
	fmt.Printf("%s %s at %s\n", verb, dst, root.Digest)
	fmt.Printf("  blobs      %d uploaded, %d already present (%s)\n",
		result.BlobsUploaded, result.BlobsSkipped, copier.HumanBytes(result.BytesUploaded))
	fmt.Printf("  manifests  %d written, %d already present\n", result.ManifestsWritten, result.ManifestsSkipped)
	fmt.Printf("  referrers  %d\n", result.ReferrersWritten)
	fmt.Printf("  tags       %d\n", result.TagsWritten)

	return nil
}

func runVerify(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	var store storeFlags
	store.bind(fs)
	fs.Usage = func() { fmt.Print(usage); fs.PrintDefaults() }

	if err := fs.Parse(reorder(fs, args)); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		fs.Usage()

		return errors.New("verify takes a destination")
	}

	ref, err := target.Parse(fs.Arg(0))
	if err != nil {
		return err
	}

	st, err := store.open(ctx, ref.Bucket)
	if err != nil {
		return err
	}

	v := &verify.Verifier{
		Store:      st,
		Repository: ref.Repository,
	}

	report, err := v.Tag(ctx, ref.Tag)
	if err != nil {
		return err
	}

	if !report.OK() {
		fmt.Printf("%s is not servable\n", ref)
		for _, problem := range report.Problems {
			fmt.Printf("  - %s\n", problem)
		}

		return fmt.Errorf("%d problems", len(report.Problems))
	}

	fmt.Printf("%s is complete at %s\n", ref, report.Digest)
	fmt.Printf("  %d manifests, %d blobs, %d referrers\n", report.Manifests, report.Blobs, report.Referrers)

	return nil
}

// stringList collects a flag that may be given more than once.
type stringList []string

func (l *stringList) String() string {
	return strings.Join(*l, ",")
}

func (l *stringList) Set(value string) error {
	*l = append(*l, value)

	return nil
}

func runMirror(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("mirror", flag.ContinueOnError)
	var (
		store        storeFlags
		excludeRepos stringList
		excludeTags  stringList
		parallel     = fs.Int("parallel", 4, "how many tags of one repository to examine at once")
		prune        = fs.Bool("prune", false, "remove destination tags the source no longer has")
		dryRun       = fs.Bool("dry-run", false, "report what would be copied without writing it")
		noReferrers  = fs.Bool("no-referrers", false, "skip signatures, attestations and SBOMs")
		doVerify     = fs.Bool("verify", false, "re-read every object after writing it")
		cosignTags   = fs.String("cosign-tags", copier.CosignTagsRoot, `how hard to look for cosign's fallback tags: "root", "all" or "none"`)
		insecure     = fs.Bool("src-insecure", false, "allow a plain-http source registry")
		quiet        = fs.Bool("quiet", false, "only report the outcome")
	)
	fs.Var(&excludeRepos, "exclude-repository", "repository pattern not to mirror; may be given more than once")
	fs.Var(&excludeTags, "exclude-tag", "tag pattern not to mirror; may be given more than once")
	store.bind(fs)
	fs.Usage = func() { fmt.Print(usage); fs.PrintDefaults() }

	if err := fs.Parse(reorder(fs, args)); err != nil {
		return err
	}
	if fs.NArg() != 2 {
		fs.Usage()

		return errors.New("mirror takes a source registry and a destination bucket")
	}

	registry, prefix, err := parseMirrorSource(fs.Arg(0))
	if err != nil {
		return err
	}

	bucket, err := parseBucket(fs.Arg(1))
	if err != nil {
		return err
	}

	log := logger(*quiet)

	reg, err := source.OpenRegistry(registry, source.Options{Insecure: *insecure})
	if err != nil {
		return err
	}

	st, err := store.open(ctx, bucket)
	if err != nil {
		return err
	}

	m := &mirror.Mirror{
		Registry:            mirror.FromSource(reg),
		Store:               st,
		Bucket:              bucket,
		Prefix:              prefix,
		Prune:               *prune,
		ExcludeRepositories: excludeRepos,
		ExcludeTags:         excludeTags,
		Parallel:            *parallel,
		Concurrency:         store.concurrency,
		DryRun:              *dryRun,
		NoReferrers:         *noReferrers,
		CosignTags:          *cosignTags,
		Verify:              *doVerify,
		Log:                 log,
	}

	result, runErr := m.Run(ctx)
	if result == nil {
		return runErr
	}

	verb := "mirrored"
	if *dryRun {
		verb = "would mirror"
	}
	fmt.Printf("%s %s into %s%s\n", verb, fs.Arg(0), target.Scheme, bucket)
	fmt.Printf("  repositories  %d\n", result.Repositories)
	fmt.Printf("  tags          %d\n", result.Tags)
	fmt.Printf("  copied        %d (%d blobs, %s)\n", result.Copied, result.BlobsUploaded, copier.HumanBytes(result.BytesUploaded))
	fmt.Printf("  skipped       %d\n", result.Skipped)
	fmt.Printf("  excluded      %d\n", result.Excluded)
	if *prune {
		if *dryRun {
			fmt.Printf("  would prune   %d tags no longer in the source\n", result.Pruned)
		} else {
			fmt.Printf("  pruned        %d tags no longer in the source\n", result.Pruned)
		}
	}
	if result.Vanished > 0 {
		fmt.Printf("  vanished      %d\n", result.Vanished)
	}
	fmt.Printf("  failed        %d\n", len(result.Failures))

	for _, failure := range result.Failures {
		fmt.Fprintf(os.Stderr, "  - %s\n", failure)
	}

	if runErr != nil {
		return runErr
	}
	if len(result.Failures) > 0 {
		// The mirror is further along than it was, and saying so with a zero
		// exit would make a permanently broken image invisible.
		return fmt.Errorf("%d of %d tags could not be mirrored", len(result.Failures), result.Tags)
	}

	return nil
}

func runUntag(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("untag", flag.ContinueOnError)
	var store storeFlags
	store.bind(fs)
	fs.Usage = func() { fmt.Print(usage); fs.PrintDefaults() }

	if err := fs.Parse(reorder(fs, args)); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		fs.Usage()

		return errors.New("untag takes a reference")
	}

	ref, err := target.Parse(fs.Arg(0))
	if err != nil {
		return err
	}

	st, err := store.open(ctx, ref.Bucket)
	if err != nil {
		return err
	}

	key := layout.ManifestKey(ref.Repository, ref.Tag)

	obj, err := st.Stat(ctx, key)
	if err != nil {
		return err
	}
	if obj == nil {
		return fmt.Errorf("%s is not tagged in this bucket", ref)
	}
	was := obj.Metadata[layout.DigestMetadataKey]

	if err := st.Delete(ctx, key); err != nil {
		return err
	}

	fmt.Printf("untagged %s\n", ref)
	if was != "" {
		fmt.Printf("  it pointed at %s, which is still pullable by digest\n", was)
	}
	fmt.Printf("  the manifests and blobs under it are still there; `derrick prune s3://%s/%s` is what reclaims them\n",
		ref.Bucket, ref.Repository)

	return nil
}

func runPrune(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("prune", flag.ContinueOnError)
	var (
		store storeFlags
		grace = fs.Duration("older-than", pruner.DefaultGrace, "how long an object must have been unreferenced before it is deleted")
		apply = fs.Bool("apply", false, "actually delete; without it nothing is written")
		quiet = fs.Bool("quiet", false, "only report the outcome")
	)
	store.bind(fs)
	fs.Usage = func() { fmt.Print(usage); fs.PrintDefaults() }

	if err := fs.Parse(reorder(fs, args)); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		fs.Usage()

		return errors.New("prune takes a repository")
	}

	bucket, repository, err := parseRepository(fs.Arg(0))
	if err != nil {
		return err
	}

	st, err := store.open(ctx, bucket)
	if err != nil {
		return err
	}

	p := &pruner.Pruner{
		Store:      st,
		Repository: repository,
		Grace:      *grace,
		Apply:      *apply,
		Log:        logger(*quiet),
	}

	result, err := p.Run(ctx)
	if err != nil {
		return err
	}

	verb := "would delete"
	if *apply {
		verb = "deleted"
	}

	fmt.Printf("%s%s/%s\n", target.Scheme, bucket, repository)
	fmt.Printf("  reachable     %d manifests, %d blobs, %d referrers from %d tags\n",
		result.Reachable.Manifests, result.Reachable.Blobs, result.Reachable.Referrers, result.Tags)
	fmt.Printf("  %-13s %d manifests, %d blobs, %d referrers (%s)\n", verb,
		result.Deleted.Manifests, result.Deleted.Blobs, result.Deleted.Referrers, copier.HumanBytes(result.BytesFreed))
	if result.Withheld.Total() > 0 {
		fmt.Printf("  held back     %d objects, unreferenced but younger than %s\n", result.Withheld.Total(), *grace)
	}
	if len(result.Unknown) > 0 {
		fmt.Printf("  left alone    %d keys this layout does not describe\n", len(result.Unknown))
	}
	if !*apply && result.Deleted.Total() > 0 {
		fmt.Println("\nNothing was written. Pass --apply to delete.")
	}

	return nil
}

// parseRepository reads a destination that names a repository and no tag.
func parseRepository(s string) (string, string, error) {
	rest, ok := strings.CutPrefix(s, target.Scheme)
	if !ok {
		return "", "", fmt.Errorf("target %q must begin with %s", s, target.Scheme)
	}

	bucket, repository, _ := strings.Cut(rest, "/")
	repository = strings.Trim(repository, "/")
	if bucket == "" || repository == "" {
		return "", "", fmt.Errorf("target %q must name a bucket and a repository, as %s<bucket>/<repository>", s, target.Scheme)
	}
	if strings.Contains(repository, ":") {
		return "", "", fmt.Errorf("target %q names a tag; prune reclaims what a whole repository no longer points at, and `derrick untag` is what removes a tag", s)
	}

	return bucket, repository, nil
}

// parseMirrorSource splits a mirror source into the registry it names and the
// prefix inside it, which may be empty.
func parseMirrorSource(s string) (string, string, error) {
	if strings.Contains(s, "://") {
		return "", "", fmt.Errorf("source %q must be a registry host, without a scheme", s)
	}

	registry, prefix, _ := strings.Cut(strings.Trim(s, "/"), "/")
	if registry == "" {
		return "", "", fmt.Errorf("source %q must name a registry, as <registry>[/<prefix>]", s)
	}
	if strings.Contains(prefix, ":") {
		// A colon in the registry is a port, which is fine. A colon anywhere
		// after it is a tag, which means a whole image reference was given
		// where a prefix belongs — and a mirror copies every tag it finds, so
		// naming one is a misunderstanding rather than a narrowing.
		return "", "", fmt.Errorf("source %q names a tag; a mirror takes a registry and a repository prefix, and copies every tag under it", s)
	}

	return registry, prefix, nil
}

// parseBucket reads a destination that is a whole bucket rather than one
// reference in it.
func parseBucket(s string) (string, error) {
	rest, ok := strings.CutPrefix(s, target.Scheme)
	if !ok {
		return "", fmt.Errorf("destination %q must begin with %s", s, target.Scheme)
	}

	bucket := strings.TrimSuffix(rest, "/")
	if bucket == "" || strings.Contains(bucket, "/") {
		return "", fmt.Errorf("destination %q must name a bucket and nothing else, as %s<bucket>: a mirror carries the repository name across unchanged, so there is nothing else to say", s, target.Scheme)
	}

	return bucket, nil
}

func logger(quiet bool) func(string, ...any) {
	if quiet {
		return func(string, ...any) {}
	}

	return func(format string, args ...any) {
		fmt.Fprintf(os.Stderr, format+"\n", args...)
	}
}

func version() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	if info.Main.Version != "" {
		return info.Main.Version
	}

	return "devel"
}
