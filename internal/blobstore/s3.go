package blobstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	smithyhttp "github.com/aws/smithy-go/transport/http"
)

// S3Config describes how to reach an S3-compatible store.
type S3Config struct {
	Bucket string

	// Endpoint is the service endpoint, for example
	// https://<account>.r2.cloudflarestorage.com. Empty means AWS S3 proper.
	Endpoint string

	// Region is "auto" for stores that have one region and do not care.
	Region string

	// PartSize is the multipart chunk size in bytes; zero takes the SDK
	// default.
	PartSize int64

	// Concurrency is how many parts of a single object upload at once.
	Concurrency int
}

// S3 is a Store backed by an S3-compatible service.
type S3 struct {
	client   *s3.Client
	uploader *manager.Uploader
	bucket   string
}

// NewS3 builds a Store from the ambient AWS credentials and cfg.
func NewS3(ctx context.Context, cfg S3Config) (*S3, error) {
	if cfg.Bucket == "" {
		return nil, errors.New("bucket is required")
	}
	region := cfg.Region
	if region == "" {
		region = "auto"
	}

	// R2 and most other S3-compatible stores do not implement the
	// x-amz-checksum-* headers that recent SDKs add to every upload by
	// default, and refuse the request when they cannot. Transfers are still
	// integrity-checked, by Content-MD5 and by TLS.
	loaded, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion(region),
		awsconfig.WithRequestChecksumCalculation(aws.RequestChecksumCalculationWhenRequired),
		awsconfig.WithResponseChecksumValidation(aws.ResponseChecksumValidationWhenRequired),
	)
	if err != nil {
		return nil, fmt.Errorf("load aws config: %w", err)
	}

	client := s3.NewFromConfig(loaded, func(o *s3.Options) {
		if cfg.Endpoint != "" {
			o.BaseEndpoint = aws.String(cfg.Endpoint)
		}
		// A custom endpoint plus virtual-host addressing needs DNS for every
		// bucket name; path style needs nothing and every S3-compatible store
		// supports it.
		o.UsePathStyle = cfg.Endpoint != ""
	})

	uploader := manager.NewUploader(client, func(u *manager.Uploader) {
		if cfg.PartSize > 0 {
			u.PartSize = cfg.PartSize
		}
		if cfg.Concurrency > 0 {
			u.Concurrency = cfg.Concurrency
		}
	})

	return &S3{client: client, uploader: uploader, bucket: cfg.Bucket}, nil
}

func (s *S3) Stat(ctx context.Context, key string) (*Object, error) {
	out, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		if isNotFound(err) {
			return nil, nil
		}

		return nil, fmt.Errorf("head %q: %w", key, err)
	}

	obj := &Object{Metadata: out.Metadata}
	if out.ContentLength != nil {
		obj.Size = *out.ContentLength
	}
	if out.ContentType != nil {
		obj.ContentType = *out.ContentType
	}

	return obj, nil
}

func (s *S3) Put(ctx context.Context, key string, body io.Reader, opts PutOptions) error {
	in := &s3.PutObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
		Body:   body,
	}
	if opts.ContentType != "" {
		in.ContentType = aws.String(opts.ContentType)
	}
	if len(opts.Metadata) > 0 {
		in.Metadata = opts.Metadata
	}

	// The uploader streams, and switches to multipart on its own once the body
	// outgrows a part. Layers are routinely larger than any single-request
	// limit, so this is the normal path rather than the exceptional one.
	if _, err := s.uploader.Upload(ctx, in); err != nil {
		return fmt.Errorf("put %q: %w", key, err)
	}

	return nil
}

func (s *S3) List(ctx context.Context, prefix string, fn func(Entry) error) error {
	paginator := s3.NewListObjectsV2Paginator(s.client, &s3.ListObjectsV2Input{
		Bucket: aws.String(s.bucket),
		Prefix: aws.String(prefix),
	})

	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return fmt.Errorf("list %q: %w", prefix, err)
		}

		for _, object := range page.Contents {
			entry := Entry{Key: aws.ToString(object.Key)}
			if object.Size != nil {
				entry.Size = *object.Size
			}
			if object.LastModified != nil {
				entry.Modified = *object.LastModified
			}

			if err := fn(entry); err != nil {
				return err
			}
		}
	}

	return nil
}

func (s *S3) Delete(ctx context.Context, key string) error {
	if _, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	}); err != nil {
		if isNotFound(err) {
			return nil
		}

		return fmt.Errorf("delete %q: %w", key, err)
	}

	return nil
}

func (s *S3) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, fmt.Errorf("get %q: %w", key, err)
	}

	return out.Body, nil
}

func isNotFound(err error) bool {
	var notFound *types.NotFound
	if errors.As(err, &notFound) {
		return true
	}

	var noSuchKey *types.NoSuchKey
	if errors.As(err, &noSuchKey) {
		return true
	}

	// A HEAD carries no body for the SDK to model, so some stores surface a
	// missing key as a bare 404.
	var response *smithyhttp.ResponseError
	if errors.As(err, &response) && response.HTTPStatusCode() == http.StatusNotFound {
		return true
	}

	return false
}
