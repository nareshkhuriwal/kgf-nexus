// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

// Package tests3 is the one place integration tests read their S3-compatible
// store from, the object-store counterpart of internal/testdb.
//
// It is imported only from _test.go files.
package tests3

import (
	"os"
	"strings"
	"testing"
)

// The ONLY variables integration tests read their object store from. They are
// test-only on purpose: the service spellings (NEXUS_S3_*, RESULT_STORE_*,
// NEXUS_RESULT_STORE_*, MINIO_ROOT_*) name a running deployment's store, and a
// test that creates and empties buckets must never pick one up by accident.
//
// There is deliberately no fallback endpoint. A compiled-in localhost:9000 with
// minioadmin/minioadmin silently points the suite at whatever happens to be
// listening there -- another project's MinIO, a dev stack's result bucket --
// and the suite then passes, skips or writes against the wrong thing.
const (
	EndpointVar  = "NEXUS_TEST_S3_ENDPOINT"          // host:port, or http(s)://host:port
	AccessKeyVar = "NEXUS_TEST_S3_ACCESS_KEY_ID"     // required when the endpoint is set
	SecretKeyVar = "NEXUS_TEST_S3_SECRET_ACCESS_KEY" // required when the endpoint is set
	BucketVar    = "NEXUS_TEST_S3_BUCKET"            // optional; each test names its own otherwise
	UseTLSVar    = "NEXUS_TEST_S3_USE_TLS"           // optional; "true" for https without a scheme
)

// SkipMarker prefixes every skip this package issues because the object store
// is missing. CI greps for it, exactly as it greps for testdb.SkipMarker: a
// skipped integration test looks like a passing one in `go test` output (F85).
const SkipMarker = "skipping integration test: no test S3 store"

// Config is the test object store.
type Config struct {
	// Endpoint as given, which may carry a scheme; see HostPort.
	Endpoint        string
	AccessKeyID     string
	SecretAccessKey string
	// Bucket is NEXUS_TEST_S3_BUCKET, or "" when unset.
	Bucket string
	UseTLS bool
}

// HostPort is Endpoint without an http:// or https:// scheme, the form the
// minio-go client takes.
func (c Config) HostPort() string {
	s := strings.TrimPrefix(c.Endpoint, "https://")
	return strings.TrimPrefix(s, "http://")
}

// BucketOr returns the configured bucket, or def when none is configured.
func (c Config) BucketOr(def string) string {
	if c.Bucket != "" {
		return c.Bucket
	}
	return def
}

// Get returns the test object store, or skips the test with SkipMarker when
// NEXUS_TEST_S3_ENDPOINT is unset. An endpoint without both credentials is a
// misconfiguration, not a missing store, so it FAILS rather than skips.
func Get(t testing.TB) Config {
	t.Helper()
	endpoint := os.Getenv(EndpointVar)
	if endpoint == "" {
		t.Skipf("%s: %s is not set", SkipMarker, EndpointVar)
	}
	c := Config{
		Endpoint:        endpoint,
		AccessKeyID:     os.Getenv(AccessKeyVar),
		SecretAccessKey: os.Getenv(SecretKeyVar),
		Bucket:          os.Getenv(BucketVar),
		UseTLS:          os.Getenv(UseTLSVar) == "true" || strings.HasPrefix(endpoint, "https://"),
	}
	if c.AccessKeyID == "" || c.SecretAccessKey == "" {
		t.Fatalf("%s is set but %s and %s are not both set", EndpointVar, AccessKeyVar, SecretKeyVar)
	}
	return c
}

// SkipUnreachable skips the test with SkipMarker because the store named by
// NEXUS_TEST_S3_ENDPOINT could not be reached.
func SkipUnreachable(t testing.TB, err error) {
	t.Helper()
	t.Skipf("%s: %s is set but unreachable: %v", SkipMarker, EndpointVar, err)
}
