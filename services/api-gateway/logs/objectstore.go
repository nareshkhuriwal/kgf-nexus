// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package logs

import (
	"context"
	"errors"

	"github.com/khuriwalgroup/kgf-nexus/internal/objstore"
)

// StoreReader implements ObjectReader over any result-store backend (S3,
// Azure Blob, GCS; ADR-0027). The gateway needs nothing but reads of
// <namespace>/logs/*; OPERATIONS §19.1 recommends a credential scoped to
// exactly that, and §26 gives the per-cloud form of that scope.
type StoreReader struct {
	store objstore.Store
}

// NewStoreReader wraps an opened store. It does not probe it: a gateway must
// start (and serve everything else) while the object store is down; log reads
// then fail individually and say so.
func NewStoreReader(s objstore.Store) *StoreReader { return &StoreReader{store: s} }

// MinIOConfig is the S3 configuration the reader was first built with, kept
// so existing callers and tests need not change.
type MinIOConfig struct {
	Endpoint        string
	Bucket          string
	Region          string
	AccessKeyID     string
	SecretAccessKey string
	UseTLS          bool
}

// NewMinIOReader builds a StoreReader over an S3-compatible store with static
// keys. New code opens an objstore.Store and calls NewStoreReader.
func NewMinIOReader(cfg MinIOConfig) (*StoreReader, error) {
	if cfg.Endpoint == "" || cfg.Bucket == "" {
		return nil, errors.New("logs: result-store endpoint and bucket are required")
	}
	if cfg.AccessKeyID == "" || cfg.SecretAccessKey == "" {
		return nil, errors.New("logs: result-store credentials are required to read logs")
	}
	s, err := objstore.Open(context.Background(), objstore.Config{
		Backend: objstore.BackendS3, Endpoint: cfg.Endpoint, Bucket: cfg.Bucket, Region: cfg.Region, UseTLS: cfg.UseTLS,
		S3: objstore.S3Config{Credentials: objstore.S3CredStatic, AccessKeyID: cfg.AccessKeyID, SecretAccessKey: cfg.SecretAccessKey},
	})
	if err != nil {
		return nil, err
	}
	return NewStoreReader(s), nil
}

// maxObjectRead caps a whole-object read; chunks are far smaller (the gateway
// refuses to index chunks over 8 MiB), so hitting this means a key was
// overwritten out of band.
const maxObjectRead = 16 << 20

// Read implements ObjectReader: length < 0 reads the whole object (bounded by
// maxObjectRead), otherwise exactly [offset, offset+length) as one range
// request, which is how a page of a compacted log is served.
func (m *StoreReader) Read(ctx context.Context, key string, offset, length int64) ([]byte, error) {
	if length == 0 {
		return []byte{}, nil
	}
	b, err := objstore.ReadAll(ctx, m.store, key, offset, length, max(int64(maxObjectRead), length))
	if errors.Is(err, objstore.ErrNotFound) {
		return nil, ErrObjectMissing
	}
	return b, err
}
