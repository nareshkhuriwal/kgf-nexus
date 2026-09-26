// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package objstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"cloud.google.com/go/storage"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
)

// gcsStore is the native Google Cloud Storage backend.
//
// Why native when GCS speaks S3 through its interoperability API: interop
// signs with HMAC keys, which are long-lived secrets bound to a service
// account. The native client authenticates with Application Default
// Credentials, which on GKE is Workload Identity -- short-lived tokens from
// the metadata server and no secret in the cluster at all (F115).
type gcsStore struct {
	client    *storage.Client
	bkt       *storage.BucketHandle
	bucket    string
	chunkSize int
	projectID string
}

func newGCS(ctx context.Context, c Config) (*gcsStore, error) {
	opts := []option.ClientOption{
		// The storage client can export its own client-side metrics to Cloud
		// Monitoring. Nexus exports its own through /metrics; a second,
		// uninvited exporter needing monitoring.write is not wanted.
		storage.WithDisabledClientMetrics(),
	}
	if c.Endpoint != "" {
		ep := c.Endpoint
		if !strings.Contains(ep, "://") {
			scheme := "http"
			if c.UseTLS {
				scheme = "https"
			}
			ep = scheme + "://" + ep
		}
		if !strings.Contains(strings.TrimPrefix(strings.SplitN(ep, "://", 2)[1], "/"), "/storage/v1") {
			ep = strings.TrimSuffix(ep, "/") + "/storage/v1/"
		}
		// An endpoint override means an emulator or a private gateway, where
		// the XML read host derived from the default endpoint does not exist;
		// JSON reads use the same host as every other call.
		opts = append(opts, option.WithEndpoint(ep), storage.WithJSONReads())
	}
	switch c.GCS.Auth {
	case "", GCSAuthAuto:
		// ADC: GKE Workload Identity (the metadata server), or
		// GOOGLE_APPLICATION_CREDENTIALS. Tokens refresh inside the client.
	case GCSAuthNone:
		opts = append(opts, option.WithoutAuthentication())
	default:
		return nil, fmt.Errorf("objstore: unknown %s %q", EnvGCSAuth, c.GCS.Auth)
	}
	client, err := storage.NewClient(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("objstore: gcs client: %w", err)
	}
	// Retries: the client retries idempotent operations by default. Uploads
	// with a precondition (IfAbsent) are idempotent; unconditional uploads of
	// content-addressed or per-attempt keys are idempotent BY CONSTRUCTION
	// (the same key always receives the same bytes), which the client cannot
	// know, so retry them too.
	bkt := client.Bucket(c.Bucket).Retryer(storage.WithPolicy(storage.RetryAlways))
	return &gcsStore{client: client, bkt: bkt, bucket: c.Bucket, chunkSize: int(c.partSize()),
		projectID: c.GCS.ProjectID}, nil
}

func (s *gcsStore) Backend() string { return BackendGCS }
func (s *gcsStore) Bucket() string  { return s.bucket }

// Put streams through a resumable upload in chunkSize pieces; the object
// becomes visible only when the final chunk commits. IfAbsent is the
// ifGenerationMatch=0 precondition, which GCS evaluates atomically.
func (s *gcsStore) Put(ctx context.Context, key string, body io.Reader, size int64, opts PutOptions) error {
	if err := ValidateKey(key); err != nil {
		return err
	}
	obj := s.bkt.Object(key)
	if opts.IfAbsent {
		obj = obj.If(storage.Conditions{DoesNotExist: true})
	}
	// Cancelling wctx is how a failed copy ABORTS the upload: closing the
	// writer after a short read would commit a truncated object.
	wctx, cancel := context.WithCancel(ctx)
	defer cancel()
	w := obj.NewWriter(wctx)
	w.ContentType = contentTypeOr(opts.ContentType)
	w.ChunkSize = s.chunkSize
	if size >= 0 && size < int64(s.chunkSize) {
		// One request, and no chunk-sized buffer, for a small object.
		w.ChunkSize = 0
	}
	n, err := io.Copy(w, body)
	if err == nil && size >= 0 && n != size {
		err = fmt.Errorf("body was %d bytes, expected %d", n, size)
	}
	if err != nil {
		cancel()
		_ = w.Close()
		return fmt.Errorf("objstore: gcs put %s/%s: %w", s.bucket, key, err)
	}
	if err := w.Close(); err != nil {
		if opts.IfAbsent && gcsStatus(err) == http.StatusPreconditionFailed {
			return ErrExists
		}
		return fmt.Errorf("objstore: gcs put %s/%s: %w", s.bucket, key, err)
	}
	return nil
}

func (s *gcsStore) Stat(ctx context.Context, key string) (ObjectInfo, error) {
	if key == "" {
		return ObjectInfo{}, errors.New("objstore: empty object key")
	}
	a, err := s.bkt.Object(key).Attrs(ctx)
	if err != nil {
		if errors.Is(err, storage.ErrObjectNotExist) {
			return ObjectInfo{}, ErrNotFound
		}
		return ObjectInfo{}, fmt.Errorf("objstore: gcs stat %s/%s: %w", s.bucket, key, err)
	}
	return ObjectInfo{Key: key, Size: a.Size, LastModified: a.Updated, ContentType: a.ContentType}, nil
}

func (s *gcsStore) Get(ctx context.Context, key string, offset, length int64) (io.ReadCloser, error) {
	if key == "" {
		return nil, errors.New("objstore: empty object key")
	}
	if length == 0 {
		return emptyReader{}, nil
	}
	if length < 0 {
		length = -1
	}
	r, err := s.bkt.Object(key).NewRangeReader(ctx, offset, length)
	if err != nil {
		if errors.Is(err, storage.ErrObjectNotExist) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("objstore: gcs get %s/%s: %w", s.bucket, key, err)
	}
	return r, nil
}

func (s *gcsStore) List(ctx context.Context, prefix string, fn func(ObjectInfo) error) error {
	return s.list(ctx, prefix, "", fn)
}

// ListAfter implements AfterLister with the JSON API's startOffset, which is
// INCLUSIVE: the cursor object itself is skipped here.
func (s *gcsStore) ListAfter(ctx context.Context, prefix, startAfter string, fn func(ObjectInfo) error) error {
	return s.list(ctx, prefix, startAfter, fn)
}

func (s *gcsStore) list(ctx context.Context, prefix, startAfter string, fn func(ObjectInfo) error) error {
	q := &storage.Query{Prefix: prefix, StartOffset: startAfter}
	// Only the fields the sweep reads: a listing of millions of objects should
	// not carry ACLs and metadata it discards.
	if err := q.SetAttrSelection([]string{"Name", "Size", "Updated", "ContentType"}); err != nil {
		return fmt.Errorf("objstore: gcs list attrs: %w", err)
	}
	it := s.bkt.Objects(ctx, q)
	for {
		a, err := it.Next()
		if errors.Is(err, iterator.Done) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("objstore: gcs list %s/%s: %w", s.bucket, prefix, err)
		}
		if strings.HasSuffix(a.Name, "/") || (startAfter != "" && a.Name <= startAfter) {
			continue
		}
		if err := fn(ObjectInfo{Key: a.Name, Size: a.Size, LastModified: a.Updated, ContentType: a.ContentType}); err != nil {
			if errors.Is(err, ErrStop) {
				return nil
			}
			return err
		}
	}
}

func (s *gcsStore) Delete(ctx context.Context, key string) error {
	if key == "" {
		return errors.New("objstore: refusing to delete an empty key")
	}
	if err := s.bkt.Object(key).Delete(ctx); err != nil && !errors.Is(err, storage.ErrObjectNotExist) {
		return fmt.Errorf("objstore: gcs delete %s/%s: %w", s.bucket, key, err)
	}
	return nil
}

// BucketExists lists at most one object rather than reading bucket metadata:
// storage.buckets.get is not in roles/storage.objectAdmin, the role a
// Workload Identity service account is normally granted on the bucket, while
// storage.objects.list is. A missing bucket answers 404 to both.
func (s *gcsStore) BucketExists(ctx context.Context) (bool, error) {
	q := &storage.Query{}
	_ = q.SetAttrSelection([]string{"Name"})
	it := s.bkt.Objects(ctx, q)
	it.PageInfo().MaxSize = 1
	_, err := it.Next()
	switch {
	case err == nil, errors.Is(err, iterator.Done):
		return true, nil
	case errors.Is(err, storage.ErrBucketNotExist), gcsStatus(err) == http.StatusNotFound:
		return false, nil
	}
	return false, fmt.Errorf("objstore: gcs probe bucket %s: %w", s.bucket, err)
}

func (s *gcsStore) CreateBucket(ctx context.Context) error {
	if ok, err := s.BucketExists(ctx); err == nil && ok {
		return nil
	}
	project := s.projectID
	if project == "" {
		project = "nexus-local"
	}
	if err := s.client.Bucket(s.bucket).Create(ctx, project, nil); err != nil && gcsStatus(err) != http.StatusConflict {
		return fmt.Errorf("objstore: gcs create bucket %s: %w", s.bucket, err)
	}
	return nil
}

func gcsStatus(err error) int {
	var ge *googleapi.Error
	if errors.As(err, &ge) {
		return ge.Code
	}
	return 0
}
