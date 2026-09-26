// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

// Package objstoretest is the conformance suite every objstore backend must
// pass, and the environment contract integration tests find their emulators
// through. Imported only from _test.go files.
//
// The suite is the executable form of the semantics in the objstore package
// comment. A backend that passes it can be put behind the agent, the log
// reader and the GC without any of them knowing which one it is.
package objstoretest

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/khuriwalgroup/kgf-nexus/internal/objstore"
)

// SkipMarker prefixes every skip issued because an emulator is not configured,
// matching tests3.SkipMarker's role: CI greps for it (F85).
const SkipMarker = "skipping integration test: no test object store"

// Environment for the emulators. Test-only names, for the reason tests3 gives:
// a suite that creates and empties buckets must never pick up a deployment's
// NEXUS_RESULT_STORE_* by accident.
const (
	AzEndpointVar = "NEXUS_TEST_AZBLOB_ENDPOINT" // e.g. http://127.0.0.1:10000/devstoreaccount1
	AzAccountVar  = "NEXUS_TEST_AZBLOB_ACCOUNT"  // e.g. devstoreaccount1
	AzKeyVar      = "NEXUS_TEST_AZBLOB_KEY"      // the account key
	GCSEndpoint   = "NEXUS_TEST_GCS_ENDPOINT"    // e.g. http://127.0.0.1:4443
)

// AzureConfig returns an azblob config for bucket from the test environment,
// or skips.
func AzureConfig(t testing.TB, bucket string) objstore.Config {
	t.Helper()
	ep := os.Getenv(AzEndpointVar)
	if ep == "" {
		t.Skipf("%s: %s is not set", SkipMarker, AzEndpointVar)
	}
	acct, key := os.Getenv(AzAccountVar), os.Getenv(AzKeyVar)
	if acct == "" || key == "" {
		t.Fatalf("%s is set but %s and %s are not both set", AzEndpointVar, AzAccountVar, AzKeyVar)
	}
	return objstore.Config{
		Backend: objstore.BackendAzBlob, Endpoint: ep, Bucket: bucket, PartSize: 5 << 20,
		Azure: objstore.AzureConfig{Account: acct, Auth: objstore.AzureAuthSharedKey, AccountKey: key},
	}
}

// GCSConfig returns a gcs config for bucket from the test environment, or skips.
func GCSConfig(t testing.TB, bucket string) objstore.Config {
	t.Helper()
	ep := os.Getenv(GCSEndpoint)
	if ep == "" {
		t.Skipf("%s: %s is not set", SkipMarker, GCSEndpoint)
	}
	return objstore.Config{
		Backend: objstore.BackendGCS, Endpoint: ep, Bucket: bucket, PartSize: 5 << 20,
		GCS: objstore.GCSConfig{Auth: objstore.GCSAuthNone, ProjectID: "nexus-test"},
	}
}

// UniqueBucket returns a bucket/container name valid on all three backends
// (lower-case, digits and dashes, 3-63 characters).
func UniqueBucket(prefix string) string {
	b := make([]byte, 5)
	_, _ = rand.Read(b)
	return strings.ToLower(prefix) + "-" + hex.EncodeToString(b)
}

// Opener builds a store for a bucket name. The suite uses it to open the
// bucket under test and a bucket that must not exist.
type Opener func(t testing.TB, bucket string) objstore.Store

// Run runs the conformance suite against a fresh bucket created by open.
func Run(t *testing.T, open Opener) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	bucket := UniqueBucket("nexus-w3g-conf")
	s := open(t, bucket)
	if err := s.CreateBucket(ctx); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	if err := s.CreateBucket(ctx); err != nil {
		t.Fatalf("CreateBucket must be idempotent: %v", err)
	}
	t.Cleanup(func() { emptyBucket(t, s) })

	t.Run("BucketExists", func(t *testing.T) {
		ok, err := s.BucketExists(ctx)
		if err != nil || !ok {
			t.Fatalf("BucketExists = %v, %v; want true", ok, err)
		}
		missing := open(t, UniqueBucket("nexus-w3g-absent"))
		ok, err = missing.BucketExists(ctx)
		if err != nil || ok {
			t.Fatalf("BucketExists(absent) = %v, %v; want false, nil (only a positive answer may be false)", ok, err)
		}
	})

	t.Run("RoundTrip", func(t *testing.T) {
		body := []byte(`{"rows": 42}`)
		key := "ns/roundtrip/" + digest(body)
		mustPut(t, s, key, body, objstore.PutOptions{ContentType: "application/json"})
		info, err := s.Stat(ctx, key)
		if err != nil {
			t.Fatalf("stat: %v", err)
		}
		if info.Key != key || info.Size != int64(len(body)) {
			t.Fatalf("stat = %+v; want key %s size %d", info, key, len(body))
		}
		if info.ContentType != "application/json" {
			t.Errorf("content type = %q; want application/json", info.ContentType)
		}
		if age := time.Since(info.LastModified); age < -5*time.Minute || age > 5*time.Minute {
			t.Errorf("LastModified %v is not recent (the GC grace period depends on it)", info.LastModified)
		}
		if got := mustGet(t, s, key, 0, -1); !bytes.Equal(got, body) {
			t.Fatalf("get = %q; want %q", got, body)
		}
	})

	t.Run("DefaultContentType", func(t *testing.T) {
		mustPut(t, s, "ns/ct/default", []byte("x"), objstore.PutOptions{})
		info, err := s.Stat(ctx, "ns/ct/default")
		if err != nil {
			t.Fatal(err)
		}
		if info.ContentType != "application/octet-stream" {
			t.Errorf("content type = %q; want application/octet-stream", info.ContentType)
		}
	})

	t.Run("NotFoundIsDistinct", func(t *testing.T) {
		if _, err := s.Stat(ctx, "ns/missing/object"); !errors.Is(err, objstore.ErrNotFound) {
			t.Fatalf("stat missing: %v; want ErrNotFound", err)
		}
		if _, err := s.Get(ctx, "ns/missing/object", 0, -1); !errors.Is(err, objstore.ErrNotFound) {
			t.Fatalf("get missing: %v; want ErrNotFound (before any read)", err)
		}
		if _, err := s.Get(ctx, "ns/missing/object", 5, 10); !errors.Is(err, objstore.ErrNotFound) {
			t.Fatalf("ranged get missing: %v; want ErrNotFound", err)
		}
		if err := s.Delete(ctx, "ns/missing/object"); err != nil {
			t.Fatalf("delete missing: %v; want nil (idempotent)", err)
		}
	})

	t.Run("RangeReads", func(t *testing.T) {
		body := []byte("0123456789abcdefghijklmnopqrstuvwxyz")
		key := "ns/logs/attempt/log.ndjson"
		mustPut(t, s, key, body, objstore.PutOptions{ContentType: "application/x-ndjson"})
		for _, c := range []struct {
			off, n int64
			want   string
		}{
			{0, -1, string(body)},
			{0, 10, "0123456789"},
			{10, 6, "abcdef"},
			{30, -1, "uvwxyz"},
			{35, 1, "z"},
			{7, 0, ""},
		} {
			if got := mustGet(t, s, key, c.off, c.n); string(got) != c.want {
				t.Errorf("get(%d,%d) = %q; want %q", c.off, c.n, got, c.want)
			}
		}
		b, err := objstore.ReadAll(ctx, s, key, 10, 6, 1<<20)
		if err != nil || string(b) != "abcdef" {
			t.Errorf("ReadAll(10,6) = %q, %v", b, err)
		}
		if _, err := objstore.ReadAll(ctx, s, key, 0, -1, 8); err == nil {
			t.Errorf("ReadAll past its bound must fail rather than truncate")
		}
	})

	t.Run("IfAbsent", func(t *testing.T) {
		key := "ns/cond/object"
		mustPut(t, s, key, []byte("first"), objstore.PutOptions{IfAbsent: true})
		err := s.Put(ctx, key, bytes.NewReader([]byte("second")), 6, objstore.PutOptions{IfAbsent: true})
		if !errors.Is(err, objstore.ErrExists) {
			t.Fatalf("second IfAbsent put: %v; want ErrExists", err)
		}
		if got := mustGet(t, s, key, 0, -1); string(got) != "first" {
			t.Fatalf("IfAbsent overwrote the object: %q", got)
		}
	})

	t.Run("OverwriteIsIdempotent", func(t *testing.T) {
		key := "ns/logs/attempt/000001.ndjson"
		mustPut(t, s, key, []byte("chunk"), objstore.PutOptions{})
		mustPut(t, s, key, []byte("chunk"), objstore.PutOptions{})
		if got := mustGet(t, s, key, 0, -1); string(got) != "chunk" {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("MultipartLarge", func(t *testing.T) {
		// More than two parts at the suite's 5 MiB part size: exercises S3
		// multipart, Azure staged blocks and GCS resumable chunks.
		body := make([]byte, 12<<20+123)
		_, _ = rand.Read(body)
		key := "ns/" + digest(body)
		mustPut(t, s, key, body, objstore.PutOptions{})
		got := mustGet(t, s, key, 0, -1)
		if digest(got) != digest(body) {
			t.Fatalf("multipart round trip corrupted the object (%d bytes back)", len(got))
		}
		tail := mustGet(t, s, key, int64(len(body))-100, 100)
		if !bytes.Equal(tail, body[len(body)-100:]) {
			t.Fatalf("range read across the last part is wrong")
		}
	})

	t.Run("UnknownSize", func(t *testing.T) {
		body := bytes.Repeat([]byte("n"), 6<<20+7)
		key := "ns/unknown-size"
		if err := s.Put(ctx, key, io.MultiReader(bytes.NewReader(body)), -1, objstore.PutOptions{}); err != nil {
			t.Fatalf("put of unknown size: %v", err)
		}
		info, err := s.Stat(ctx, key)
		if err != nil || info.Size != int64(len(body)) {
			t.Fatalf("stat = %+v, %v; want size %d", info, err, len(body))
		}
	})

	t.Run("FailedPutLeavesNothing", func(t *testing.T) {
		for _, size := range []int64{64 << 10, 11 << 20} {
			key := fmt.Sprintf("ns/failed/%d", size)
			r := &failingReader{remaining: size - 1000}
			err := s.Put(ctx, key, r, size, objstore.PutOptions{})
			if err == nil {
				t.Fatalf("put of a body that fails mid-stream (%d bytes) succeeded", size)
			}
			if _, err := s.Stat(ctx, key); !errors.Is(err, objstore.ErrNotFound) {
				t.Fatalf("a failed %d-byte put left something under the key: stat err=%v", size, err)
			}
		}
	})

	t.Run("KeyValidation", func(t *testing.T) {
		for _, key := range []string{"", "/abs", "a//b", "dir/", "a/../b"} {
			if err := s.Put(ctx, key, bytes.NewReader([]byte("x")), 1, objstore.PutOptions{}); err == nil {
				t.Errorf("put accepted invalid key %q", key)
			}
		}
	})

	t.Run("ListPrefixOrderAndStop", func(t *testing.T) {
		const n = 1100 // past S3's and GCS's 1000-object page
		want := make([]string, 0, n)
		for i := 0; i < n; i++ {
			k := fmt.Sprintf("list/p/%05d", i)
			want = append(want, k)
		}
		putMany(t, s, want)
		mustPut(t, s, "list/other/x", []byte("x"), objstore.PutOptions{})
		mustPut(t, s, "list/pp", []byte("x"), objstore.PutOptions{})

		var got []string
		err := s.List(ctx, "list/p/", func(o objstore.ObjectInfo) error {
			if o.Size != 1 || o.LastModified.IsZero() {
				return fmt.Errorf("object %s: size %d, modified %v", o.Key, o.Size, o.LastModified)
			}
			got = append(got, o.Key)
			return nil
		})
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(got) != n {
			t.Fatalf("listed %d objects under list/p/; want %d", len(got), n)
		}
		if !sort.StringsAreSorted(got) {
			t.Fatalf("listing is not in lexical key order")
		}

		count := 0
		err = s.List(ctx, "list/", func(objstore.ObjectInfo) error {
			count++
			if count == 10 {
				return objstore.ErrStop
			}
			return nil
		})
		if err != nil || count != 10 {
			t.Fatalf("ErrStop: count=%d err=%v; want 10, nil", count, err)
		}
		boom := errors.New("boom")
		if err := s.List(ctx, "list/", func(objstore.ObjectInfo) error { return boom }); !errors.Is(err, boom) {
			t.Fatalf("callback error not returned: %v", err)
		}
		cctx, ccancel := context.WithCancel(ctx)
		ccancel()
		if err := s.List(cctx, "list/", func(objstore.ObjectInfo) error { return nil }); err == nil {
			t.Fatalf("list with a cancelled context succeeded")
		}
	})

	t.Run("Delete", func(t *testing.T) {
		mustPut(t, s, "ns/del/x", []byte("x"), objstore.PutOptions{})
		if err := s.Delete(ctx, "ns/del/x"); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Stat(ctx, "ns/del/x"); !errors.Is(err, objstore.ErrNotFound) {
			t.Fatalf("stat after delete: %v", err)
		}
		if err := s.Delete(ctx, ""); err == nil {
			t.Fatalf("delete of the empty key must be refused")
		}
	})
}

type failingReader struct{ remaining int64 }

func (f *failingReader) Read(p []byte) (int, error) {
	if f.remaining <= 0 {
		return 0, errors.New("objstoretest: injected body failure")
	}
	if int64(len(p)) > f.remaining {
		p = p[:f.remaining]
	}
	for i := range p {
		p[i] = 'f'
	}
	f.remaining -= int64(len(p))
	return len(p), nil
}

func digest(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func mustPut(t testing.TB, s objstore.Store, key string, body []byte, o objstore.PutOptions) {
	t.Helper()
	if err := s.Put(context.Background(), key, bytes.NewReader(body), int64(len(body)), o); err != nil {
		t.Fatalf("put %s: %v", key, err)
	}
}

func mustGet(t testing.TB, s objstore.Store, key string, off, n int64) []byte {
	t.Helper()
	rc, err := s.Get(context.Background(), key, off, n)
	if err != nil {
		t.Fatalf("get %s [%d,%d]: %v", key, off, n, err)
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read %s: %v", key, err)
	}
	return b
}

func putMany(t testing.TB, s objstore.Store, keys []string) {
	t.Helper()
	sem := make(chan struct{}, 32)
	errs := make(chan error, len(keys))
	for _, k := range keys {
		sem <- struct{}{}
		go func(k string) {
			defer func() { <-sem }()
			errs <- s.Put(context.Background(), k, bytes.NewReader([]byte("x")), 1, objstore.PutOptions{})
		}(k)
	}
	for range keys {
		if err := <-errs; err != nil {
			t.Fatalf("put: %v", err)
		}
	}
}

func emptyBucket(t testing.TB, s objstore.Store) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	var keys []string
	_ = s.List(ctx, "", func(o objstore.ObjectInfo) error { keys = append(keys, o.Key); return nil })
	for _, k := range keys {
		_ = s.Delete(ctx, k)
	}
}
