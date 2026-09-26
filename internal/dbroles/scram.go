// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package dbroles

import (
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"regexp"
)

// ScramIterations is PostgreSQL's own default (scram_iterations, 4096). The
// verifier records its iteration count, so a later change here needs no
// coordination with the server.
const ScramIterations = 4096

const scramSaltBytes = 16

// passwordRe is the alphabet a runtime password may use: RFC 3986's unreserved
// characters. The chart embeds the password in a postgres:// URL through
// Kubernetes env expansion, which cannot percent-encode, and SASLprep (which
// the server applies to non-ASCII passwords) is the identity on this set, so
// the verifier computed here is exactly the one the server would compute.
var passwordRe = regexp.MustCompile(`^[A-Za-z0-9._~-]+$`)

// MinPasswordLength is the shortest runtime password accepted. Terraform
// generates 32 alphanumerics; 16 is the floor the kgf-nexus module already
// enforces for the admin password.
const MinPasswordLength = 16

// ValidatePassword refuses a password the chart could not carry in a URL, or
// one too short to be generated rather than typed.
func ValidatePassword(p string) error {
	if len(p) < MinPasswordLength {
		return fmt.Errorf("password is %d characters; at least %d are required", len(p), MinPasswordLength)
	}
	if !passwordRe.MatchString(p) {
		return fmt.Errorf("password may contain only A-Z a-z 0-9 . _ ~ - (it is embedded unescaped in a postgres:// URL)")
	}
	return nil
}

// ScramSHA256Verifier returns the SCRAM-SHA-256 verifier PostgreSQL stores in
// pg_authid.rolpassword for password (RFC 5802 / RFC 7677):
//
//	SCRAM-SHA-256$<iterations>:<salt>$<StoredKey>:<ServerKey>
//
// Computing it here, and sending ALTER ROLE ... PASSWORD '<verifier>', means
// the plaintext never reaches the server: not the statement log (log_statement
// = ddl is common on managed servers), not pgaudit, not pg_stat_statements.
// psql's \password does the same, but only interactively.
func ScramSHA256Verifier(password string, salt []byte, iterations int) (string, error) {
	if iterations < 1 {
		return "", fmt.Errorf("iterations must be positive")
	}
	if len(salt) == 0 {
		return "", fmt.Errorf("salt must not be empty")
	}
	salted, err := pbkdf2.Key(sha256.New, password, salt, iterations, sha256.Size)
	if err != nil {
		return "", fmt.Errorf("pbkdf2: %w", err)
	}
	clientKey := hmacSHA256(salted, "Client Key")
	storedKey := sha256.Sum256(clientKey)
	serverKey := hmacSHA256(salted, "Server Key")
	b64 := base64.StdEncoding.EncodeToString
	return fmt.Sprintf("SCRAM-SHA-256$%d:%s$%s:%s",
		iterations, b64(salt), b64(storedKey[:]), b64(serverKey)), nil
}

// NewVerifier validates password and returns its verifier with a fresh random
// salt.
func NewVerifier(password string) (string, error) {
	if err := ValidatePassword(password); err != nil {
		return "", err
	}
	salt := make([]byte, scramSaltBytes)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("salt: %w", err)
	}
	return ScramSHA256Verifier(password, salt, ScramIterations)
}

func hmacSHA256(key []byte, msg string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(msg))
	return m.Sum(nil)
}
