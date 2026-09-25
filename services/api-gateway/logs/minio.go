// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package logs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// MinIOConfig is the gateway's READ access to the result store. The gateway
// needs nothing but GetObject on <namespace>/logs/*; OPERATIONS recommends a
// credential scoped to exactly that.
type MinIOConfig struct {
	Endpoint        string
	Bucket          string
	Region          string
	AccessKeyID     string
	SecretAccessKey string
	UseTLS          bool
}

// MinIOReader implements ObjectReader over any S3-compatible store.
type MinIOReader struct {
	mc     *minio.Client
	bucket string
}

// NewMinIOReader builds the reader. It does not probe the store: a gateway
// must start (and serve everything else) while the object store is down; log
// reads then fail individually and say so.
func NewMinIOReader(cfg MinIOConfig) (*MinIOReader, error) {
	if cfg.Endpoint == "" || cfg.Bucket == "" {
		return nil, errors.New("logs: result-store endpoint and bucket are required")
	}
	if cfg.AccessKeyID == "" || cfg.SecretAccessKey == "" {
		return nil, errors.New("logs: result-store credentials are required to read logs")
	}
	host := strings.TrimPrefix(strings.TrimPrefix(cfg.Endpoint, "https://"), "http://")
	mc, err := minio.New(host, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.AccessKeyID, cfg.SecretAccessKey, ""),
		Secure: cfg.UseTLS || strings.HasPrefix(cfg.Endpoint, "https://"),
		Region: cfg.Region,
	})
	if err != nil {
		return nil, fmt.Errorf("logs: result-store client: %w", err)
	}
	return &MinIOReader{mc: mc, bucket: cfg.Bucket}, nil
}

// maxObjectRead caps a whole-object read; chunks are far smaller (the gateway
// refuses to index chunks over 8 MiB), so hitting this means a key was
// overwritten out of band.
const maxObjectRead = 16 << 20

// Read implements ObjectReader.
func (m *MinIOReader) Read(ctx context.Context, key string, offset, length int64) ([]byte, error) {
	opts := minio.GetObjectOptions{}
	if length >= 0 {
		if length == 0 {
			return []byte{}, nil
		}
		if err := opts.SetRange(offset, offset+length-1); err != nil {
			return nil, err
		}
	}
	obj, err := m.mc.GetObject(ctx, m.bucket, key, opts)
	if err != nil {
		return nil, classify(err)
	}
	defer obj.Close()
	limit := int64(maxObjectRead)
	if length >= 0 {
		limit = length
	}
	b, err := io.ReadAll(io.LimitReader(obj, limit+1))
	if err != nil {
		return nil, classify(err)
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("logs: object %s larger than expected", key)
	}
	if length >= 0 && int64(len(b)) != length {
		return nil, fmt.Errorf("logs: short read of %s: %d of %d bytes", key, len(b), length)
	}
	return b, nil
}

func classify(err error) error {
	resp := minio.ToErrorResponse(err)
	if resp.StatusCode == http.StatusNotFound || resp.Code == "NoSuchKey" {
		return ErrObjectMissing
	}
	return err
}
