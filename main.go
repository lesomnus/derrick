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
	"time"

	"github.com/lesomnus/derrick/internal/blobstore"
	"github.com/lesomnus/derrick/internal/copier"
	"github.com/lesomnus/derrick/internal/ledger"
	"github.com/lesomnus/derrick/internal/mirror"
	"github.com/lesomnus/derrick/internal/source"
	"github.com/lesomnus/derrick/internal/target"
	"github.com/lesomnus/derrick/internal/verify"
)

const usage = `derrick copies container images into a serverless-registry bucket.

usage:
  derrick copy [flags] <source-image> s3://<bucket>/<repository>:<tag>
  derrick mirror [flags] <source-registry>[/<prefix>] s3://<bucket>
  derrick verify [flags] s3://<bucket>/<repository>:<tag>
  derrick ledger [flags] s3://<bucket>
  derrick version

Signatures, attestations and SBOMs attached to the image are copied with it,
found through the referrers API and through the tags cosign falls back to.

A mirror copies every tag of every repository under a prefix, carrying the
repository name across unchanged, and records what it copied in a ledger in
the bucket so that a later run examines a known tag with one request instead
of walking the image behind it.

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
	case "ledger":
		return runLedger(ctx, args[1:])
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

func (f *storeFlags) open(ctx context.Context, bucket string) (blobstore.Store, error) {
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
		result.BlobsUploaded, result.BlobsSkipped, humanBytes(result.BytesUploaded))
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
		recheck      = fs.Bool("recheck", false, "confirm every recorded tag against the bucket instead of trusting the ledger")
		noLedger     = fs.Bool("no-ledger", false, "examine every tag against the bucket and record nothing")
		ledgerKey    = fs.String("ledger", ledger.DefaultKey, "key the ledger is stored under, in the destination bucket")
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

	var book *ledger.Ledger
	if !*noLedger {
		book, err = ledger.Load(ctx, st, *ledgerKey)
		if err != nil {
			return err
		}
		log("ledger holds %d tags", book.Len())
	}

	m := &mirror.Mirror{
		Registry:            mirror.FromSource(reg),
		Store:               st,
		Bucket:              bucket,
		Prefix:              prefix,
		Ledger:              book,
		Recheck:             *recheck,
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
	fmt.Printf("  copied        %d (%d blobs, %s)\n", result.Copied, result.BlobsUploaded, humanBytes(result.BytesUploaded))
	fmt.Printf("  skipped       %d\n", result.Skipped)
	fmt.Printf("  excluded      %d\n", result.Excluded)
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

func runLedger(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("ledger", flag.ContinueOnError)
	var (
		store     storeFlags
		ledgerKey = fs.String("ledger", ledger.DefaultKey, "key the ledger is stored under")
		summary   = fs.Bool("summary", false, "print a count per repository instead of the entries")
	)
	store.bind(fs)
	fs.Usage = func() { fmt.Print(usage); fs.PrintDefaults() }

	if err := fs.Parse(reorder(fs, args)); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		fs.Usage()

		return errors.New("ledger takes a bucket")
	}

	bucket, err := parseBucket(fs.Arg(0))
	if err != nil {
		return err
	}

	st, err := store.open(ctx, bucket)
	if err != nil {
		return err
	}

	book, err := ledger.Load(ctx, st, *ledgerKey)
	if err != nil {
		return err
	}

	if !*summary {
		// The entries are written out exactly as they are stored, so that
		// reading the ledger and downloading it are the same operation.
		_, err := book.WriteTo(os.Stdout)

		return err
	}

	entries := book.Entries()
	fmt.Printf("%s%s/%s holds %d tags\n", target.Scheme, bucket, *ledgerKey, len(entries))

	repository := ""
	count := 0
	newest := time.Time{}
	flush := func() {
		if repository == "" {
			return
		}
		fmt.Printf("  %-48s %5d  newest %s\n", repository, count, newest.Format(time.RFC3339))
	}
	for _, entry := range entries {
		if entry.Repository != repository {
			flush()
			repository, count, newest = entry.Repository, 0, time.Time{}
		}
		count++
		if entry.At.After(newest) {
			newest = entry.At
		}
	}
	flush()

	return nil
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

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}

	div, exp := int64(unit), 0
	for size := n / unit; size >= unit; size /= unit {
		div *= unit
		exp++
	}

	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
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
