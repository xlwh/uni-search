package objstore

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// MinioS3Client implements S3Client using the minio-go SDK.
// @spec: ./archive/2026-10-20-m2-cloud-native-ga/02_spec.md#2.2
type MinioS3Client struct {
	client   *minio.Client
	bucket   string
}

// NewMinioS3Client creates a real S3-compatible client using minio-go.
func NewMinioS3Client(cfg S3Config) (*MinioS3Client, error) {
	client, err := minio.New(cfg.Endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Secure: cfg.UseSSL,
		Region: cfg.Region,
	})
	if err != nil {
		return nil, fmt.Errorf("objstore: create minio client: %w", err)
	}

	ctx := context.Background()
	// Ensure bucket exists.
	exists, err := client.BucketExists(ctx, cfg.Bucket)
	if err != nil {
		return nil, fmt.Errorf("objstore: check bucket: %w", err)
	}
	if !exists {
		if err := client.MakeBucket(ctx, cfg.Bucket, minio.MakeBucketOptions{}); err != nil {
			return nil, fmt.Errorf("objstore: create bucket: %w", err)
		}
		log.Printf("objstore: created bucket %s", cfg.Bucket)
	}

	return &MinioS3Client{client: client, bucket: cfg.Bucket}, nil
}

func (c *MinioS3Client) PutObject(ctx context.Context, key string, data []byte) error {
	_, err := c.client.PutObject(ctx, c.bucket, key, bytes.NewReader(data), int64(len(data)), minio.PutObjectOptions{})
	return err
}

func (c *MinioS3Client) GetObject(ctx context.Context, key string) (io.ReadCloser, error) {
	obj, err := c.client.GetObject(ctx, c.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, err
	}
	// Verify the object exists by statting.
	if _, err := obj.Stat(); err != nil {
		obj.Close()
		return nil, fmt.Errorf("objstore: object %q not found: %w", key, err)
	}
	return obj, nil
}

func (c *MinioS3Client) ListObjects(ctx context.Context, prefix string) ([]string, error) {
	var keys []string
	objectCh := c.client.ListObjects(ctx, c.bucket, minio.ListObjectsOptions{Prefix: prefix, Recursive: true})
	for obj := range objectCh {
		if obj.Err != nil {
			return nil, obj.Err
		}
		keys = append(keys, obj.Key)
	}
	return keys, nil
}

func (c *MinioS3Client) DeleteObject(ctx context.Context, key string) error {
	return c.client.RemoveObject(ctx, c.bucket, key, minio.RemoveObjectOptions{})
}

func (c *MinioS3Client) ObjectExists(ctx context.Context, key string) (bool, error) {
	_, err := c.client.StatObject(ctx, c.bucket, key, minio.StatObjectOptions{})
	if err != nil {
		errResp := minio.ToErrorResponse(err)
		if errResp.Code == "NoSuchKey" {
			return false, nil
		}
		return false, err
	}
	return true, nil
}
