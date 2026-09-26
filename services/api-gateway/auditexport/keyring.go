// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

// Package auditexport delivers the audit trail to external systems (ADR-0025
// §2): HMAC-signed webhook batches from a durable per-sink cursor, and the
// retention purge loop. The signing secrets it needs are stored encrypted
// under a key only the gateway holds.
package auditexport

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Environment variables for the sink-secret encryption key: a base64 (std or
// URL) encoding of exactly 32 random bytes, inline or in a file. The _FILE
// form is the production one (a mounted Kubernetes Secret).
const (
	EnvSinkKey     = "NEXUS_AUDIT_SINK_KEY"
	EnvSinkKeyFile = "NEXUS_AUDIT_SINK_KEY_FILE"
)

// ErrNoKey means sinks cannot be created or delivered: no key is configured.
var ErrNoKey = errors.New("audit sinks are disabled: NEXUS_AUDIT_SINK_KEY(_FILE) is not configured")

// Keyring encrypts sink signing secrets with AES-256-GCM. The ciphertext is
// bound (additional data) to the sink's namespace, so a row copied into
// another tenant's sink does not decrypt.
type Keyring struct {
	aead cipher.AEAD
	id   string
}

// KeyringFromEnv loads the key; (nil, nil) when none is configured.
func KeyringFromEnv(getenv func(string) string) (*Keyring, error) {
	raw := strings.TrimSpace(getenv(EnvSinkKey))
	if f := getenv(EnvSinkKeyFile); f != "" {
		if raw != "" {
			return nil, fmt.Errorf("set only one of %s and %s", EnvSinkKey, EnvSinkKeyFile)
		}
		b, err := os.ReadFile(f)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", EnvSinkKeyFile, err)
		}
		raw = strings.TrimSpace(string(b))
	}
	if raw == "" {
		return nil, nil
	}
	key, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		key, err = base64.RawURLEncoding.DecodeString(strings.TrimRight(raw, "="))
	}
	if err != nil || len(key) != 32 {
		return nil, fmt.Errorf("%s must be base64 of exactly 32 bytes (openssl rand -base64 32)", EnvSinkKey)
	}
	return NewKeyring(key)
}

// NewKeyring builds a keyring over a 32-byte key.
func NewKeyring(key []byte) (*Keyring, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(append([]byte("nexus-audit-sink-key:"), key...))
	return &Keyring{aead: aead, id: hex.EncodeToString(sum[:8])}, nil
}

// KeyID names the key without revealing it, so a rotated key is detectable.
func (k *Keyring) KeyID() string { return k.id }

// Seal encrypts plaintext for namespace.
func (k *Keyring) Seal(namespace string, plaintext []byte) (ciphertext, nonce []byte, err error) {
	if k == nil {
		return nil, nil, ErrNoKey
	}
	nonce = make([]byte, k.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, nil, err
	}
	return k.aead.Seal(nil, nonce, plaintext, []byte(namespace)), nonce, nil
}

// Open decrypts a secret sealed for namespace under this key.
func (k *Keyring) Open(namespace, keyID string, ciphertext, nonce []byte) ([]byte, error) {
	if k == nil {
		return nil, ErrNoKey
	}
	if keyID != k.id {
		return nil, fmt.Errorf("sink secret was sealed under key %s, the gateway holds %s; rotate the sink's secret", keyID, k.id)
	}
	return k.aead.Open(nil, nonce, ciphertext, []byte(namespace))
}

// NewSecret returns a fresh signing secret, 32 random bytes, base64url.
func NewSecret() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "whsec_" + base64.RawURLEncoding.EncodeToString(b), nil
}

// SignatureHeader is the webhook signature header.
const SignatureHeader = "X-Nexus-Signature"

// Sign returns the X-Nexus-Signature value for body sent at unix time ts:
// "t=<ts>,v1=<hex HMAC-SHA256(secret, "<ts>." + body)>". Signing the
// timestamp lets a receiver reject replays older than its tolerance.
func Sign(secret []byte, ts int64, body []byte) string {
	m := hmac.New(sha256.New, secret)
	m.Write([]byte(strconv.FormatInt(ts, 10)))
	m.Write([]byte("."))
	m.Write(body)
	return "t=" + strconv.FormatInt(ts, 10) + ",v1=" + hex.EncodeToString(m.Sum(nil))
}

// Verify checks a signature header against body, for receivers and tests.
// tolerance bounds |now - t| in seconds; 0 disables the check.
func Verify(secret []byte, header string, body []byte, now, tolerance int64) error {
	var ts int64
	var sig string
	for _, part := range strings.Split(header, ",") {
		k, v, _ := strings.Cut(strings.TrimSpace(part), "=")
		switch k {
		case "t":
			ts, _ = strconv.ParseInt(v, 10, 64)
		case "v1":
			sig = v
		}
	}
	if ts == 0 || sig == "" {
		return errors.New("malformed signature header")
	}
	if tolerance > 0 && (now-ts > tolerance || ts-now > tolerance) {
		return errors.New("signature timestamp outside tolerance")
	}
	want := Sign(secret, ts, body)
	if !hmac.Equal([]byte(want), []byte("t="+strconv.FormatInt(ts, 10)+",v1="+sig)) {
		return errors.New("signature mismatch")
	}
	return nil
}
