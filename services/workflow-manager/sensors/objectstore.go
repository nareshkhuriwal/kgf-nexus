// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package sensors

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/minio/minio-go/v7"

	"github.com/khuriwalgroup/kgf-nexus/internal/objstore"
)

// ObjectInfo is one listed object.
type ObjectInfo struct {
	Key          string
	Size         int64
	ETag         string
	LastModified time.Time
}

// Location is a parsed object-store URI: scheme://bucket/prefix.
type Location struct {
	Scheme string
	Bucket string
	Prefix string
}

// URI renders the location back.
func (l Location) URI() string { return l.Scheme + "://" + l.Bucket + "/" + l.Prefix }

// ParseLocation parses "s3://bucket/prefix/". The prefix may be empty.
func ParseLocation(raw string) (Location, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return Location{}, fmt.Errorf("%q is not a scheme://bucket/prefix URI", raw)
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return Location{}, fmt.Errorf("%q must not carry credentials, a query or a fragment", raw)
	}
	return Location{Scheme: strings.ToLower(u.Scheme), Bucket: u.Host, Prefix: strings.TrimPrefix(u.Path, "/")}, nil
}

// ObjectLister is everything a data-arrival sensor needs from a store. Keep it
// this small: native Azure Blob and GCS backends implement the same two
// behaviours (lexicographic listing after a key, bounded pages) and register
// under their own schemes.
type ObjectLister interface {
	// List returns up to max objects under loc.Prefix whose keys sort after
	// startAfter, in lexicographic order, and whether more may follow.
	List(ctx context.Context, loc Location, startAfter string, max int) ([]ObjectInfo, bool, error)
}

// ListerFactory builds a lister for a scheme on first use.
type ListerFactory func(ctx context.Context) (ObjectLister, error)

// Registry maps URI schemes to listers.
type Registry struct {
	mu        sync.Mutex
	factories map[string]ListerFactory
	built     map[string]ObjectLister
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{factories: map[string]ListerFactory{}, built: map[string]ObjectLister{}}
}

// Register installs a factory for scheme (e.g. "s3", "gs", "az").
func (r *Registry) Register(scheme string, f ListerFactory) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.factories[strings.ToLower(scheme)] = f
	delete(r.built, strings.ToLower(scheme))
}

// Schemes lists registered schemes.
func (r *Registry) Schemes() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.factories))
	for s := range r.factories {
		out = append(out, s)
	}
	return out
}

// ErrNoLister means no lister is registered for a scheme.
var ErrNoLister = errors.New("no object-store lister is configured for this scheme")

// For returns the lister for scheme, building it once.
func (r *Registry) For(ctx context.Context, scheme string) (ObjectLister, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	scheme = strings.ToLower(scheme)
	if l, ok := r.built[scheme]; ok {
		return l, nil
	}
	f, ok := r.factories[scheme]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNoLister, scheme)
	}
	l, err := f(ctx)
	if err != nil {
		return nil, err
	}
	r.built[scheme] = l
	return l, nil
}

// ── S3-compatible (minio-go, the result store's client library) ────────────

// S3Config addresses an S3-compatible endpoint.
type S3Config struct {
	Endpoint        string
	Region          string
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
	// Credentials is the result store's credential mode (NEXUS_S3_CREDENTIALS:
	// auto, static, web-identity, pod-identity, iam, env, anonymous; "" =
	// auto), so a sensor watching the platform's own store uses the same
	// keyless cloud identity (ADR-0027) rather than needing a key pair.
	Credentials string
	UseTLS      bool
}

// S3ConfigFromEnv reads NEXUS_SENSOR_S3_*, falling back to the result store's
// NEXUS_RESULT_STORE_ENDPOINT / NEXUS_S3_* so a default deployment can watch
// its own store without new settings. ok=false means nothing is configured.
func S3ConfigFromEnv(getenv func(string) string) (S3Config, bool) {
	if getenv == nil {
		getenv = os.Getenv
	}
	// The result store's settings describe an S3 endpoint only when it IS s3:
	// on azblob or gcs, NEXUS_RESULT_STORE_ENDPOINT is an Azure service URL or
	// a GCS emulator, and those stores have their own listers.
	own := resultStoreIs(getenv, objstore.BackendS3)
	pick := func(keys ...string) string {
		for i, k := range keys {
			if i > 0 && !own {
				break
			}
			if v := strings.TrimSpace(getenv(k)); v != "" {
				return v
			}
		}
		return ""
	}
	c := S3Config{
		Endpoint:        pick("NEXUS_SENSOR_S3_ENDPOINT", "NEXUS_RESULT_STORE_ENDPOINT", "RESULT_STORE_ENDPOINT"),
		Region:          pick("NEXUS_SENSOR_S3_REGION", "NEXUS_RESULT_STORE_REGION", "RESULT_STORE_REGION"),
		AccessKeyID:     pick("NEXUS_SENSOR_S3_ACCESS_KEY_ID", "NEXUS_S3_ACCESS_KEY_ID", "RESULT_STORE_ACCESS_KEY"),
		SecretAccessKey: pick("NEXUS_SENSOR_S3_SECRET_ACCESS_KEY", "NEXUS_S3_SECRET_ACCESS_KEY", "RESULT_STORE_SECRET_KEY"),
		SessionToken:    pick("NEXUS_SENSOR_S3_SESSION_TOKEN", "NEXUS_S3_SESSION_TOKEN"),
		Credentials:     pick("NEXUS_SENSOR_S3_CREDENTIALS", "NEXUS_S3_CREDENTIALS"),
	}
	tls := pick("NEXUS_SENSOR_S3_USE_TLS", "NEXUS_S3_USE_TLS")
	c.UseTLS = tls == "true" || strings.HasPrefix(c.Endpoint, "https://")
	return c, c.Endpoint != ""
}

// S3Lister lists an S3-compatible store.
type S3Lister struct{ mc *minio.Client }

// NewS3Lister dials (lazily; minio-go connects per request).
func NewS3Lister(c S3Config) (*S3Lister, error) {
	ep := strings.TrimPrefix(strings.TrimPrefix(c.Endpoint, "https://"), "http://")
	creds, err := objstore.S3Credentials(c.Credentials, c.AccessKeyID, c.SecretAccessKey, c.SessionToken)
	if err != nil {
		return nil, fmt.Errorf("s3 lister: %w", err)
	}
	mc, err := minio.New(ep, &minio.Options{
		Creds:  creds,
		Secure: c.UseTLS,
		Region: c.Region,
	})
	if err != nil {
		return nil, fmt.Errorf("s3 lister: %w", err)
	}
	return &S3Lister{mc: mc}, nil
}

// List implements ObjectLister with ListObjectsV2 StartAfter.
func (s *S3Lister) List(ctx context.Context, loc Location, startAfter string, max int) ([]ObjectInfo, bool, error) {
	if max <= 0 {
		max = 1000
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	opts := minio.ListObjectsOptions{Prefix: loc.Prefix, Recursive: true, MaxKeys: min(max, 1000)}
	if startAfter != "" {
		opts.StartAfter = startAfter
	}
	var out []ObjectInfo
	for obj := range s.mc.ListObjects(ctx, loc.Bucket, opts) {
		if obj.Err != nil {
			return nil, false, fmt.Errorf("list %s: %w", loc.URI(), obj.Err)
		}
		if len(out) == max {
			// One more exists: stop here and say so.
			return out, true, nil
		}
		out = append(out, ObjectInfo{Key: obj.Key, Size: obj.Size, ETag: strings.Trim(obj.ETag, `"`), LastModified: obj.LastModified})
	}
	return out, false, nil
}
