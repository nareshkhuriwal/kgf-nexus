// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package secretbox

import (
	"bytes"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func rawKey(b byte) []byte { return bytes.Repeat([]byte{b}, 32) }

func TestSealOpenRoundTripAndContextBinding(t *testing.T) {
	b, err := New(rawKey(1))
	if err != nil {
		t.Fatal(err)
	}
	ctx := Context("webhook_triggers", "ns-a", "id-1")
	ct, err := b.Seal([]byte("s3cret"), ctx)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(ct, []byte("s3cret")) {
		t.Fatal("ciphertext contains the plaintext")
	}
	pt, err := b.Open(ct, ctx)
	if err != nil || string(pt) != "s3cret" {
		t.Fatalf("open = %q, %v", pt, err)
	}
	// A ciphertext moved to another row must not open there.
	if _, err := b.Open(ct, Context("webhook_triggers", "ns-a", "id-2")); !errors.Is(err, ErrOpen) {
		t.Fatalf("open under another row's context: %v, want ErrOpen", err)
	}
	ct[len(ct)-1] ^= 1
	if _, err := b.Open(ct, ctx); !errors.Is(err, ErrOpen) {
		t.Fatalf("tampered ciphertext opened: %v", err)
	}
}

func TestContextIsUnambiguous(t *testing.T) {
	if Context("a:b", "c") == Context("a", "b:c") {
		t.Fatal("contexts collide")
	}
}

func TestRotationOpensOldAndSealsNew(t *testing.T) {
	old, _ := New(rawKey(1))
	ct, _ := old.Seal([]byte("x"), "c")
	rotated, err := New(rawKey(2), rawKey(1))
	if err != nil {
		t.Fatal(err)
	}
	if pt, err := rotated.Open(ct, "c"); err != nil || string(pt) != "x" {
		t.Fatalf("rotated box cannot open old ciphertext: %v", err)
	}
	if !rotated.NeedsReseal(ct) {
		t.Fatal("old ciphertext should need a reseal")
	}
	fresh, _ := rotated.Seal([]byte("x"), "c")
	if rotated.NeedsReseal(fresh) {
		t.Fatal("fresh ciphertext should not need a reseal")
	}
	onlyNew, _ := New(rawKey(2))
	if _, err := onlyNew.Open(ct, "c"); !errors.Is(err, ErrOpen) {
		t.Fatalf("retired key still opens: %v", err)
	}
}

func TestNilBoxRefusesToSeal(t *testing.T) {
	var b *Box
	if _, err := b.Seal([]byte("x"), "c"); !errors.Is(err, ErrNoKey) {
		t.Fatalf("nil box sealed: %v", err)
	}
}

func TestFromEnv(t *testing.T) {
	env := map[string]string{}
	get := func(k string) string { return env[k] }
	if b, err := FromEnv(get); b != nil || err != nil {
		t.Fatalf("no key: %v %v", b, err)
	}
	env["NEXUS_SECRET_KEY"] = base64.StdEncoding.EncodeToString(rawKey(7))
	b, err := FromEnv(get)
	if err != nil || b == nil || b.KeyID() == "" {
		t.Fatalf("base64 key: %v %v", b, err)
	}
	env["NEXUS_SECRET_KEY"] = "too-short"
	if _, err := FromEnv(get); err == nil {
		t.Fatal("malformed key accepted")
	}
	dir := t.TempDir()
	f := filepath.Join(dir, "k")
	if err := os.WriteFile(f, []byte(base64.StdEncoding.EncodeToString(rawKey(3))+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env["NEXUS_SECRET_KEY"] = ""
	env["NEXUS_SECRET_KEY_FILE"] = f
	if b, err := FromEnv(get); err != nil || b == nil {
		t.Fatalf("file key: %v", err)
	}
}

func TestRefs(t *testing.T) {
	for _, bad := range []string{"", "plain", "env:", "file:relative", "file:/a/../etc/passwd", "env:NEXUS_SECRET_KEY", "env:DATABASE_URL"} {
		if ValidateRef(bad) == nil {
			t.Errorf("ValidateRef(%q) accepted", bad)
		}
	}
	got, err := ResolveRef("env:SLACK_HOOK", func(k string) string {
		if k == "SLACK_HOOK" {
			return "https://hooks.example/x"
		}
		return ""
	})
	if err != nil || string(got) != "https://hooks.example/x" {
		t.Fatalf("resolve env: %q %v", got, err)
	}
	if _, err := ResolveRef("env:MISSING", func(string) string { return "" }); err == nil {
		t.Fatal("empty env resolved")
	}
}

func TestHintNeverRevealsTheSecret(t *testing.T) {
	cases := map[string]string{
		"https://hooks.slack.com/services/T0/B0/XYZSECRET": "hooks.slack.com/…",
		"https://user:pw@example.com/x":                    "example.com/…",
		"short":                                            "…",
		"xoxb-1234567890-abcdefgh":                         "…efgh",
	}
	for in, want := range cases {
		if got := Hint(in); got != want {
			t.Errorf("Hint(%q) = %q, want %q", in, got, want)
		}
		if strings.Contains(Hint(in), "SECRET") || strings.Contains(Hint(in), "pw") {
			t.Errorf("hint leaks: %q", Hint(in))
		}
	}
}
