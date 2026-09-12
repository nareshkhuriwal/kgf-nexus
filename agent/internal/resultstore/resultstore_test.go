// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package resultstore

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

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

func TestStripScheme(t *testing.T) {
	for in, want := range map[string]string{
		"http://localhost:9000":    "localhost:9000",
		"https://s3.amazonaws.com": "s3.amazonaws.com",
		"minio:9000":               "minio:9000",
	} {
		if got := stripScheme(in); got != want {
			t.Errorf("stripScheme(%q) = %q, want %q", in, got, want)
		}
	}
}
