// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package artifactgc

import (
	"context"
	"errors"
	"fmt"

	"github.com/khuriwalgroup/kgf-nexus/internal/objstore"
)

// ObjectStore is the result-store side of the sweep over any backend (S3,
// Azure Blob, GCS; ADR-0027). It is separate from
// agent/internal/resultstore.Client on purpose: that client is the WORKER's
// data path and has no business deleting, and this one has no business
// writing. Keeping the delete capability out of the worker's client means a bug
// on the task path cannot reach a delete call at all.
//
// The sweep's safety argument (list, then query reachability; the grace
// period on LastModified; stop on any "could not tell") depends only on the
// objstore contract, which every backend passes the same conformance suite
// for, so it holds unchanged on Azure and GCS.
type ObjectStore struct {
	s objstore.Store
}

// MinIOStore is the name this type had while S3 was the only backend.
type MinIOStore = ObjectStore

// NewObjectStore wraps an opened store and VERIFIES the bucket exists.
//
// Verification is not optional here, and the reason is the trap documented at
// length in deploy/backup-artifacts.sh: a listing against a bucket that does
// not resolve can come back empty and successful, which is byte-identical to
// "the bucket has no objects". For a backup script that produces an empty backup
// that claims success. For a GC sweep the failure mode is the mirror image and
// just as bad: an empty listing means zero candidates, so the sweep reports "no
// orphans" and an operator concludes the bucket is clean when in fact nothing was
// ever examined. Failing loudly at construction is the only way to tell the two
// apart. On GCS it is also what makes the object-level "not found" safe to
// believe: GCS answers 404 for an object in a missing bucket too.
func NewObjectStore(ctx context.Context, s objstore.Store) (*ObjectStore, error) {
	exists, err := s.BucketExists(ctx)
	if err != nil {
		return nil, fmt.Errorf("artifactgc: stat %s bucket %s: %w", s.Backend(), s.Bucket(), err)
	}
	if !exists {
		return nil, fmt.Errorf(
			"artifactgc: %s bucket %q does not exist; refusing to sweep, because an "+
				"empty listing from a bucket that does not resolve is indistinguishable "+
				"from a bucket with no orphans", s.Backend(), s.Bucket())
	}
	return &ObjectStore{s: s}, nil
}

// MinIOConfig is the S3 static-key configuration NewMinIOStore takes.
type MinIOConfig struct {
	Endpoint        string
	Bucket          string
	Region          string
	AccessKeyID     string
	SecretAccessKey string
	UseTLS          bool
}

// NewMinIOStore opens an S3 store with static keys and verifies its bucket.
// New code opens an objstore.Store and calls NewObjectStore.
func NewMinIOStore(ctx context.Context, cfg MinIOConfig) (*ObjectStore, error) {
	if cfg.Endpoint == "" {
		return nil, errors.New("artifactgc: a result-store endpoint is required")
	}
	s, err := objstore.Open(ctx, objstore.Config{
		Backend: objstore.BackendS3, Endpoint: cfg.Endpoint, Bucket: cfg.Bucket, Region: cfg.Region, UseTLS: cfg.UseTLS,
		S3: objstore.S3Config{Credentials: objstore.S3CredStatic, AccessKeyID: cfg.AccessKeyID, SecretAccessKey: cfg.SecretAccessKey},
	})
	if err != nil {
		return nil, fmt.Errorf("artifactgc: %w", err)
	}
	return NewObjectStore(ctx, s)
}

// Bucket reports the bucket being swept, for logging.
func (s *ObjectStore) Bucket() string { return s.s.Bucket() }

// Backend names the backend being swept, for logging.
func (s *ObjectStore) Backend() string { return s.s.Backend() }

// ListObjects streams up to limit objects under prefix.
//
// Streaming with an explicit stop at limit, NOT a full listing truncated
// afterwards: the point of the bound is that the sweep's memory and its request
// count stay independent of bucket size. Every backend fetches pages lazily and
// stops fetching when the callback returns ErrStop.
func (s *ObjectStore) ListObjects(ctx context.Context, prefix string, limit int) ([]Candidate, error) {
	if limit <= 0 {
		limit = DefaultMaxObjects
	}
	out := make([]Candidate, 0, min(limit, 1024))
	err := s.s.List(ctx, prefix, func(o objstore.ObjectInfo) error {
		out = append(out, Candidate{Key: o.Key, SizeBytes: o.Size, LastModified: o.LastModified})
		if len(out) >= limit {
			return objstore.ErrStop
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("artifactgc: list %s/%s: %w", s.s.Bucket(), prefix, err)
	}
	return out, nil
}

// DeleteObject removes one object. Single-object deletes rather than a batch:
// the sweep deletes rarely and auditably, and one log line per removed key is
// worth more than the round-trips a batch would save.
func (s *ObjectStore) DeleteObject(ctx context.Context, key string) error {
	if key == "" {
		return errors.New("artifactgc: refusing to delete an empty key")
	}
	if err := s.s.Delete(ctx, key); err != nil {
		return fmt.Errorf("artifactgc: %w", err)
	}
	return nil
}

// StatObject reports whether one object exists.
//
// The distinction between "not found" and "could not tell" is the whole point of
// the error return, and it must never be collapsed. A not-found answer is
// evidence the object is gone; a timeout, a 403 or a DNS failure is evidence of
// nothing at all, and returning false for those would report a healthy bucket as
// total data loss. objstore returns ErrNotFound only for the positive answer.
func (s *ObjectStore) StatObject(ctx context.Context, key string) (bool, error) {
	if key == "" {
		return false, errors.New("artifactgc: refusing to stat an empty key")
	}
	_, err := s.s.Stat(ctx, key)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, objstore.ErrNotFound):
		return false, nil
	}
	return false, fmt.Errorf("artifactgc: %w", err)
}
