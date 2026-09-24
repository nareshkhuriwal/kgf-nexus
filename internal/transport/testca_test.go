// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package transport

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// testPKI is a throwaway certificate authority and the leaves it issued.
//
// Generated per test rather than checked in, because a committed key pair is a
// credential in the repository and an expiry date that eventually turns the suite
// red for a reason that has nothing to do with the code under test.
type testPKI struct {
	dir    string
	caFile string

	cert *x509.Certificate
	key  *ecdsa.PrivateKey
}

// newTestCA mints a CA into t.TempDir() and returns a handle for issuing leaves
// from it.
func newTestCA(t *testing.T, name string) *testPKI {
	t.Helper()
	dir := t.TempDir()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate CA key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial(t),
		Subject:               pkix.Name{CommonName: name},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("self-sign CA: %v", err)
	}
	ca, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse CA: %v", err)
	}

	p := &testPKI{dir: dir, caFile: filepath.Join(dir, name+"-ca.pem")}
	writePEM(t, p.caFile, "CERTIFICATE", der)
	p.cert, p.key = ca, key
	return p
}

// issue writes a leaf certificate and its key, signed by this CA, and returns the
// two paths in the order ServerOptions and DialOption read them (cert, key).
//
// dnsNames is what a client verifies the server against; an empty list is right
// for a client leaf, which is identified by its chain rather than by a name.
func (p *testPKI) issue(t *testing.T, name string, dnsNames []string, client bool) (certFile, keyFile string) {
	t.Helper()
	return p.issueValid(t, name, dnsNames, client, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
}

// issueValid is issue with an explicit validity window, for the expired and
// not-yet-valid cases the reload tests need.
func (p *testPKI) issueValid(t *testing.T, name string, dnsNames []string, client bool, notBefore, notAfter time.Time) (certFile, keyFile string) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate %s key: %v", name, err)
	}
	usage := x509.ExtKeyUsageServerAuth
	if client {
		usage = x509.ExtKeyUsageClientAuth
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial(t),
		Subject:      pkix.Name{CommonName: name},
		NotBefore:    notBefore,
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{usage},
		DNSNames:     dnsNames,
		// The tests dial 127.0.0.1 on an ephemeral port, and an IP peer is matched
		// against IPAddresses, never against DNSNames -- omitting this fails the
		// handshake with a name mismatch that looks like a code bug.
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, p.cert, &key.PublicKey, p.key)
	if err != nil {
		t.Fatalf("sign %s: %v", name, err)
	}

	certFile = filepath.Join(p.dir, name+".pem")
	keyFile = filepath.Join(p.dir, name+"-key.pem")
	writePEM(t, certFile, "CERTIFICATE", der)

	pkcs8, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal %s key: %v", name, err)
	}
	writePEM(t, keyFile, "PRIVATE KEY", pkcs8)
	return certFile, keyFile
}

func writePEM(t *testing.T, path, blockType string, der []byte) {
	t.Helper()
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: blockType, Bytes: der}), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func serial(t *testing.T) *big.Int {
	t.Helper()
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		t.Fatalf("serial: %v", err)
	}
	return n
}
