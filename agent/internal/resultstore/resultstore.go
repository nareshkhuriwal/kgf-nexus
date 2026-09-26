// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

// Package resultstore is the worker's DIRECT client for task inputs and
// outputs (ADR-0008).
//
// "Direct" is the load-bearing word. Payloads must never transit the control
// plane: routing a 200 GiB checkpoint through the API gateway would saturate the
// control plane's network path and starve heartbeats, and a starved heartbeat is
// a wrongly-declared-dead worker, which is DUPLICATED WORK. So a data-path
// problem becomes a correctness problem, and the fix is that the data path never
// touches the control plane at all. The metadata store holds only references.
package resultstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
	"github.com/khuriwalgroup/kgf-nexus/internal/domain"
	"github.com/khuriwalgroup/kgf-nexus/internal/objstore"
	"github.com/khuriwalgroup/kgf-nexus/internal/tracing"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// DefaultInlineThresholdBytes is the fallback when the control plane does not
// supply one. 64 KiB per ADR-0008: below it the two round-trips to object
// storage cost more than the bytes are worth, and in a 50-node DAG of trivial
// outputs that overhead is the dominant latency term.
//
// Aliases internal/domain so the gateway that configures the value and the
// agent that applies it share one definition (ADR-0016).
const DefaultInlineThresholdBytes = domain.DefaultInlineThresholdBytes

// Client reads and writes content-addressed objects.
type Client struct {
	store  objstore.Store
	bucket string
	// namespace prefixes every key. It is the tenancy boundary: scoped worker
	// credentials should permit only this prefix, so a compromised worker cannot
	// read another tenant's results.
	namespace string
	inlineMax int64
	log       *slog.Logger
	onPut     func(result string, bytes int64, d time.Duration)
	// refreshAge: a deduplicated object older than this is re-uploaded; see
	// DedupeRefreshAge.
	refreshAge time.Duration
}

// DedupeRefreshAge bounds how old an existing object may be for a Put to skip
// the upload and reuse it (F141).
//
// The artifact GC deletes unreferenced objects older than its grace period
// (24 h by default). An old ORPHAN with the same content as a new output --
// the same bytes a pruned run produced last week -- is exactly such an object.
// If Put deduplicated against it without touching it, the GC could delete it
// in the window between this Put and the attempt's report committing the
// reference, and the run would record a reference to bytes that no longer
// exist. Re-uploading refreshes the object's modification time, which moves it
// back inside every grace period; an hour keeps the saving for the common case
// (a retry, or a fan-out producing identical outputs moments apart) while
// leaving the GC grace period 23 hours of margin.
const DedupeRefreshAge = time.Hour

// PutResult values passed to Config.OnPut.
const (
	PutInline       = "inline"       // at or below the threshold; never left the agent
	PutUploaded     = "uploaded"     // written to object storage
	PutDeduplicated = "deduplicated" // identical content already present; nothing transferred
	PutError        = "error"        // stat or upload failed
)

// Config is what the agent needs to reach the store. Endpoint, bucket and the
// inline threshold all arrive from RegisterResponse.result_store, so the control
// plane — not the worker's own config file — is authoritative about where
// results live.
type Config struct {
	// Store, when set, is the backend to use (s3, azblob or gcs; see
	// internal/objstore) and every S3 field below is ignored. The agent builds
	// it from the advertised ResultStoreConfig and its own credentials.
	Store objstore.Store

	// The S3 fields build an S3 backend when Store is nil. With no key pair
	// the requests are unsigned, as they always were.
	Endpoint        string
	Bucket          string
	Region          string
	Namespace       string
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
	UseTLS          bool
	InlineThreshold int64
	Log             *slog.Logger
	// OnPut, when set, is told the outcome, payload size and duration of every
	// Put. It is how the agent's metrics see upload bytes and latency without
	// this package depending on them. It must not block.
	OnPut func(result string, bytes int64, d time.Duration)
}

// New builds a client. It does NOT verify the bucket exists: a worker that
// cannot reach the store at startup should still register and start heartbeating
// (so the control plane sees a live worker) and fail individual tasks with
// INFRASTRUCTURE, rather than refusing to boot and silently shrinking the fleet
// during an object-store blip.
func New(cfg Config) (*Client, error) {
	store := cfg.Store
	if store == nil {
		if cfg.Endpoint == "" {
			return nil, errors.New("resultstore: endpoint is required")
		}
		if cfg.Bucket == "" {
			return nil, errors.New("resultstore: bucket is required")
		}
		mode := objstore.S3CredStatic
		if cfg.AccessKeyID == "" || cfg.SecretAccessKey == "" {
			mode = objstore.S3CredAnonymous
		}
		var err error
		store, err = objstore.Open(context.Background(), objstore.Config{
			Backend: objstore.BackendS3, Endpoint: cfg.Endpoint, Bucket: cfg.Bucket,
			Region: cfg.Region, UseTLS: cfg.UseTLS,
			S3: objstore.S3Config{Credentials: mode, AccessKeyID: cfg.AccessKeyID,
				SecretAccessKey: cfg.SecretAccessKey, SessionToken: cfg.SessionToken},
		})
		if err != nil {
			return nil, fmt.Errorf("resultstore: new client for %s: %w", cfg.Endpoint, err)
		}
	}
	inline := domain.ClampInlineThreshold(cfg.InlineThreshold)
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	return &Client{store: store, bucket: store.Bucket(), namespace: cfg.Namespace, inlineMax: inline, log: log,
		onPut: cfg.OnPut, refreshAge: DedupeRefreshAge}, nil
}

// Backend names the object-store backend in use.
func (c *Client) Backend() string { return c.store.Backend() }

// InlineThreshold reports the configured inline cutoff, for tests and logs.
func (c *Client) InlineThreshold() int64 { return c.inlineMax }

// Put stores a payload and returns the ResultRef the control plane will record.
//
// Payloads at or below the inline threshold are returned INLINE and never touch
// object storage, avoiding two network round-trips for `{"rows": 42}`. Larger
// payloads are written content-addressed at <namespace>/<sha256>.
//
// Objects are IMMUTABLE. A retry writes a NEW object rather than mutating an
// existing one, which is what makes content addressing and memoization sound: a
// reference that could change out from under a consumer is not self-verifying,
// and a cache key over mutable content is a correctness bug waiting for a
// deadline.
func (c *Client) Put(ctx context.Context, payload []byte, contentType string) (*nexusv1.ResultRef, error) {
	start := time.Now()
	ctx, span := tracing.Start(ctx, "resultstore.put",
		trace.WithAttributes(attribute.Int64("nexus.result.size_bytes", int64(len(payload)))))
	ref, result, err := c.put(ctx, payload, contentType)
	span.SetAttributes(attribute.String("nexus.result.outcome", result))
	tracing.End(span, err)
	if c.onPut != nil {
		c.onPut(result, int64(len(payload)), time.Since(start))
	}
	return ref, err
}

func (c *Client) put(ctx context.Context, payload []byte, contentType string) (*nexusv1.ResultRef, string, error) {
	size := int64(len(payload))
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	if size <= c.inlineMax {
		return &nexusv1.ResultRef{
			Kind:        &nexusv1.ResultRef_Inline{Inline: payload},
			ContentType: contentType,
			SizeBytes:   size,
		}, PutInline, nil
	}

	sum := sha256.Sum256(payload)
	digest := hex.EncodeToString(sum[:])
	key := c.Key(digest)

	// Content addressing makes a re-upload of identical bytes pointless: the
	// object is already exactly what we would write. Skipping it turns a retry
	// of a large output into a metadata call instead of a re-transfer -- unless
	// the existing object is old enough to be a GC candidate (DedupeRefreshAge).
	info, err := c.store.Stat(ctx, key)
	switch {
	case err == nil && time.Since(info.LastModified) < c.refreshAge:
		c.log.DebugContext(ctx, "result already present; skipping upload",
			"bucket", c.bucket, "key", key, "size_bytes", size)
		return blobRef(c.bucket, key, digest, contentType, size), PutDeduplicated, nil
	case err == nil:
		c.log.DebugContext(ctx, "result present but old; re-uploading to refresh it ahead of the GC grace period",
			"bucket", c.bucket, "key", key, "modified", info.LastModified)
	case !errors.Is(err, objstore.ErrNotFound):
		return nil, PutError, fmt.Errorf("resultstore: stat %s/%s: %w", c.bucket, key, err)
	}

	if err := c.store.Put(ctx, key, bytes.NewReader(payload), size,
		objstore.PutOptions{ContentType: contentType}); err != nil {
		return nil, PutError, fmt.Errorf("resultstore: put %s/%s: %w", c.bucket, key, err)
	}
	c.log.DebugContext(ctx, "result stored", "bucket", c.bucket, "key", key, "size_bytes", size)
	return blobRef(c.bucket, key, digest, contentType, size), PutUploaded, nil
}

// Get resolves a ResultRef to bytes, VERIFYING the digest for blob refs.
//
// Verification is not paranoia: the reference IS the hash, so a mismatch means
// either corruption in transit or an object that was mutated in violation of the
// immutability rule. Feeding either to a task would produce a silently wrong
// result, which is worse than a failed one.
func (c *Client) Get(ctx context.Context, ref *nexusv1.ResultRef) ([]byte, error) {
	if ref == nil {
		return nil, nil
	}
	switch k := ref.GetKind().(type) {
	case *nexusv1.ResultRef_Inline:
		return k.Inline, nil
	case *nexusv1.ResultRef_Blob:
		bucket := k.Blob.GetBucket()
		if bucket != "" && bucket != c.bucket {
			// A client is bound to one bucket (or container). A reference into
			// another one means the deployment's result store moved; reading it
			// needs that store's credentials, which this worker does not have.
			return nil, fmt.Errorf("resultstore: %s/%s is in bucket %q, but this worker's result store is %q",
				bucket, k.Blob.GetKey(), bucket, c.bucket)
		}
		bucket = c.bucket
		rc, err := c.store.Get(ctx, k.Blob.GetKey(), 0, -1)
		if err != nil {
			return nil, fmt.Errorf("resultstore: get %s/%s: %w", bucket, k.Blob.GetKey(), err)
		}
		defer rc.Close()
		b, err := io.ReadAll(rc)
		if err != nil {
			return nil, fmt.Errorf("resultstore: read %s/%s: %w", bucket, k.Blob.GetKey(), err)
		}
		if want := k.Blob.GetSha256(); want != "" {
			sum := sha256.Sum256(b)
			if got := hex.EncodeToString(sum[:]); got != want {
				return nil, fmt.Errorf("resultstore: digest mismatch for %s/%s: want %s, got %s",
					bucket, k.Blob.GetKey(), want, got)
			}
		}
		return b, nil
	default:
		return nil, fmt.Errorf("resultstore: ResultRef has no kind set")
	}
}

// Key renders the content-addressed object key: <namespace>/<sha256>.
func (c *Client) Key(digest string) string {
	if c.namespace == "" {
		return digest
	}
	return c.namespace + "/" + digest
}

// EnsureBucket creates the bucket when absent. Intended for local development
// and tests only: in production the bucket is provisioned with a retention and
// lifecycle policy, and a worker with permission to create buckets has far more
// authority than a worker needs.
func (c *Client) EnsureBucket(ctx context.Context) error {
	if err := c.store.CreateBucket(ctx); err != nil {
		return fmt.Errorf("resultstore: %w", err)
	}
	return nil
}

// Reachable probes the store, used by /readyz and by the agent's startup log.
func (c *Client) Reachable(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	ok, err := c.store.BucketExists(ctx)
	if err != nil {
		return fmt.Errorf("resultstore: unreachable: %w", err)
	}
	if !ok {
		return fmt.Errorf("resultstore: bucket %q does not exist", c.bucket)
	}
	return nil
}

func blobRef(bucket, key, digest, contentType string, size int64) *nexusv1.ResultRef {
	return &nexusv1.ResultRef{
		Kind: &nexusv1.ResultRef_Blob{Blob: &nexusv1.BlobRef{
			Bucket: bucket, Key: key, Sha256: digest,
		}},
		ContentType: contentType,
		SizeBytes:   size,
	}
}

// ErrLogKeyOutsideNamespace refuses a log write outside this worker's own
// namespace prefix. The gateway refuses to INDEX such a key anyway; refusing
// to WRITE it means a bug here cannot scribble into another tenant's prefix
// with credentials that happen to be broader than they should be.
var ErrLogKeyOutsideNamespace = errors.New("resultstore: log key outside this namespace's log prefix")

// PutLogObject writes one log object (a chunk or the compacted log) at an
// explicit key under <namespace>/logs/ (ADR-0020).
//
// Unlike Put this is NOT content-addressed: a log chunk is identified by its
// attempt and sequence number, because that is what the index and the read API
// address. A retry rewrites the same bytes to the same key.
func (c *Client) PutLogObject(ctx context.Context, key string, body io.Reader, size int64) error {
	if c.namespace == "" || !strings.HasPrefix(key, c.namespace+"/logs/") {
		return fmt.Errorf("%w: %q", ErrLogKeyOutsideNamespace, key)
	}
	if err := c.store.Put(ctx, key, body, size,
		objstore.PutOptions{ContentType: "application/x-ndjson"}); err != nil {
		return fmt.Errorf("resultstore: put log %s/%s: %w", c.bucket, key, err)
	}
	return nil
}
