package storage

import (
	"context"
	"fmt"
	"io"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// RustFSClient wraps the S3-compatible RustFS object store.
type RustFSClient struct {
	client *s3.Client
	bucket string
}

// NewRustFSClient builds an S3 client pointed at a RustFS endpoint. RustFS
// requires path-style addressing (no virtual-hosted bucket subdomains).
func NewRustFSClient(endpoint, bucket, accessKey, secretKey string) (*RustFSClient, error) {
	cfg, err := config.LoadDefaultConfig(context.TODO(),
		config.WithRegion("us-east-1"),
		config.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(accessKey, secretKey, ""),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("load s3 config: %w", err)
	}

	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(endpoint)
		o.UsePathStyle = true
	})

	return &RustFSClient{client: client, bucket: bucket}, nil
}

// PutObject streams an object into the bucket.
func (c *RustFSClient) PutObject(ctx context.Context, key string, reader io.Reader, size int64, mime string) error {
	input := &s3.PutObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(key),
		Body:   reader,
	}
	if size > 0 {
		input.ContentLength = aws.Int64(size)
	}
	if mime != "" {
		input.ContentType = aws.String(mime)
	}
	if _, err := c.client.PutObject(ctx, input); err != nil {
		return fmt.Errorf("put object %s: %w", key, err)
	}
	return nil
}

// GetObject opens a read stream for an object. Caller must Close it.
func (c *RustFSClient) GetObject(ctx context.Context, key string) (io.ReadCloser, error) {
	out, err := c.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, fmt.Errorf("get object %s: %w", key, err)
	}
	return out.Body, nil
}
