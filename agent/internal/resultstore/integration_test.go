//go:build integration

// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

// Integration tests for the result store. Behind the `integration` tag because
// they need a live S3-compatible endpoint; run with:
//
//	go test -tags integration ./agent/internal/resultstore/...
//
// They SKIP cleanly rather than fail when no endpoint is reachable: a test that
// fails for want of infrastructure teaches nobody anything.
package resultstore

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
)

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func newIntegrationClient(t *testing.T, inlineThreshold int64) *Client {
	t.Helper()
	c, err := New(Config{
		Endpoint:        envOr("NEXUS_S3_ENDPOINT", "localhost:9000"),
		Bucket:          envOr("NEXUS_S3_BUCKET", "nexus-test"),
		Namespace:       "itest",
		AccessKeyID:     envOr("NEXUS_S3_ACCESS_KEY_ID", "minioadmin"),
		SecretAccessKey: envOr("NEXUS_S3_SECRET_ACCESS_KEY", "minioadmin"),
		InlineThreshold: inlineThreshold,
		Log:             slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Skipf("cannot build a result store client (%v); skipping", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Reachable(ctx); err != nil {
		if err2 := c.EnsureBucket(ctx); err2 != nil {
			t.Skipf("result store unreachable (%v / %v); skipping", err, err2)
		}
	}
	if err := c.EnsureBucket(context.Background()); err != nil {
		t.Skipf("cannot ensure the test bucket (%v); skipping", err)
	}
	return c
}

// TestPutInlinesSmallResults: below the threshold, the payload is returned inline
// and never touches object storage, avoiding two round-trips for `{"rows": 42}`.
func TestPutInlinesSmallResults(t *testing.T) {
	c := newIntegrationClient(t, 1024)
	ctx := context.Background()

	payload := []byte(`{"rows":42}`)
	ref, err := c.Put(ctx, payload, "application/json")
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	inline, ok := ref.GetKind().(*nexusv1.ResultRef_Inline)
	if !ok {
		t.Fatalf("a %d-byte payload under a 1024-byte threshold was NOT inlined", len(payload))
	}
	if !bytes.Equal(inline.Inline, payload) {
		t.Errorf("inline payload = %q, want %q", inline.Inline, payload)
	}
	if ref.GetSizeBytes() != int64(len(payload)) {
		t.Errorf("size = %d, want %d", ref.GetSizeBytes(), len(payload))
	}

	// Round trip.
	got, err := c.Get(ctx, ref)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("round trip = %q, want %q", got, payload)
	}
}

// TestPutStoresLargeResultsContentAddressed: above the threshold the object goes to
// the store at <namespace>/<sha256>, and the digest makes the reference
// self-verifying.
func TestPutStoresLargeResultsContentAddressed(t *testing.T) {
	c := newIntegrationClient(t, 16)
	ctx := context.Background()

	payload := make([]byte, 4096)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}
	ref, err := c.Put(ctx, payload, "application/octet-stream")
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	blob, ok := ref.GetKind().(*nexusv1.ResultRef_Blob)
	if !ok {
		t.Fatal("a 4 KiB payload over a 16-byte threshold was inlined")
	}
	sum := sha256.Sum256(payload)
	want := hex.EncodeToString(sum[:])
	if blob.Blob.GetSha256() != want {
		t.Errorf("digest = %q, want %q", blob.Blob.GetSha256(), want)
	}
	if blob.Blob.GetKey() != "itest/"+want {
		t.Errorf("key = %q, want itest/%s (content-addressed under the namespace)",
			blob.Blob.GetKey(), want)
	}

	got, err := c.Get(ctx, ref)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Error("blob round trip returned different bytes")
	}
}

// TestIdenticalContentDedupes: content addressing means identical bytes are stored
// once, so a retry of a large output becomes a metadata call instead of a
// re-transfer. This is also the foundation memoization (§6.4) rests on.
func TestIdenticalContentDedupes(t *testing.T) {
	c := newIntegrationClient(t, 16)
	ctx := context.Background()

	payload := []byte("the same bytes, written twice by two different attempts")
	first, err := c.Put(ctx, payload, "text/plain")
	if err != nil {
		t.Fatal(err)
	}
	second, err := c.Put(ctx, payload, "text/plain")
	if err != nil {
		t.Fatal(err)
	}
	fb := first.GetKind().(*nexusv1.ResultRef_Blob).Blob
	sb := second.GetKind().(*nexusv1.ResultRef_Blob).Blob
	if fb.GetKey() != sb.GetKey() {
		t.Errorf("identical content produced different keys (%q vs %q); dedup is broken",
			fb.GetKey(), sb.GetKey())
	}
}

// TestDigestMismatchIsDetected: the reference IS the hash, so a mismatch means
// either corruption in transit or an object mutated in violation of the
// immutability rule. Feeding either to a task produces a silently WRONG result,
// which is worse than a failed one.
func TestDigestMismatchIsDetected(t *testing.T) {
	c := newIntegrationClient(t, 16)
	ctx := context.Background()

	payload := []byte("original content that will be misattributed")
	ref, err := c.Put(ctx, payload, "text/plain")
	if err != nil {
		t.Fatal(err)
	}
	blob := ref.GetKind().(*nexusv1.ResultRef_Blob).Blob
	// Claim a digest that does not match the stored bytes.
	tampered := &nexusv1.ResultRef{
		Kind: &nexusv1.ResultRef_Blob{Blob: &nexusv1.BlobRef{
			Bucket: blob.GetBucket(), Key: blob.GetKey(),
			Sha256: "0000000000000000000000000000000000000000000000000000000000000000",
		}},
	}
	if _, err := c.Get(ctx, tampered); err == nil {
		t.Fatal("a digest mismatch was NOT detected; a task would silently read the wrong data")
	}
}

// TestObjectsAreImmutable: a "retry" writing different content must land at a
// DIFFERENT key rather than mutating the original, which is what makes content
// addressing and memoization sound.
func TestObjectsAreImmutable(t *testing.T) {
	c := newIntegrationClient(t, 16)
	ctx := context.Background()

	firstRef, err := c.Put(ctx, []byte("attempt one output aaaaaaaaaaaaaaaa"), "text/plain")
	if err != nil {
		t.Fatal(err)
	}
	secondRef, err := c.Put(ctx, []byte("attempt two output bbbbbbbbbbbbbbbb"), "text/plain")
	if err != nil {
		t.Fatal(err)
	}
	fk := firstRef.GetKind().(*nexusv1.ResultRef_Blob).Blob.GetKey()
	sk := secondRef.GetKind().(*nexusv1.ResultRef_Blob).Blob.GetKey()
	if fk == sk {
		t.Fatal("different content landed at the same key; objects are not immutable")
	}
	// The first object must still read back unchanged.
	got, err := c.Get(ctx, firstRef)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "attempt one output aaaaaaaaaaaaaaaa" {
		t.Error("the first attempt's object was mutated by the second write")
	}
}

func TestGetInlineNeedsNoStore(t *testing.T) {
	c := newIntegrationClient(t, 1024)
	got, err := c.Get(context.Background(), &nexusv1.ResultRef{
		Kind: &nexusv1.ResultRef_Inline{Inline: []byte("local")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "local" {
		t.Errorf("got %q", got)
	}
}
