package artifactgc

import (
	"context"
	"fmt"
	"strings"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// MinIOStore is the result-store side of the sweep, over any S3-compatible
// endpoint. It is separate from agent/internal/resultstore.Client on purpose:
// that client is the WORKER's data path and has no business knowing how to
// enumerate or delete, and this one has no business knowing how to Put. Keeping
// the delete capability out of the worker's client means a bug on the task path
// cannot reach a delete call at all.
type MinIOStore struct {
	mc     *minio.Client
	bucket string
}

// MinIOConfig mirrors the environment the rest of the stack already uses, so an
// operator does not learn a new set of variable names. See deploy/docker-compose.yml
// x-minio-env and deploy/backup-artifacts.sh, which read the same values.
type MinIOConfig struct {
	Endpoint        string
	Bucket          string
	Region          string
	AccessKeyID     string
	SecretAccessKey string
	UseTLS          bool
}

// NewMinIOStore dials the result store and VERIFIES the bucket exists.
//
// Verification is not optional here, and the reason is the trap documented at
// length in deploy/backup-artifacts.sh: an S3 listing against a bucket that does
// not resolve can come back empty and successful, which is byte-identical to
// "the bucket has no objects". For a backup script that produces an empty backup
// that claims success. For a GC sweep the failure mode is the mirror image and
// just as bad: an empty listing means zero candidates, so the sweep reports "no
// orphans" and an operator concludes the bucket is clean when in fact nothing was
// ever examined. Failing loudly at construction is the only way to tell the two
// apart, exactly as the backup script gates every count behind `mc stat`.
func NewMinIOStore(ctx context.Context, cfg MinIOConfig) (*MinIOStore, error) {
	if cfg.Endpoint == "" {
		return nil, fmt.Errorf("artifactgc: a result-store endpoint is required")
	}
	if cfg.Bucket == "" {
		return nil, fmt.Errorf("artifactgc: a result-store bucket is required")
	}
	secure := cfg.UseTLS || strings.HasPrefix(cfg.Endpoint, "https://")
	mc, err := minio.New(stripScheme(cfg.Endpoint), &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.AccessKeyID, cfg.SecretAccessKey, ""),
		Secure: secure,
		Region: cfg.Region,
	})
	if err != nil {
		return nil, fmt.Errorf("artifactgc: dial %s: %w", cfg.Endpoint, err)
	}
	exists, err := mc.BucketExists(ctx, cfg.Bucket)
	if err != nil {
		return nil, fmt.Errorf("artifactgc: stat bucket %s at %s: %w", cfg.Bucket, cfg.Endpoint, err)
	}
	if !exists {
		return nil, fmt.Errorf(
			"artifactgc: bucket %q does not exist at %s; refusing to sweep, because an "+
				"empty listing from a bucket that does not resolve is indistinguishable "+
				"from a bucket with no orphans", cfg.Bucket, cfg.Endpoint)
	}
	return &MinIOStore{mc: mc, bucket: cfg.Bucket}, nil
}

// Bucket reports the bucket being swept, for logging.
func (s *MinIOStore) Bucket() string { return s.bucket }

// ListObjects streams up to limit objects under prefix.
//
// Streaming with an explicit break at limit, NOT a full listing truncated
// afterwards: the point of the bound is that the sweep's memory and its S3 call
// count stay independent of bucket size. minio-go's ListObjects returns a channel
// backed by paginated requests, so breaking out genuinely stops fetching. The
// cancel is what makes the producer goroutine exit rather than block forever on a
// channel nobody is reading.
func (s *MinIOStore) ListObjects(ctx context.Context, prefix string, limit int) ([]Candidate, error) {
	if limit <= 0 {
		limit = DefaultMaxObjects
	}
	listCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	out := make([]Candidate, 0, min(limit, 1024))
	for obj := range s.mc.ListObjects(listCtx, s.bucket, minio.ListObjectsOptions{
		Prefix:    prefix,
		Recursive: true,
	}) {
		if obj.Err != nil {
			return nil, fmt.Errorf("artifactgc: list %s/%s: %w", s.bucket, prefix, obj.Err)
		}
		// Directory-style zero-byte markers are not results and are never
		// referenced, but they are also not ours to remove.
		if strings.HasSuffix(obj.Key, "/") {
			continue
		}
		out = append(out, Candidate{
			Key:          obj.Key,
			SizeBytes:    obj.Size,
			LastModified: obj.LastModified,
		})
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

// DeleteObject removes one object. Single-object deletes rather than a batch
// RemoveObjects call: the sweep deletes rarely and auditably, and one log line
// per removed key is worth more than the round-trips a batch would save.
func (s *MinIOStore) DeleteObject(ctx context.Context, key string) error {
	if key == "" {
		return fmt.Errorf("artifactgc: refusing to delete an empty key")
	}
	if err := s.mc.RemoveObject(ctx, s.bucket, key, minio.RemoveObjectOptions{}); err != nil {
		return fmt.Errorf("artifactgc: remove %s/%s: %w", s.bucket, key, err)
	}
	return nil
}

// StatObject reports whether one object exists.
//
// The distinction between "not found" and "could not tell" is the whole point of
// the error return, and it must never be collapsed. A NoSuchKey answer is
// evidence the object is gone; a timeout, a 403 or a DNS failure is evidence of
// nothing at all, and returning false for those would report a healthy bucket as
// total data loss. So only the specific not-found codes yield (false, nil) and
// everything else is an error the caller must stop on.
func (s *MinIOStore) StatObject(ctx context.Context, key string) (bool, error) {
	if key == "" {
		return false, fmt.Errorf("artifactgc: refusing to stat an empty key")
	}
	_, err := s.mc.StatObject(ctx, s.bucket, key, minio.StatObjectOptions{})
	if err == nil {
		return true, nil
	}
	switch minio.ToErrorResponse(err).Code {
	case "NoSuchKey", "NotFound":
		return false, nil
	}
	return false, fmt.Errorf("artifactgc: stat %s/%s: %w", s.bucket, key, err)
}

func stripScheme(endpoint string) string {
	endpoint = strings.TrimPrefix(endpoint, "https://")
	return strings.TrimPrefix(endpoint, "http://")
}
