// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"errors"
	"strings"
	"testing"
)

// These are unit tests with no database: password hashing is pure computation,
// and the properties below are the ones a leak of the users table depends on.

func TestPasswordRoundTrip(t *testing.T) {
	const pw = "correct horse battery staple"
	encoded, err := HashPassword(pw)
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	if err := VerifyPassword(encoded, pw); err != nil {
		t.Errorf("the password that was hashed did not verify: %v", err)
	}
	if err := VerifyPassword(encoded, pw+"x"); !errors.Is(err, ErrPasswordMismatch) {
		t.Errorf("a wrong password returned %v, want ErrPasswordMismatch", err)
	}
}

// TestPasswordHashIsSalted is the property that makes a leak of this table
// survivable: two accounts with the same password must not share a verifier, or
// cracking one cracks every account that reused it and a precomputed table works.
func TestPasswordHashIsSalted(t *testing.T) {
	const pw = "same-password-both-times"
	a, err := HashPassword(pw)
	if err != nil {
		t.Fatalf("hash a: %v", err)
	}
	b, err := HashPassword(pw)
	if err != nil {
		t.Fatalf("hash b: %v", err)
	}
	if a == b {
		t.Error("hashing the same password twice produced identical verifiers; the salt is not random")
	}
	// Both must still verify: a salt that broke verification would be worse.
	if err := VerifyPassword(a, pw); err != nil {
		t.Errorf("verifier a did not verify: %v", err)
	}
	if err := VerifyPassword(b, pw); err != nil {
		t.Errorf("verifier b did not verify: %v", err)
	}
}

// TestPasswordHashIsNotTheSHA256UsedForAPIKeys guards the distinction the brief
// and gateway_apikeys.go both rest on. If someone ever "simplifies" passwords onto
// HashAPIKey, the users table becomes offline-crackable at GPU speed -- and it
// would still pass a naive round-trip test, which is why this asserts it directly.
func TestPasswordHashIsNotTheSHA256UsedForAPIKeys(t *testing.T) {
	const pw = "a-human-chosen-password"
	encoded, err := HashPassword(pw)
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	if encoded == HashAPIKey(pw) {
		t.Fatal("password hashing produced a plain SHA-256; passwords are low-entropy " +
			"and need a memory-hard KDF (see gateway_apikeys.go for why API keys do not)")
	}
	if !strings.HasPrefix(encoded, "$argon2id$") {
		t.Errorf("verifier %q is not a PHC argon2id string; the parameters must travel "+
			"with the hash so the cost can be raised later", firstN(encoded, 24))
	}
}

// TestPasswordVerifierCarriesItsParameters is what makes raising the cost possible
// without invalidating existing passwords.
func TestPasswordVerifierCarriesItsParameters(t *testing.T) {
	encoded, err := HashPassword("parameters-please")
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	params, salt, hash, err := parsePHC(encoded)
	if err != nil {
		t.Fatalf("parse own output: %v", err)
	}
	if params.memory != argonMemory || params.time != argonTime {
		t.Errorf("recovered m=%d,t=%d, want m=%d,t=%d", params.memory, params.time, argonMemory, argonTime)
	}
	if len(salt) != argonSaltLen {
		t.Errorf("salt is %d bytes, want %d", len(salt), argonSaltLen)
	}
	if len(hash) != argonKeyLen {
		t.Errorf("hash is %d bytes, want %d", len(hash), argonKeyLen)
	}
}

func TestEmptyPasswordIsRefused(t *testing.T) {
	// An empty password must not produce a usable verifier: an account anyone can
	// log into would be indistinguishable from a real one.
	if _, err := HashPassword(""); err == nil {
		t.Error("hashing an empty password succeeded; that creates an account anyone can use")
	}
}

// TestMalformedVerifierIsNotAMismatch keeps a data-integrity fault from being
// reported as a credential decision. Telling an operator "wrong password" when the
// row is corrupt sends them to reset a password that was never the problem.
func TestMalformedVerifierIsNotAMismatch(t *testing.T) {
	cases := map[string]string{
		"empty":              "",
		"not PHC":            "just-a-string",
		"too few fields":     "$argon2id$v=19$m=65536,t=1,p=4$onlysalt",
		"wrong algorithm":    "$argon2i$v=19$m=65536,t=1,p=4$c2FsdHNhbHQ$aGFzaGhhc2g",
		"bad base64 salt":    "$argon2id$v=19$m=65536,t=1,p=4$!!!!$aGFzaGhhc2g",
		"zero cost":          "$argon2id$v=19$m=0,t=0,p=0$c2FsdHNhbHQ$aGFzaGhhc2g",
		"unparseable params": "$argon2id$v=19$m=abc,t=1,p=4$c2FsdHNhbHQ$aGFzaGhhc2g",
	}
	for name, encoded := range cases {
		err := VerifyPassword(encoded, "anything")
		if errors.Is(err, ErrPasswordMismatch) {
			t.Errorf("%s: reported as a password mismatch; a corrupt verifier is a data "+
				"fault and must be distinguishable", name)
		}
		if !errors.Is(err, ErrPasswordHashMalformed) {
			t.Errorf("%s: got %v, want ErrPasswordHashMalformed", name, err)
		}
	}
}

// TestPasswordNeedsRehash covers the upgrade path. Without it, a password hashed
// under old parameters keeps its old strength forever and raising the cost
// protects only new accounts.
func TestPasswordNeedsRehash(t *testing.T) {
	current, err := HashPassword("current-parameters")
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	if PasswordNeedsRehash(current) {
		t.Error("a hash made with the current parameters wants a rehash; every login would re-hash forever")
	}

	// A verifier made with weaker memory must be flagged.
	weak := "$argon2id$v=19$m=1024,t=1,p=1$c2FsdHNhbHRzYWx0c2E$aGFzaGhhc2hoYXNoaGFzaGhhc2hoYXNoaGE"
	if !PasswordNeedsRehash(weak) {
		t.Error("a hash made with 1MB of memory was not flagged for rehash")
	}

	// A malformed verifier fails toward "replace it", which is the safe direction.
	if !PasswordNeedsRehash("garbage") {
		t.Error("a malformed verifier was not flagged for rehash")
	}
}

// TestGenerateSecretIsHighEntropyAndPrefixed covers the property that justifies
// hashing session and scoped tokens with SHA-256 rather than a KDF.
func TestGenerateSecretIsHighEntropyAndPrefixed(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		s, err := GenerateSecret("nxsess")
		if err != nil {
			t.Fatalf("generate secret: %v", err)
		}
		if !strings.HasPrefix(s, "nxsess_") {
			t.Fatalf("secret %q lacks its prefix; a secret scanner relies on it", firstN(s, 12))
		}
		body := strings.TrimPrefix(s, "nxsess_")
		// 32 random bytes in unpadded base64url is 43 characters.
		if len(body) != 43 {
			t.Fatalf("secret body is %d chars, want 43 (32 bytes); less entropy would make "+
				"the SHA-256 at-rest hash the wrong choice", len(body))
		}
		if seen[s] {
			t.Fatal("GenerateSecret returned a duplicate; the source is not random")
		}
		seen[s] = true
	}
}

func firstN(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
