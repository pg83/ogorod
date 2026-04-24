package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
)

// S3Client wraps MinIO object operations scoped to one repository
// under <bucket>/<repo>/objects/. Keys are flat — no two-byte
// directory sharding like git's loose-object layout, that's a
// filesystem-inode concern and S3 doesn't care.
type S3Client struct {
	cli    *s3.Client
	bucket string
	prefix string
}

func newS3Client(env Env, repo string) *S3Client {
	cfg := Throw2(config.LoadDefaultConfig(context.Background(),
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(
			env.S3AccessKey, env.S3SecretKey, "",
		)),
		// MinIO ignores region but aws-sdk-go-v2 requires one be set.
		config.WithRegion("us-east-1"),
	))

	cli := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(env.S3Endpoint)
		o.UsePathStyle = true
	})

	return &S3Client{
		cli:    cli,
		bucket: env.S3Bucket,
		prefix: repo + "/objects/",
	}
}

func (s *S3Client) key(sha string) string {
	return s.prefix + sha
}

// Get returns the blob's raw bytes and true if present, (nil, false)
// if the object doesn't exist. Any other error throws.
func (s *S3Client) Get(ctx context.Context, sha string) ([]byte, bool) {
	out, err := s.cli.GetObject(ctx, &s3.GetObjectInput{
		Bucket: &s.bucket,
		Key:    aws.String(s.key(sha)),
	})

	if err != nil {
		if isS3NotFound(err) {
			return nil, false
		}

		Throw(err)
	}

	defer out.Body.Close()
	body := Throw2(io.ReadAll(out.Body))

	return body, true
}

// Put writes a blob, overwriting any existing object at the same key.
// Safe for loose-object uploads since git objects are content-addressed
// — the same sha always round-trips the same bytes.
func (s *S3Client) Put(ctx context.Context, sha string, data []byte) {
	Throw2(s.cli.PutObject(ctx, &s3.PutObjectInput{
		Bucket: &s.bucket,
		Key:    aws.String(s.key(sha)),
		Body:   bytes.NewReader(data),
	}))
}

// Has is a cheap HEAD — true if the object exists.
func (s *S3Client) Has(ctx context.Context, sha string) bool {
	_, err := s.cli.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: &s.bucket,
		Key:    aws.String(s.key(sha)),
	})

	if err == nil {
		return true
	}

	if isS3NotFound(err) {
		return false
	}

	Throw(err)

	return false
}

// Delete removes one blob; used by GC. No-op if already absent.
func (s *S3Client) Delete(ctx context.Context, sha string) {
	_, err := s.cli.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: &s.bucket,
		Key:    aws.String(s.key(sha)),
	})

	if err != nil && !isS3NotFound(err) {
		Throw(err)
	}
}

// ListAll returns every sha currently stored under this repo's
// prefix. Used by GC to diff against the reachable set.
func (s *S3Client) ListAll(ctx context.Context) []string {
	pager := s3.NewListObjectsV2Paginator(s.cli, &s3.ListObjectsV2Input{
		Bucket: &s.bucket,
		Prefix: &s.prefix,
	})

	var out []string

	for pager.HasMorePages() {
		page := Throw2(pager.NextPage(ctx))

		for _, obj := range page.Contents {
			if obj.Key == nil {
				continue
			}

			sha := strings.TrimPrefix(*obj.Key, s.prefix)
			out = append(out, sha)
		}
	}

	return out
}

func isS3NotFound(err error) bool {
	var apiErr smithy.APIError

	if errors.As(err, &apiErr) {
		code := apiErr.ErrorCode()

		return code == "NoSuchKey" || code == "NotFound"
	}

	return false
}
