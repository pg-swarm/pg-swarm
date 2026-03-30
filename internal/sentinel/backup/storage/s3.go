package storage

import (
	"context"
	"fmt"
	"io"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	pgswarmv1 "github.com/pg-swarm/pg-swarm/api/gen/v1"
	"github.com/rs/zerolog"
)

// s3Backend implements Backend for AWS S3 and S3-compatible stores (e.g. MinIO).
type s3Backend struct {
	client     *s3.Client
	bucket     string
	pathPrefix string
	log        zerolog.Logger
}

func newS3(ctx context.Context, dest *pgswarmv1.S3Destination, log zerolog.Logger) (*s3Backend, error) {
	if dest.Bucket == "" {
		return nil, fmt.Errorf("s3 bucket is required")
	}
	if dest.AccessKeyId == "" || dest.SecretAccessKey == "" {
		return nil, fmt.Errorf("s3 access_key_id and secret_access_key are required")
	}

	creds := credentials.NewStaticCredentialsProvider(dest.AccessKeyId, dest.SecretAccessKey, "")

	opts := []func(*config.LoadOptions) error{
		config.WithCredentialsProvider(creds),
	}
	if dest.Region != "" {
		opts = append(opts, config.WithRegion(dest.Region))
	}

	cfg, err := config.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("load s3 config: %w", err)
	}

	s3Opts := []func(*s3.Options){}
	if dest.Endpoint != "" {
		s3Opts = append(s3Opts, func(o *s3.Options) {
			o.BaseEndpoint = aws.String(dest.Endpoint)
		})
	}
	if dest.ForcePathStyle {
		s3Opts = append(s3Opts, func(o *s3.Options) {
			o.UsePathStyle = true
		})
	}

	client := s3.NewFromConfig(cfg, s3Opts...)

	return &s3Backend{
		client:     client,
		bucket:     dest.Bucket,
		pathPrefix: dest.PathPrefix,
		log:        log.With().Str("backend", "s3").Str("bucket", dest.Bucket).Logger(),
	}, nil
}

func (b *s3Backend) fullKey(key string) string {
	if b.pathPrefix == "" {
		return key
	}
	return b.pathPrefix + "/" + key
}

func (b *s3Backend) Upload(ctx context.Context, key string, r io.Reader) error {
	_, err := b.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(b.bucket),
		Key:    aws.String(b.fullKey(key)),
		Body:   r,
	})
	if err != nil {
		return fmt.Errorf("s3 upload %s: %w", key, err)
	}
	b.log.Debug().Str("key", key).Msg("uploaded object")
	return nil
}

func (b *s3Backend) Download(ctx context.Context, key string) (io.ReadCloser, error) {
	out, err := b.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(b.bucket),
		Key:    aws.String(b.fullKey(key)),
	})
	if err != nil {
		return nil, fmt.Errorf("s3 download %s: %w", key, err)
	}
	return out.Body, nil
}

func (b *s3Backend) List(ctx context.Context, prefix string) ([]ObjectInfo, error) {
	var objects []ObjectInfo
	paginator := s3.NewListObjectsV2Paginator(b.client, &s3.ListObjectsV2Input{
		Bucket: aws.String(b.bucket),
		Prefix: aws.String(b.fullKey(prefix)),
	})
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("s3 list %s: %w", prefix, err)
		}
		for _, obj := range page.Contents {
			objects = append(objects, ObjectInfo{
				Key:          aws.ToString(obj.Key),
				Size:         aws.ToInt64(obj.Size),
				LastModified: aws.ToTime(obj.LastModified),
			})
		}
	}
	return objects, nil
}

func (b *s3Backend) Delete(ctx context.Context, key string) error {
	_, err := b.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(b.bucket),
		Key:    aws.String(b.fullKey(key)),
	})
	if err != nil {
		// S3 returns success even for non-existent keys, but handle errors from
		// S3-compatible stores that may behave differently.
		var notFound *types.NoSuchKey
		if ok := isS3NoSuchKey(err, notFound); ok {
			return nil
		}
		return fmt.Errorf("s3 delete %s: %w", key, err)
	}
	b.log.Debug().Str("key", key).Msg("deleted object")
	return nil
}

func (b *s3Backend) Stat(ctx context.Context, key string) (*ObjectInfo, error) {
	out, err := b.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(b.bucket),
		Key:    aws.String(b.fullKey(key)),
	})
	if err != nil {
		return nil, fmt.Errorf("s3 stat %s: %w", key, err)
	}
	return &ObjectInfo{
		Key:          b.fullKey(key),
		Size:         aws.ToInt64(out.ContentLength),
		LastModified: aws.ToTime(out.LastModified),
	}, nil
}

func (b *s3Backend) Close() error { return nil }

// isS3NoSuchKey checks if the error is a NoSuchKey S3 error.
func isS3NoSuchKey(err error, _ *types.NoSuchKey) bool {
	if err == nil {
		return false
	}
	_, ok := err.(*types.NoSuchKey)
	return ok
}
