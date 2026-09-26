// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package resultstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
	"github.com/khuriwalgroup/kgf-nexus/internal/objstore/objstoretest"
)

// TestPutRefreshesOldDeduplicatedObjects pins F141: an identical object that
// is old enough to be a GC candidate is re-uploaded (refreshing its
// modification time), while a recent one is reused without a transfer.
func TestPutRefreshesOldDeduplicatedObjects(t *testing.T) {
	mem := objstoretest.NewMemory("results")
	var outcomes []string
	c, err := New(Config{Store: mem, Namespace: "ns", InlineThreshold: 1,
		OnPut: func(r string, _ int64, _ time.Duration) { outcomes = append(outcomes, r) }})
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("a payload larger than one byte")
	ctx := context.Background()
	ref, err := c.Put(ctx, payload, "")
	if err != nil {
		t.Fatal(err)
	}
	key := ref.GetBlob().GetKey()
	if _, err := c.Put(ctx, payload, ""); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour)
	mem.SetModified(key, old)
	if _, err := c.Put(ctx, payload, ""); err != nil {
		t.Fatal(err)
	}
	want := []string{PutUploaded, PutDeduplicated, PutUploaded}
	if strings.Join(outcomes, ",") != strings.Join(want, ",") {
		t.Fatalf("outcomes = %v; want %v", outcomes, want)
	}
	info, err := mem.Stat(ctx, key)
	if err != nil || !info.LastModified.After(old) {
		t.Fatalf("old object was not refreshed: %+v %v", info, err)
	}
	got, err := c.Get(ctx, ref)
	if err != nil || string(got) != string(payload) {
		t.Fatalf("get = %q, %v", got, err)
	}
}

// TestGetRefusesAnotherBucket: a client is bound to one bucket; a reference
// into another is an error that names both, never a silent read elsewhere.
func TestGetRefusesAnotherBucket(t *testing.T) {
	c, err := New(Config{Store: objstoretest.NewMemory("results"), Namespace: "ns"})
	if err != nil {
		t.Fatal(err)
	}
	ref := &nexusv1.ResultRef{Kind: &nexusv1.ResultRef_Blob{Blob: &nexusv1.BlobRef{Bucket: "elsewhere", Key: "ns/x"}}}
	if _, err := c.Get(context.Background(), ref); err == nil || !strings.Contains(err.Error(), "elsewhere") {
		t.Fatalf("err = %v", err)
	}
}

// TestKeyIsContentAddressedUnderTheNamespace: the key shape is <namespace>/<sha256>
// (ADR-0008). The namespace prefix is the TENANCY boundary — scoped worker
// credentials permit only that prefix, so a compromised worker cannot read another
// tenant's results — and the digest is what makes the reference self-verifying.
func TestKeyIsContentAddressedUnderTheNamespace(t *testing.T) {
	c := &Client{namespace: "team-ml", inlineMax: 64}
	sum := sha256.Sum256([]byte("hello"))
	digest := hex.EncodeToString(sum[:])

	got := c.Key(digest)
	want := "team-ml/" + digest
	if got != want {
		t.Errorf("Key() = %q, want %q", got, want)
	}
	if !strings.HasPrefix(got, "team-ml/") {
		t.Error("the key does not sit under the namespace prefix; credential scoping would not contain it")
	}

	// With no namespace the key is the bare digest, which keeps a single-tenant
	// deployment from growing a meaningless prefix.
	c2 := &Client{inlineMax: 64}
	if got := c2.Key(digest); got != digest {
		t.Errorf("Key() with no namespace = %q, want the bare digest", got)
	}
}

// TestInlineThresholdDefaults guards the ADR-0008 value: below 64 KiB the two
// round-trips to object storage cost more than the bytes are worth, and in a
// 50-node DAG of trivial outputs that overhead dominates.
func TestInlineThresholdDefaults(t *testing.T) {
	if DefaultInlineThresholdBytes != 64*1024 {
		t.Errorf("default inline threshold = %d, want 65536 (64 KiB per ADR-0008)",
			DefaultInlineThresholdBytes)
	}
	c, err := New(Config{Endpoint: "localhost:9000", Bucket: "b"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if c.InlineThreshold() != DefaultInlineThresholdBytes {
		t.Errorf("threshold = %d, want the default when the control plane supplies none",
			c.InlineThreshold())
	}

	// A control-plane-supplied value must win: the control plane owns the tuning
	// parameter, not the worker's config file.
	c2, err := New(Config{Endpoint: "localhost:9000", Bucket: "b", InlineThreshold: 128})
	if err != nil {
		t.Fatal(err)
	}
	if c2.InlineThreshold() != 128 {
		t.Errorf("threshold = %d, want the control-plane value 128", c2.InlineThreshold())
	}
}

func TestNewRequiresEndpointAndBucket(t *testing.T) {
	if _, err := New(Config{Bucket: "b"}); err == nil {
		t.Error("expected an error with no endpoint")
	}
	if _, err := New(Config{Endpoint: "localhost:9000"}); err == nil {
		t.Error("expected an error with no bucket")
	}
}

// TestCheckpointKeysAreConfinedToTheNamespacePrefix: a checkpoint write or read
// outside <namespace>/checkpoints/ is refused before any network call, so a bug
// in key construction cannot reach another tenant's objects (ADR-0032 §4).
func TestCheckpointKeysAreConfinedToTheNamespacePrefix(t *testing.T) {
	c := &Client{namespace: "team-ml"}
	for _, key := range []string{
		"team-ml/logs/a1/0",
		"other/checkpoints/k/a1/1",
		"team-ml/checkpoints/../../other/x",
		"team-mlx/checkpoints/k/a1/1",
	} {
		if err := c.PutCheckpointObject(t.Context(), key, strings.NewReader("x"), 1); !errors.Is(err, ErrCheckpointKeyOutsideNamespace) {
			t.Errorf("put %q: err = %v, want ErrCheckpointKeyOutsideNamespace", key, err)
		}
		if _, err := c.GetCheckpointObject(t.Context(), "", key, 10); !errors.Is(err, ErrCheckpointKeyOutsideNamespace) {
			t.Errorf("get %q: err = %v, want ErrCheckpointKeyOutsideNamespace", key, err)
		}
	}
	if !c.checkpointKeyAllowed("team-ml/checkpoints/k/a1/1") {
		t.Error("the attempt's own checkpoint key was refused")
	}
	if (&Client{}).checkpointKeyAllowed("checkpoints/k/a1/1") {
		t.Error("a client with no namespace must refuse every checkpoint key")
	}
}

// TestCheckpointRoundTripThroughTheStore: checkpoints go through the result
// store's backend interface (ADR-0027), bounded on read, with a missing object
// distinct from a failure and another bucket refused.
func TestCheckpointRoundTripThroughTheStore(t *testing.T) {
	c, err := New(Config{Store: objstoretest.NewMemory("results"), Namespace: "team-ml"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	key := "team-ml/checkpoints/k/a1/1"
	if _, err := c.GetCheckpointObject(ctx, "", key, 10); !errors.Is(err, ErrObjectNotFound) {
		t.Fatalf("missing: err = %v, want ErrObjectNotFound", err)
	}
	if err := c.PutCheckpointObject(ctx, key, strings.NewReader("state-1"), 7); err != nil {
		t.Fatal(err)
	}
	if b, err := c.GetCheckpointObject(ctx, "results", key, 7); err != nil || string(b) != "state-1" {
		t.Fatalf("get = %q, %v", b, err)
	}
	if _, err := c.GetCheckpointObject(ctx, "", key, 6); !errors.Is(err, ErrObjectTooLarge) {
		t.Fatalf("bound: err = %v, want ErrObjectTooLarge", err)
	}
	if _, err := c.GetCheckpointObject(ctx, "elsewhere", key, 10); err == nil || !strings.Contains(err.Error(), "elsewhere") {
		t.Fatalf("other bucket: err = %v", err)
	}
}
