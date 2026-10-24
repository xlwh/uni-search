// Package objstore implements the M2 object storage layer (design
// 05-distributed.md §2). It provides an S3-compatible client interface
// and a LogStore that double-writes WAL segments to local NVMe (sync)
// and S3 (async追赶). The S3 client uses the minio-go SDK; local
// testing uses MinIO.
// @spec: ./archive/2026-10-20-m2-cloud-native-ga/02_spec.md#2.2
package objstore

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sync"
)

// S3Client is the object storage interface. Implementations include
// the minio-go backed client and an in-memory mock for testing.
type S3Client interface {
	PutObject(ctx context.Context, key string, data []byte) error
	GetObject(ctx context.Context, key string) (io.ReadCloser, error)
	ListObjects(ctx context.Context, prefix string) ([]string, error)
	DeleteObject(ctx context.Context, key string) error
	ObjectExists(ctx context.Context, key string) (bool, error)
}

// S3Config holds the connection configuration.
type S3Config struct {
	Endpoint   string  // e.g. "localhost:9000" (MinIO) or "s3.amazonaws.com"
	AccessKey  string
	SecretKey  string
	Bucket     string
	UseSSL     bool
	Region     string
}

// MemS3Client is an in-memory S3 client for testing.
type MemS3Client struct {
	mu    sync.RWMutex
	store map[string][]byte
}

// NewMemS3Client creates an in-memory S3 client.
func NewMemS3Client() *MemS3Client {
	return &MemS3Client{store: make(map[string][]byte)}
}

func (c *MemS3Client) PutObject(_ context.Context, key string, data []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.store[key] = make([]byte, len(data))
	copy(c.store[key], data)
	return nil
}

func (c *MemS3Client) GetObject(_ context.Context, key string) (io.ReadCloser, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	data, ok := c.store[key]
	if !ok {
		return nil, fmt.Errorf("objstore: object %q not found", key)
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func (c *MemS3Client) ListObjects(_ context.Context, prefix string) ([]string, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	var keys []string
	for k := range c.store {
		if len(prefix) == 0 || len(k) >= len(prefix) && k[:len(prefix)] == prefix {
			keys = append(keys, k)
		}
	}
	return keys, nil
}

func (c *MemS3Client) DeleteObject(_ context.Context, key string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.store, key)
	return nil
}

func (c *MemS3Client) ObjectExists(_ context.Context, key string) (bool, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	_, ok := c.store[key]
	return ok, nil
}
