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
	"net/http"
	"strings"
	"time"

	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// DefaultInlineThresholdBytes is the fallback when the control plane does not
// supply one. 64 KiB per ADR-0008: below it the two round-trips to object
// storage cost more than the bytes are worth, and in a 50-node DAG of trivial
// outputs that overhead is the dominant latency term.
const DefaultInlineThresholdBytes = 64 * 1024

// Client reads and writes content-addressed objects.
type Client struct {
	mc     *minio.Client
	bucket string
	// namespace prefixes every key. It is the tenancy boundary: scoped worker
	// credentials should permit only this prefix, so a compromised worker cannot
	// read another tenant's results.
	namespace string
	inlineMax int64
	log       *slog.Logger
}

// Config is what the agent needs to reach the store. Endpoint, bucket and the
// inline threshold all arrive from RegisterResponse.result_store, so the control
// plane — not the worker's own config file — is authoritative about where
// results live.
type Config struct {
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
}

// New builds a client. It does NOT verify the bucket exists: a worker that
// cannot reach the store at startup should still register and start heartbeating
// (so the control plane sees a live worker) and fail individual tasks with
// INFRASTRUCTURE, rather than refusing to boot and silently shrinking the fleet
// during an object-store blip.
func New(cfg Config) (*Client, error) {
	if cfg.Endpoint == "" {
		return nil, errors.New("resultstore: endpoint is required")
	}
	if cfg.Bucket == "" {
		return nil, errors.New("resultstore: bucket is required")
	}
	creds := credentials.NewStaticV4(cfg.AccessKeyID, cfg.SecretAccessKey, cfg.SessionToken)
	mc, err := minio.New(stripScheme(cfg.Endpoint), &minio.Options{
		Creds:  creds,
		Secure: cfg.UseTLS || strings.HasPrefix(cfg.Endpoint, "https://"),
		Region: cfg.Region,
	})
	if err != nil {
		return nil, fmt.Errorf("resultstore: new client for %s: %w", cfg.Endpoint, err)
	}
	inline := cfg.InlineThreshold
	if inline <= 0 {
		inline = DefaultInlineThresholdBytes
	}
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	return &Client{mc: mc, bucket: cfg.Bucket, namespace: cfg.Namespace, inlineMax: inline, log: log}, nil
}

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
	size := int64(len(payload))
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	if size <= c.inlineMax {
		return &nexusv1.ResultRef{
			Kind:        &nexusv1.ResultRef_Inline{Inline: payload},
			ContentType: contentType,
			SizeBytes:   size,
		}, nil
	}

	sum := sha256.Sum256(payload)
	digest := hex.EncodeToString(sum[:])
	key := c.Key(digest)

	// Content addressing makes a re-upload of identical bytes pointless: the
	// object is already exactly what we would write. Skipping it turns a retry
	// of a large output into a metadata call instead of a re-transfer.
	if _, err := c.mc.StatObject(ctx, c.bucket, key, minio.StatObjectOptions{}); err == nil {
		c.log.Debug("result already present; skipping upload",
			"bucket", c.bucket, "key", key, "size_bytes", size)
		return blobRef(c.bucket, key, digest, contentType, size), nil
	} else if !isNotFound(err) {
		return nil, fmt.Errorf("resultstore: stat %s/%s: %w", c.bucket, key, err)
	}

	_, err := c.mc.PutObject(ctx, c.bucket, key, bytes.NewReader(payload), size,
		minio.PutObjectOptions{ContentType: contentType})
	if err != nil {
		return nil, fmt.Errorf("resultstore: put %s/%s: %w", c.bucket, key, err)
	}
	c.log.Debug("result stored", "bucket", c.bucket, "key", key, "size_bytes", size)
	return blobRef(c.bucket, key, digest, contentType, size), nil
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
		if bucket == "" {
			bucket = c.bucket
		}
		obj, err := c.mc.GetObject(ctx, bucket, k.Blob.GetKey(), minio.GetObjectOptions{})
		if err != nil {
			return nil, fmt.Errorf("resultstore: get %s/%s: %w", bucket, k.Blob.GetKey(), err)
		}
		defer obj.Close()
		b, err := io.ReadAll(obj)
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
	ok, err := c.mc.BucketExists(ctx, c.bucket)
	if err != nil {
		return fmt.Errorf("resultstore: bucket exists %s: %w", c.bucket, err)
	}
	if ok {
		return nil
	}
	if err := c.mc.MakeBucket(ctx, c.bucket, minio.MakeBucketOptions{}); err != nil {
		// A racing worker may have created it between our check and our call.
		if exists, ierr := c.mc.BucketExists(ctx, c.bucket); ierr == nil && exists {
			return nil
		}
		return fmt.Errorf("resultstore: make bucket %s: %w", c.bucket, err)
	}
	return nil
}

// Reachable probes the store, used by /readyz and by the agent's startup log.
func (c *Client) Reachable(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if _, err := c.mc.BucketExists(ctx, c.bucket); err != nil {
		return fmt.Errorf("resultstore: unreachable: %w", err)
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

func isNotFound(err error) bool {
	var resp minio.ErrorResponse
	if errors.As(err, &resp) {
		return resp.StatusCode == http.StatusNotFound || resp.Code == "NoSuchKey"
	}
	return minio.ToErrorResponse(err).StatusCode == http.StatusNotFound
}

func stripScheme(endpoint string) string {
	endpoint = strings.TrimPrefix(endpoint, "https://")
	return strings.TrimPrefix(endpoint, "http://")
}
