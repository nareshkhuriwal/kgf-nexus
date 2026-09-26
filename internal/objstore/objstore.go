// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

// Package objstore is the one interface every result-store user goes through
// (ADR-0027): the agent's data path (agent/internal/resultstore), the gateway's
// log reader (services/api-gateway/logs) and the reachability sweep
// (internal/artifactgc).
//
// Three backends implement it:
//
//   - s3     -- any S3-compatible store through minio-go: AWS S3, Silo/MinIO,
//     and GCS through its S3 interoperability API (HMAC keys). Credentials are
//     static keys, IRSA / web identity, EKS Pod Identity, ECS task roles or EC2
//     instance roles (s3.go).
//   - azblob -- Azure Blob Storage through the official azblob SDK, with
//     workload identity, managed identity or a shared key (azblob.go).
//   - gcs    -- Google Cloud Storage through cloud.google.com/go/storage, with
//     Application Default Credentials (GKE Workload Identity) (gcs.go).
//
// THE SEMANTICS ARE THE CONTRACT, not the backend. Every backend must pass the
// same conformance suite (objstoretest.Run), which pins down what callers rely
// on:
//
//   - Put writes the whole object or nothing: a failed Put never leaves a
//     partial object visible under the key. Large bodies go up in parts (S3
//     multipart, Azure staged blocks, GCS resumable chunks) and become visible
//     only when committed.
//   - PutOptions.IfAbsent is an atomic create-if-absent where the backend has
//     one (Azure If-None-Match: *, GCS ifGenerationMatch=0) and stat-then-put
//     on S3, where conditional writes are not universally supported (GCS
//     interop, older MinIO). Content addressing is what makes the S3 fallback
//     safe: a racing writer of the same key writes the same bytes.
//   - Stat and Get distinguish "the object does not exist" (ErrNotFound) from
//     "I could not tell" (every other error). Collapsing the two would make an
//     outage look like data loss to the GC's dangling-reference check.
//   - Get serves byte ranges, which is what makes a log page one range request
//     into the compacted log.
//   - List streams in pages and stops when the callback asks it to, so a sweep's
//     memory and request count are bounded by its own limit, not the bucket.
//   - Delete is idempotent: deleting a missing object succeeds.
//   - BucketExists answers false ONLY when the backend says the bucket or
//     container does not exist. The GC refuses to sweep a bucket it cannot
//     prove exists, because an empty listing of a bucket that does not resolve
//     is indistinguishable from a clean one.
package objstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// Backend names, as NEXUS_RESULT_STORE_BACKEND spells them and as the gateway
// advertises them to agents in ResultStoreConfig.backend.
const (
	BackendS3     = "s3"
	BackendAzBlob = "azblob"
	BackendGCS    = "gcs"
)

// Backends lists every supported backend, in documentation order.
var Backends = []string{BackendS3, BackendAzBlob, BackendGCS}

var (
	// ErrNotFound means the backend positively reported that the object does
	// not exist. It is never returned for a timeout, an authorization failure
	// or a missing bucket.
	ErrNotFound = errors.New("objstore: object not found")
	// ErrExists is returned by Put with IfAbsent when the object already exists.
	ErrExists = errors.New("objstore: object already exists")
	// ErrStop, returned by a List callback, ends the listing without an error.
	ErrStop = errors.New("objstore: stop listing")
)

// ObjectInfo describes one stored object.
type ObjectInfo struct {
	Key          string
	Size         int64
	LastModified time.Time
	ContentType  string
}

// PutOptions tune one Put.
type PutOptions struct {
	// ContentType defaults to application/octet-stream.
	ContentType string
	// IfAbsent refuses to replace an existing object; Put then returns
	// ErrExists. See the package comment for per-backend atomicity.
	IfAbsent bool
}

// Store is a bucket (S3, GCS) or container (Azure) of immutable objects.
//
// Keys are slash-separated paths without a leading slash, e.g.
// "<namespace>/<sha256>" or "<namespace>/logs/<attempt>/<seq>.ndjson".
// Implementations must be safe for concurrent use.
type Store interface {
	// Backend names the implementation (BackendS3, ...).
	Backend() string
	// Bucket names the bucket or container, for logs and references.
	Bucket() string

	// Put stores body (size bytes; -1 when unknown) under key.
	Put(ctx context.Context, key string, body io.Reader, size int64, opts PutOptions) error
	// Stat describes key, or returns ErrNotFound.
	Stat(ctx context.Context, key string) (ObjectInfo, error)
	// Get opens key for reading from offset. length < 0 reads to the end;
	// length == 0 yields an empty reader. It returns ErrNotFound before any
	// bytes are read when the object does not exist.
	Get(ctx context.Context, key string, offset, length int64) (io.ReadCloser, error)
	// List calls fn for every object whose key starts with prefix, in
	// lexical key order, fetching pages lazily. fn returning ErrStop ends the
	// listing with a nil error; any other error ends it with that error.
	// Directory markers (keys ending in "/") are not reported.
	List(ctx context.Context, prefix string, fn func(ObjectInfo) error) error
	// Delete removes key. Deleting a missing object is not an error.
	Delete(ctx context.Context, key string) error

	// BucketExists reports whether the bucket or container exists.
	BucketExists(ctx context.Context) (bool, error)
	// CreateBucket creates the bucket or container if it is absent. For local
	// development and tests only: production buckets are provisioned with
	// retention, encryption and lifecycle policy by Terraform.
	CreateBucket(ctx context.Context) error
}

// ValidateKey rejects keys every backend would treat differently or badly.
// Every backend's Put enforces it, so a caller bug fails the same way
// everywhere instead of writing a "//"-containing key on S3 that Azure would
// normalise into a different blob. Reads and deletes reject only the empty key:
// an object some older writer stored under an odd key must stay readable and
// collectable.
func ValidateKey(key string) error {
	switch {
	case key == "":
		return errors.New("objstore: empty object key")
	case strings.HasPrefix(key, "/"):
		return fmt.Errorf("objstore: object key %q must not start with '/'", key)
	case strings.HasSuffix(key, "/"):
		return fmt.Errorf("objstore: object key %q must not end with '/'", key)
	case strings.Contains(key, "//"):
		return fmt.Errorf("objstore: object key %q contains an empty path segment", key)
	case len(key) > 1024:
		return fmt.Errorf("objstore: object key is %d bytes; the limit is 1024", len(key))
	}
	for _, seg := range strings.Split(key, "/") {
		if seg == "." || seg == ".." {
			return fmt.Errorf("objstore: object key %q contains a relative path segment", key)
		}
	}
	return nil
}

// ReadAll reads a whole object (or a range of it) into memory, bounded by max
// bytes. A longer object is an error rather than a silent truncation.
func ReadAll(ctx context.Context, s Store, key string, offset, length, max int64) ([]byte, error) {
	rc, err := s.Get(ctx, key, offset, length)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	limit := max
	if length >= 0 && length < limit {
		limit = length
	}
	b, err := io.ReadAll(io.LimitReader(rc, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("objstore: %s is larger than the %d bytes expected", key, limit)
	}
	if length >= 0 && int64(len(b)) != length {
		return nil, fmt.Errorf("objstore: short read of %s: %d of %d bytes", key, len(b), length)
	}
	return b, nil
}

func contentTypeOr(ct string) string {
	if ct == "" {
		return "application/octet-stream"
	}
	return ct
}

// emptyReader is Get's answer to length == 0, without a round trip.
type emptyReader struct{}

func (emptyReader) Read([]byte) (int, error) { return 0, io.EOF }
func (emptyReader) Close() error             { return nil }
