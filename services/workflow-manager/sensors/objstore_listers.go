// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package sensors

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/khuriwalgroup/kgf-nexus/internal/objstore"
)

// Native Azure Blob and GCS listers (ADR-0028 §4, ADR-0027).
//
// Both are the result store's own backends (internal/objstore), used through
// the same conformance-tested interface, so a sensor on azblob:// or gcs://
// authenticates exactly as the platform's store does -- workload identity,
// managed identity or a shared key on Azure; Application Default Credentials
// (GKE Workload Identity) on GCS -- and lists with the same ordering and
// not-found semantics the artifact GC relies on.
//
// Schemes. "azblob" and "gcs" are the backend names operators already use in
// NEXUS_RESULT_STORE_BACKEND; "az" and "gs" are the spellings of the Azure and
// Google CLIs (gs://bucket/...), accepted as aliases so a URI copied from
// either tool works unchanged. In an Azure URI the host is the CONTAINER; the
// storage account comes from configuration, never from the URI, so a sensor
// definition cannot point the platform's identity at an arbitrary account.

// Scheme names.
const (
	SchemeAzBlob = "azblob"
	SchemeAz     = "az"
	SchemeGCS    = "gcs"
	SchemeGS     = "gs"
)

// maxOpenStores bounds the per-bucket store cache. Each store holds an HTTP
// client; sensors name buckets freely, so an unbounded cache would grow with
// every typo a tenant ever saved.
const maxOpenStores = 256

// StoreOpener opens the objstore.Store for one bucket (container).
type StoreOpener func(ctx context.Context, bucket string) (objstore.Store, error)

// StoreLister is an ObjectLister over objstore stores, one per bucket.
type StoreLister struct {
	scheme string
	open   StoreOpener

	mu     sync.Mutex
	stores map[string]objstore.Store
}

// NewStoreLister returns a lister for scheme that opens stores with open.
func NewStoreLister(scheme string, open StoreOpener) *StoreLister {
	return &StoreLister{scheme: scheme, open: open, stores: map[string]objstore.Store{}}
}

func (l *StoreLister) store(ctx context.Context, bucket string) (objstore.Store, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if s, ok := l.stores[bucket]; ok {
		return s, nil
	}
	s, err := l.open(ctx, bucket)
	if err != nil {
		return nil, fmt.Errorf("%s lister: open %s: %w", l.scheme, bucket, err)
	}
	if len(l.stores) >= maxOpenStores {
		// Drop them all rather than track recency: reopening is cheap (no
		// network until the first request) and this only happens past the cap.
		l.stores = map[string]objstore.Store{}
	}
	l.stores[bucket] = s
	return s, nil
}

// List implements ObjectLister: up to max objects under loc.Prefix whose keys
// sort strictly after startAfter, and whether more may follow. A bucket that
// does not exist is an error from the backend, never an empty page.
func (l *StoreLister) List(ctx context.Context, loc Location, startAfter string, max int) ([]ObjectInfo, bool, error) {
	if max <= 0 {
		max = 1000
	}
	s, err := l.store(ctx, loc.Bucket)
	if err != nil {
		return nil, false, err
	}
	var out []ObjectInfo
	more := false
	err = objstore.ListAfter(ctx, s, loc.Prefix, startAfter, func(o objstore.ObjectInfo) error {
		if len(out) == max {
			more = true
			return objstore.ErrStop
		}
		// ETag is not part of the objstore contract; the sensor keys an
		// observation on (key, size, last-modified), which it always has.
		out = append(out, ObjectInfo{Key: o.Key, Size: o.Size, LastModified: o.LastModified})
		return nil
	})
	if err != nil {
		return nil, false, fmt.Errorf("list %s: %w", loc.URI(), err)
	}
	return out, more, nil
}

// pick returns the first non-empty value among keys.
func pick(getenv func(string) string, keys ...string) string {
	for _, k := range keys {
		if v := strings.TrimSpace(getenv(k)); v != "" {
			return v
		}
	}
	return ""
}

// resultStoreIs reports whether the platform's own result store is backend.
func resultStoreIs(getenv func(string) string, backend string) bool {
	b := strings.ToLower(strings.TrimSpace(getenv(objstore.EnvBackend)))
	if b == "" {
		b = objstore.BackendS3
	}
	return b == backend
}

// AzBlobConfigFromEnv reads NEXUS_SENSOR_AZURE_STORAGE_{ACCOUNT,AUTH,KEY},
// NEXUS_SENSOR_AZURE_CLIENT_ID and NEXUS_SENSOR_AZBLOB_ENDPOINT, falling back
// to the result store's NEXUS_AZURE_* (and its endpoint) only when the result
// store IS azblob, so a default Azure deployment watches its own account with
// no new settings. ok=false means azblob sensors are not configured. The
// Bucket field is left empty: it is the container each sensor names.
func AzBlobConfigFromEnv(getenv func(string) string) (objstore.Config, bool, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	own := resultStoreIs(getenv, objstore.BackendAzBlob)
	fallback := func(sensorKey, storeKey string) string {
		if v := pick(getenv, sensorKey); v != "" {
			return v
		}
		if own {
			return pick(getenv, storeKey)
		}
		return ""
	}
	c := objstore.Config{
		Backend:  objstore.BackendAzBlob,
		Endpoint: fallback("NEXUS_SENSOR_AZBLOB_ENDPOINT", objstore.EnvEndpoint),
		Azure: objstore.AzureConfig{
			Account:    fallback("NEXUS_SENSOR_AZURE_STORAGE_ACCOUNT", objstore.EnvAzureAccount),
			Auth:       strings.ToLower(fallback("NEXUS_SENSOR_AZURE_STORAGE_AUTH", objstore.EnvAzureAuth)),
			AccountKey: fallback("NEXUS_SENSOR_AZURE_STORAGE_KEY", objstore.EnvAzureKey),
			ClientID:   fallback("NEXUS_SENSOR_AZURE_CLIENT_ID", objstore.EnvAzureClient),
		},
	}
	if c.Azure.Account == "" && c.Endpoint == "" {
		return c, false, nil
	}
	// Validate with a placeholder container: the modes and the account are
	// checked at boot, the container per sensor.
	probe := c
	probe.Bucket = "probe"
	if err := probe.Validate(); err != nil {
		return c, true, fmt.Errorf("azblob sensors: %w", err)
	}
	return c, true, nil
}

// GCSConfigFromEnv reads NEXUS_SENSOR_GCS_{AUTH,ENDPOINT,ENABLED}, falling
// back to the result store's NEXUS_GCS_AUTH and endpoint when the result store
// IS gcs. Application Default Credentials need no setting at all, so a
// deployment whose store is elsewhere opts in with NEXUS_SENSOR_GCS_ENABLED.
func GCSConfigFromEnv(getenv func(string) string) (objstore.Config, bool, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	own := resultStoreIs(getenv, objstore.BackendGCS)
	c := objstore.Config{
		Backend:  objstore.BackendGCS,
		Endpoint: pick(getenv, "NEXUS_SENSOR_GCS_ENDPOINT"),
		GCS:      objstore.GCSConfig{Auth: strings.ToLower(pick(getenv, "NEXUS_SENSOR_GCS_AUTH"))},
	}
	if own {
		if c.Endpoint == "" {
			c.Endpoint = pick(getenv, objstore.EnvEndpoint)
		}
		if c.GCS.Auth == "" {
			c.GCS.Auth = strings.ToLower(pick(getenv, objstore.EnvGCSAuth))
		}
	}
	enabled := own || c.Endpoint != "" || c.GCS.Auth != "" ||
		strings.EqualFold(pick(getenv, "NEXUS_SENSOR_GCS_ENABLED"), "true")
	if !enabled {
		return c, false, nil
	}
	probe := c
	probe.Bucket = "probe"
	if err := probe.Validate(); err != nil {
		return c, true, fmt.Errorf("gcs sensors: %w", err)
	}
	if c.GCS.Auth == objstore.GCSAuthNone && c.Endpoint == "" {
		return c, true, errors.New("gcs sensors: auth none is unauthenticated and only for an emulator; set NEXUS_SENSOR_GCS_ENDPOINT")
	}
	return c, true, nil
}

// openerFor opens cfg for one bucket.
func openerFor(cfg objstore.Config) StoreOpener {
	return func(ctx context.Context, bucket string) (objstore.Store, error) {
		c := cfg
		c.Bucket = bucket
		return objstore.Open(ctx, c)
	}
}

// RegisterFromEnv installs a lister for every object store the environment
// configures: s3 (S3ConfigFromEnv), azblob/az and gcs/gs. A malformed setting
// is an error, so a service fails at boot instead of every sensor failing at
// its first poll.
func RegisterFromEnv(r *Registry, getenv func(string) string) error {
	if getenv == nil {
		getenv = os.Getenv
	}
	if s3cfg, ok := S3ConfigFromEnv(getenv); ok {
		r.Register("s3", func(context.Context) (ObjectLister, error) { return NewS3Lister(s3cfg) })
	}
	az, ok, err := AzBlobConfigFromEnv(getenv)
	if err != nil {
		return err
	}
	if ok {
		l := NewStoreLister(SchemeAzBlob, openerFor(az))
		for _, s := range []string{SchemeAzBlob, SchemeAz} {
			r.Register(s, func(context.Context) (ObjectLister, error) { return l, nil })
		}
	}
	gcs, ok, err := GCSConfigFromEnv(getenv)
	if err != nil {
		return err
	}
	if ok {
		l := NewStoreLister(SchemeGCS, openerFor(gcs))
		for _, s := range []string{SchemeGCS, SchemeGS} {
			r.Register(s, func(context.Context) (ObjectLister, error) { return l, nil })
		}
	}
	return nil
}
