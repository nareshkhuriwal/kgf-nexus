// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// The chart renders DATABASE_URL as
//
//	postgres://<user>:<pw>@<host>:<port>/<db>?sslmode=verify-full&sslrootcert=/etc/nexus/postgres-ca/ca.crt&<params>
//
// and DSN hands it to pgx unchanged. These tests pin that pgx turns it into
// what verify-full promises: the mounted bundle as the ONLY trust anchors and
// the DSN host as the name the certificate must carry. If a pgx upgrade ever
// changed that, the chart's verify-full would quietly become weaker.
func TestVerifyFullDSNPinsRootsAndHostName(t *testing.T) {
	ca := writeCA(t)
	t.Setenv("DATABASE_URL", "postgres://nexus_scheduler_login:pw@db.example.internal:5432/nexus?sslmode=verify-full&sslrootcert="+ca+"&connect_timeout=10")
	dsn, err := resolveDSN(os.Getenv, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("pgx refused the chart's DSN shape: %v", err)
	}
	tc := cfg.ConnConfig.TLSConfig
	if tc == nil {
		t.Fatal("verify-full produced no TLS config")
	}
	if tc.InsecureSkipVerify {
		t.Fatal("verify-full skips verification")
	}
	if tc.ServerName != "db.example.internal" {
		t.Fatalf("ServerName = %q, want the DSN host (host name verification)", tc.ServerName)
	}
	if tc.RootCAs == nil || !tc.RootCAs.Equal(poolOf(t, ca)) {
		t.Fatal("RootCAs is not exactly the sslrootcert bundle")
	}
	if len(cfg.ConnConfig.Fallbacks) != 0 {
		t.Fatalf("verify-full has %d fallbacks; a plaintext or unverified fallback would defeat it", len(cfg.ConnConfig.Fallbacks))
	}
	if cfg.ConnConfig.User != "nexus_scheduler_login" || cfg.ConnConfig.ConnectTimeout != 10*time.Second {
		t.Fatalf("user/params lost: %q %v", cfg.ConnConfig.User, cfg.ConnConfig.ConnectTimeout)
	}
}

// A missing or unreadable bundle is a startup error, never a silent
// downgrade to the system roots or to no verification.
func TestVerifyFullWithMissingBundleFails(t *testing.T) {
	_, err := pgxpool.ParseConfig("postgres://u:p@db:5432/nexus?sslmode=verify-full&sslrootcert=" + filepath.Join(t.TempDir(), "absent.crt"))
	if err == nil || !strings.Contains(err.Error(), "sslrootcert") {
		t.Fatalf("missing bundle: err = %v", err)
	}
}

func writeCA(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test-ca"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "ca.crt")
	if err := os.WriteFile(p, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func poolOf(t *testing.T, path string) *x509.CertPool {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	p := x509.NewCertPool()
	p.AppendCertsFromPEM(b)
	return p
}
