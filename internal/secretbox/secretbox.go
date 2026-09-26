// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

// Package secretbox seals small secrets (webhook signing keys, channel tokens) at
// rest with AES-256-GCM, and resolves secret REFERENCES (env:NAME, file:/path)
// for deployments that keep secrets out of the database entirely (ADR-0028).
//
// THE PROPERTIES THAT MATTER:
//
//   - Nothing is ever stored in clear. With no key configured, Seal fails with
//     ErrNoKey and the caller refuses the write; it never falls back to storing
//     plaintext.
//   - A ciphertext is bound to where it lives. The caller passes an associated
//     context (table, namespace, row id) that GCM authenticates, so a ciphertext
//     copied from one row onto another -- by a bug, or by someone with UPDATE on
//     the table -- fails to open instead of silently becoming that row's secret.
//   - Keys rotate without a flag day. Every ciphertext names its key id, and a
//     Box holds one PRIMARY key (seals) plus any number of previous keys (open
//     only). Rotation is: add the new key as primary, keep the old as previous,
//     re-seal (rotating a trigger's secret does it), then drop the old key.
package secretbox

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
)

// ErrNoKey means no sealing key is configured.
var ErrNoKey = errors.New("secretbox: no secret key configured (set NEXUS_SECRET_KEY or NEXUS_SECRET_KEY_FILE)")

// ErrOpen means a ciphertext could not be opened: wrong key, wrong context, or
// tampered bytes. Deliberately one error for all three -- distinguishing them
// would tell an attacker which part to change.
var ErrOpen = errors.New("secretbox: ciphertext could not be opened")

const (
	version  = byte(1)
	keyIDLen = 8
)

// Box seals and opens.
type Box struct {
	primary  *key
	previous []*key
}

type key struct {
	id   [keyIDLen]byte
	aead cipher.AEAD
}

func newKey(raw []byte) (*key, error) {
	if len(raw) != 32 {
		return nil, fmt.Errorf("secretbox: key must be 32 bytes (AES-256), got %d", len(raw))
	}
	block, err := aes.NewCipher(raw)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(raw)
	k := &key{aead: aead}
	copy(k.id[:], sum[:keyIDLen])
	return k, nil
}

// New builds a Box from a primary key and optional previous keys, each 32 raw
// bytes.
func New(primary []byte, previous ...[]byte) (*Box, error) {
	p, err := newKey(primary)
	if err != nil {
		return nil, err
	}
	b := &Box{primary: p}
	for _, raw := range previous {
		k, err := newKey(raw)
		if err != nil {
			return nil, fmt.Errorf("previous key: %w", err)
		}
		b.previous = append(b.previous, k)
	}
	return b, nil
}

// KeyID reports the primary key's id (hex), for logs and a readiness report.
func (b *Box) KeyID() string {
	if b == nil || b.primary == nil {
		return ""
	}
	return hex.EncodeToString(b.primary.id[:])
}

// FromEnv builds a Box from NEXUS_SECRET_KEY (base64 or hex, 32 bytes) or
// NEXUS_SECRET_KEY_FILE, plus NEXUS_SECRET_KEY_PREVIOUS / _FILE for rotation.
// It returns (nil, nil) when no key is configured: that is a supported state in
// which sealing is refused, not a startup failure. A key that IS configured but
// malformed is an error, because silently running without it would turn every
// sealed secret into a refusal at the worst moment.
func FromEnv(getenv func(string) string) (*Box, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	primary, err := loadKey(getenv, "NEXUS_SECRET_KEY")
	if err != nil {
		return nil, err
	}
	if primary == nil {
		return nil, nil
	}
	prev, err := loadKey(getenv, "NEXUS_SECRET_KEY_PREVIOUS")
	if err != nil {
		return nil, err
	}
	var prevs [][]byte
	if prev != nil {
		prevs = append(prevs, prev)
	}
	return New(primary, prevs...)
}

func loadKey(getenv func(string) string, name string) ([]byte, error) {
	v := strings.TrimSpace(getenv(name))
	if f := strings.TrimSpace(getenv(name + "_FILE")); f != "" {
		if v != "" {
			return nil, fmt.Errorf("secretbox: set %s or %s_FILE, not both", name, name)
		}
		b, err := os.ReadFile(f)
		if err != nil {
			return nil, fmt.Errorf("secretbox: read %s_FILE: %w", name, err)
		}
		v = strings.TrimSpace(string(b))
	}
	if v == "" {
		return nil, nil
	}
	raw, err := DecodeKey(v)
	if err != nil {
		return nil, fmt.Errorf("secretbox: %s: %w", name, err)
	}
	return raw, nil
}

// DecodeKey accepts a 32-byte key as standard/URL base64 or hex.
func DecodeKey(v string) ([]byte, error) {
	for _, dec := range []func(string) ([]byte, error){
		hex.DecodeString,
		base64.StdEncoding.DecodeString,
		base64.RawStdEncoding.DecodeString,
		base64.URLEncoding.DecodeString,
		base64.RawURLEncoding.DecodeString,
	} {
		if b, err := dec(v); err == nil && len(b) == 32 {
			return b, nil
		}
	}
	return nil, errors.New("key must be 32 bytes encoded as base64 or hex (generate one with: openssl rand -base64 32)")
}

// Seal encrypts plaintext bound to context. Layout:
// version(1) | key id(8) | nonce(12) | ciphertext+tag.
func (b *Box) Seal(plaintext []byte, context string) ([]byte, error) {
	if b == nil || b.primary == nil {
		return nil, ErrNoKey
	}
	nonce := make([]byte, b.primary.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("secretbox: nonce: %w", err)
	}
	out := make([]byte, 0, 1+keyIDLen+len(nonce)+len(plaintext)+b.primary.aead.Overhead())
	out = append(out, version)
	out = append(out, b.primary.id[:]...)
	out = append(out, nonce...)
	return b.primary.aead.Seal(out, nonce, plaintext, []byte(context)), nil
}

// Open decrypts a ciphertext sealed under context by the primary or a previous key.
func (b *Box) Open(ciphertext []byte, context string) ([]byte, error) {
	if b == nil || b.primary == nil {
		return nil, ErrNoKey
	}
	if len(ciphertext) < 1+keyIDLen || ciphertext[0] != version {
		return nil, ErrOpen
	}
	var id [keyIDLen]byte
	copy(id[:], ciphertext[1:1+keyIDLen])
	for _, k := range append([]*key{b.primary}, b.previous...) {
		if k.id != id {
			continue
		}
		ns := k.aead.NonceSize()
		body := ciphertext[1+keyIDLen:]
		if len(body) < ns+k.aead.Overhead() {
			return nil, ErrOpen
		}
		pt, err := k.aead.Open(nil, body[:ns], body[ns:], []byte(context))
		if err != nil {
			return nil, ErrOpen
		}
		return pt, nil
	}
	return nil, fmt.Errorf("%w: sealed under key %s, which is neither the primary nor a previous key",
		ErrOpen, hex.EncodeToString(id[:]))
}

// NeedsReseal reports whether a ciphertext was sealed under a key other than the
// primary, so a caller can re-seal it opportunistically during a rotation.
func (b *Box) NeedsReseal(ciphertext []byte) bool {
	if b == nil || b.primary == nil || len(ciphertext) < 1+keyIDLen {
		return false
	}
	var id [keyIDLen]byte
	copy(id[:], ciphertext[1:1+keyIDLen])
	return id != b.primary.id
}

// Context builds the associated-data string for a row. Every component is
// length-prefixed so ("a:b", "c") and ("a", "b:c") cannot collide.
func Context(parts ...string) string {
	var sb strings.Builder
	for _, p := range parts {
		fmt.Fprintf(&sb, "%d:%s|", len(p), p)
	}
	return sb.String()
}

// ── references ──────────────────────────────────────────────────────────────

// ErrBadRef is returned for a reference in an unsupported form.
var ErrBadRef = errors.New("secretbox: a secret reference must be env:NAME or file:/absolute/path")

// ValidateRef checks a reference's FORM without resolving it. The gateway calls
// this; only the process that uses the secret resolves it, because that process's
// environment is the one the reference names.
func ValidateRef(ref string) error {
	switch {
	case strings.HasPrefix(ref, "env:"):
		name := strings.TrimPrefix(ref, "env:")
		if name == "" || strings.ContainsAny(name, " =\x00") {
			return ErrBadRef
		}
		// Refuse the platform's own secrets: a reference is a way to NAME a
		// channel secret, not a way to exfiltrate the database password into a
		// webhook body.
		upper := strings.ToUpper(name)
		for _, deny := range []string{"NEXUS_SECRET_KEY", "DATABASE_URL", "NEXUS_POSTGRES", "PG", "NEXUS_NATS", "NEXUS_S3_SECRET", "RESULT_STORE_SECRET"} {
			if strings.HasPrefix(upper, deny) {
				return fmt.Errorf("secretbox: env:%s names a platform credential and cannot be used as a channel secret", name)
			}
		}
		return nil
	case strings.HasPrefix(ref, "file:"):
		path := strings.TrimPrefix(ref, "file:")
		if !strings.HasPrefix(path, "/") || strings.Contains(path, "..") {
			return ErrBadRef
		}
		return nil
	}
	return ErrBadRef
}

// ResolveRef reads a reference in this process's environment.
func ResolveRef(ref string, getenv func(string) string) ([]byte, error) {
	if err := ValidateRef(ref); err != nil {
		return nil, err
	}
	if getenv == nil {
		getenv = os.Getenv
	}
	if name, ok := strings.CutPrefix(ref, "env:"); ok {
		v := getenv(name)
		if v == "" {
			return nil, fmt.Errorf("secretbox: %s is empty or unset in this service's environment", ref)
		}
		return []byte(v), nil
	}
	path := strings.TrimPrefix(ref, "file:")
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("secretbox: read %s: %w", ref, err)
	}
	return []byte(strings.TrimRight(string(b), "\r\n")), nil
}

// Hint renders what a read may show of a secret: nothing for short values, the
// host for a URL, otherwise the last four characters.
func Hint(secret string) string {
	s := strings.TrimSpace(secret)
	if strings.HasPrefix(s, "https://") || strings.HasPrefix(s, "http://") {
		rest := s[strings.Index(s, "//")+2:]
		if i := strings.IndexAny(rest, "/?#"); i >= 0 {
			rest = rest[:i]
		}
		if at := strings.LastIndex(rest, "@"); at >= 0 {
			rest = rest[at+1:]
		}
		return rest + "/…"
	}
	if len(s) < 12 {
		return "…"
	}
	return "…" + s[len(s)-4:]
}
