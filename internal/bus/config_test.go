// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package bus

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"log/slog"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nkeys"

	"github.com/khuriwalgroup/kgf-nexus/internal/transport"
)

// every variable ConfigFromEnv reads, so each test starts from a known-empty
// environment regardless of the shell it runs in.
var allNATSEnv = []string{
	EnvURL, EnvCreds, EnvNKeySeedFile, EnvUser, EnvPassword, EnvPasswordFile,
	EnvRequireAuth, EnvTLS, EnvTLSCAFile, EnvTLSCertFile, EnvTLSKeyFile, EnvTLSServerName,
	transport.EnvCACert, transport.EnvClientCert, transport.EnvClientKey,
	"NEXUS_REQUIRE_TLS", "NEXUS_ENV",
}

func clearEnv(t *testing.T) {
	t.Helper()
	for _, k := range allNATSEnv {
		t.Setenv(k, "")
	}
}

func writeFile(t *testing.T, name, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// userSeedFile writes a fresh user seed and returns (path, seed, public key).
func userSeedFile(t *testing.T) (string, string, string) {
	t.Helper()
	kp, err := nkeys.CreateUser()
	if err != nil {
		t.Fatal(err)
	}
	seed, _ := kp.Seed()
	pub, _ := kp.PublicKey()
	return writeFile(t, "user.nk", string(seed)+"\n"), string(seed), pub
}

func mustConfig(t *testing.T) Config {
	t.Helper()
	c, err := ConfigFromEnv("nexus-agent")
	if err != nil {
		t.Fatalf("ConfigFromEnv: %v", err)
	}
	return c
}

func mustReject(t *testing.T, wantInErr string, secrets ...string) {
	t.Helper()
	_, err := ConfigFromEnv("nexus-agent")
	if err == nil {
		t.Fatalf("ConfigFromEnv accepted a configuration it must reject (want an error mentioning %q)", wantInErr)
	}
	if !strings.Contains(err.Error(), wantInErr) {
		t.Errorf("error %q does not mention %q", err, wantInErr)
	}
	for _, s := range secrets {
		if s != "" && strings.Contains(err.Error(), s) {
			t.Errorf("error message leaks a secret: %q", err)
		}
	}
}

func TestConfigFromEnvDisabledAndBasics(t *testing.T) {
	clearEnv(t)
	c := mustConfig(t)
	if c.Enabled() {
		t.Error("an empty NEXUS_NATS_URL must disable the bus")
	}
	// A disabled bus is valid even where auth is required: nothing connects.
	t.Setenv(EnvRequireAuth, "1")
	if _, err := ConfigFromEnv("x"); err != nil {
		t.Errorf("disabled bus rejected: %v", err)
	}

	clearEnv(t)
	seedPath, _, _ := userSeedFile(t)
	t.Setenv(EnvURL, "  nats://nats:4222 ")
	t.Setenv(EnvNKeySeedFile, seedPath)
	c = mustConfig(t)
	if !c.Enabled() || c.URL != "nats://nats:4222" || c.Name != "nexus-agent" || c.AuthMethod() != AuthNKey {
		t.Errorf("ConfigFromEnv = %+v (auth %s)", c, c.AuthMethod())
	}
	if c.InboxPrefix != "_INBOX.nexus-agent" {
		t.Errorf("InboxPrefix = %q, want _INBOX.nexus-agent (deploy/ permissions grant exactly that)", c.InboxPrefix)
	}
	if c.TLS.Enabled {
		t.Error("TLS must stay off unless asked for")
	}
}

func TestConfigRejectsUnsupportedScheme(t *testing.T) {
	clearEnv(t)
	t.Setenv(EnvURL, "ws://nats:8080")
	mustReject(t, "not supported")
}

// TestRequireAuthFailsClosed: the production guard. A production-like
// NEXUS_ENV, or NEXUS_NATS_REQUIRE_AUTH, refuses a credential-less broker
// connection; an explicit false overrides NEXUS_ENV and is reported.
func TestRequireAuthFailsClosed(t *testing.T) {
	for _, env := range []string{"production", "prod", "staging", "PRODUCTION"} {
		clearEnv(t)
		t.Setenv(EnvURL, "nats://nats:4222")
		t.Setenv("NEXUS_ENV", env)
		mustReject(t, EnvNKeySeedFile)
	}

	clearEnv(t)
	t.Setenv(EnvURL, "nats://nats:4222")
	t.Setenv(EnvRequireAuth, "true")
	mustReject(t, "no NATS credentials")

	clearEnv(t)
	t.Setenv(EnvURL, "nats://nats:4222")
	t.Setenv("NEXUS_ENV", "dev")
	if c := mustConfig(t); c.RequireAuth || c.AuthMethod() != AuthNone {
		t.Errorf("dev must keep the credential-less path: %+v", c)
	}
	if RequireAuthOverridden() {
		t.Error("nothing was overridden in dev")
	}

	clearEnv(t)
	t.Setenv(EnvURL, "nats://nats:4222")
	t.Setenv("NEXUS_ENV", "production")
	t.Setenv(EnvRequireAuth, "false")
	if c := mustConfig(t); c.RequireAuth {
		t.Error("NEXUS_NATS_REQUIRE_AUTH=false must override NEXUS_ENV")
	}
	if !RequireAuthOverridden() {
		t.Error("the override must be reportable so callers can WARN")
	}
}

func TestOneCredentialMethodOnly(t *testing.T) {
	seedPath, seed, _ := userSeedFile(t)
	cases := map[string]map[string]string{
		"nkey+creds":    {EnvNKeySeedFile: seedPath, EnvCreds: "/x.creds"},
		"nkey+password": {EnvNKeySeedFile: seedPath, EnvUser: "u", EnvPassword: "p"},
		"nkey+url":      {EnvNKeySeedFile: seedPath, EnvURL: "nats://u:p@127.0.0.1:4222"},
	}
	for name, env := range cases {
		t.Run(name, func(t *testing.T) {
			clearEnv(t)
			t.Setenv(EnvURL, "nats://127.0.0.1:4222")
			for k, v := range env {
				t.Setenv(k, v)
			}
			mustReject(t, "more than one NATS credential method", seed, "p@")
		})
	}
}

func TestNKeySeedValidation(t *testing.T) {
	clearEnv(t)
	t.Setenv(EnvURL, "nats://nats:4222")

	t.Setenv(EnvNKeySeedFile, filepath.Join(t.TempDir(), "missing.nk"))
	mustReject(t, EnvNKeySeedFile)

	junk := "not-a-seed-but-maybe-a-secret"
	t.Setenv(EnvNKeySeedFile, writeFile(t, "junk.nk", junk))
	mustReject(t, "does not contain an NKey seed", junk)

	// An ACCOUNT seed is a real NKey but the wrong kind: the server would
	// reject it as a user, so reject it here with a message that says why.
	akp, _ := nkeys.CreateAccount()
	aseed, _ := akp.Seed()
	t.Setenv(EnvNKeySeedFile, writeFile(t, "acct.nk", string(aseed)))
	mustReject(t, "not a USER seed", string(aseed))

	// The armoured form nsc and `nats auth nkey gen` write is accepted too.
	ukp, _ := nkeys.CreateUser()
	useed, _ := ukp.Seed()
	armoured := "-----BEGIN USER NKEY SEED-----\n" + string(useed) + "\n------END USER NKEY SEED------\n"
	t.Setenv(EnvNKeySeedFile, writeFile(t, "armoured.nk", armoured))
	if c := mustConfig(t); c.AuthMethod() != AuthNKey {
		t.Errorf("armoured seed: auth = %s", c.AuthMethod())
	}
}

func TestCredsFileValidation(t *testing.T) {
	clearEnv(t)
	t.Setenv(EnvURL, "nats://nats:4222")
	ukp, _ := nkeys.CreateUser()
	useed, _ := ukp.Seed()
	creds := "-----BEGIN NATS USER JWT-----\neyJ0eXAiOiJKV1QiLCJhbGciOiJlZDI1NTE5LW5rZXkifQ.e30.sig\n------END NATS USER JWT------\n\n" +
		"-----BEGIN USER NKEY SEED-----\n" + string(useed) + "\n------END USER NKEY SEED------\n"
	t.Setenv(EnvCreds, writeFile(t, "svc.creds", creds))
	if c := mustConfig(t); c.AuthMethod() != AuthCreds {
		t.Errorf("auth = %s", c.AuthMethod())
	}

	t.Setenv(EnvCreds, writeFile(t, "bad.creds", "garbage "+string(useed)))
	mustReject(t, EnvCreds, string(useed))
}

func TestPasswordAuth(t *testing.T) {
	const pw = "hunter2-very-secret"

	clearEnv(t)
	t.Setenv(EnvURL, "nats://127.0.0.1:4222")
	t.Setenv(EnvPassword, pw)
	mustReject(t, "without "+EnvUser, pw)

	clearEnv(t)
	t.Setenv(EnvURL, "nats://127.0.0.1:4222")
	t.Setenv(EnvUser, "event-service")
	mustReject(t, "without "+EnvPassword)

	clearEnv(t)
	t.Setenv(EnvURL, "nats://127.0.0.1:4222")
	t.Setenv(EnvUser, "event-service")
	t.Setenv(EnvPassword, pw)
	t.Setenv(EnvPasswordFile, writeFile(t, "pw", pw))
	mustReject(t, "both", pw)

	// The password crosses the wire in CONNECT: refused in plaintext to a
	// remote broker, allowed to loopback or over TLS.
	clearEnv(t)
	t.Setenv(EnvURL, "nats://nats.internal:4222")
	t.Setenv(EnvUser, "event-service")
	t.Setenv(EnvPassword, pw)
	mustReject(t, "plaintext", pw)

	t.Setenv(EnvURL, "nats://localhost:4222,nats://[::1]:4223")
	if c := mustConfig(t); c.AuthMethod() != AuthPassword {
		t.Errorf("loopback password: auth = %s", c.AuthMethod())
	}

	clearEnv(t)
	t.Setenv(EnvURL, "tls://nats.internal:4222")
	t.Setenv(EnvUser, "event-service")
	t.Setenv(EnvPasswordFile, writeFile(t, "pw", pw+"\n"))
	c := mustConfig(t)
	if !c.TLS.Enabled || c.AuthMethod() != AuthPassword {
		t.Errorf("password file over tls:// = %+v", c)
	}

	t.Setenv(EnvPasswordFile, writeFile(t, "empty", "\n"))
	mustReject(t, "is empty")

	// Userinfo in the URL is a password too.
	clearEnv(t)
	t.Setenv(EnvURL, "nats://svc:"+pw+"@nats.internal:4222")
	mustReject(t, "plaintext", pw)
}

func TestTLSSelection(t *testing.T) {
	ca, cert, key := testPKI(t)

	clearEnv(t)
	t.Setenv(EnvURL, "tls://nats:4222")
	if c := mustConfig(t); !c.TLS.Enabled {
		t.Error("a tls:// URL must enable TLS")
	}

	// The gRPC material is reused when no NATS-specific file is set.
	clearEnv(t)
	t.Setenv(EnvURL, "nats://nats:4222")
	t.Setenv(EnvTLS, "1")
	t.Setenv(transport.EnvCACert, ca)
	t.Setenv(transport.EnvClientCert, cert)
	t.Setenv(transport.EnvClientKey, key)
	c := mustConfig(t)
	if !c.TLS.Enabled || c.TLS.CAFile != ca || c.TLS.CertFile != cert || c.TLS.KeyFile != key {
		t.Errorf("fallback to NEXUS_TLS_* not applied: %+v", c.TLS)
	}

	// ...but only when TLS to NATS is on: gRPC TLS alone does not flip NATS.
	t.Setenv(EnvTLS, "")
	if c := mustConfig(t); c.TLS.Enabled {
		t.Error("NEXUS_TLS_* alone must not turn NATS TLS on")
	}

	// A NATS-specific file enables TLS and wins over the gRPC one.
	clearEnv(t)
	t.Setenv(EnvURL, "nats://nats:4222")
	t.Setenv(EnvTLSCAFile, ca)
	t.Setenv(transport.EnvCACert, "/nope")
	if c := mustConfig(t); !c.TLS.Enabled || c.TLS.CAFile != ca {
		t.Errorf("NATS CA not preferred: %+v", c.TLS)
	}

	clearEnv(t)
	t.Setenv(EnvURL, "tls://nats:4222")
	t.Setenv(EnvTLS, "false")
	mustReject(t, "pick one")

	clearEnv(t)
	t.Setenv(EnvURL, "nats://nats:4222")
	t.Setenv(EnvTLSCertFile, cert)
	mustReject(t, "half-configured")

	clearEnv(t)
	t.Setenv(EnvURL, "nats://nats:4222")
	t.Setenv(EnvTLSCAFile, filepath.Join(t.TempDir(), "missing.pem"))
	mustReject(t, "NATS TLS file")

	// NEXUS_REQUIRE_TLS covers the broker hop too.
	clearEnv(t)
	t.Setenv(EnvURL, "nats://nats:4222")
	t.Setenv("NEXUS_REQUIRE_TLS", "1")
	mustReject(t, "plaintext")
}

// TestAuthTLSOptionsApply checks what the options actually set on the client,
// which is what the broker sees.
func TestAuthTLSOptionsApply(t *testing.T) {
	apply := func(t *testing.T, c Config) nats.Options {
		t.Helper()
		if err := c.Validate(); err != nil {
			t.Fatalf("Validate: %v", err)
		}
		opts, err := c.authTLSOptions(slog.Default())
		if err != nil {
			t.Fatalf("authTLSOptions: %v", err)
		}
		o := nats.GetDefaultOptions()
		for _, fn := range opts {
			if err := fn(&o); err != nil {
				t.Fatalf("option: %v", err)
			}
		}
		return o
	}

	seedPath, _, pub := userSeedFile(t)
	o := apply(t, Config{URL: "nats://n:4222", NKeySeedFile: seedPath, InboxPrefix: InboxPrefixFor("nexus-event-service")})
	if o.Nkey != pub || o.SignatureCB == nil {
		t.Errorf("nkey option: Nkey=%q (want %q), SignatureCB set=%v", o.Nkey, pub, o.SignatureCB != nil)
	}
	if sig, err := o.SignatureCB([]byte("nonce")); err != nil || len(sig) == 0 {
		t.Errorf("signing the nonce failed: %v", err)
	}
	if o.InboxPrefix != "_INBOX.nexus-event-service" {
		t.Errorf("InboxPrefix = %q", o.InboxPrefix)
	}
	if !o.IgnoreAuthErrorAbort {
		t.Error("an authenticated client must keep retrying after auth errors (rotation must self-heal)")
	}
	if o.Secure {
		t.Error("TLS set without being asked for")
	}

	o = apply(t, Config{URL: "nats://127.0.0.1:4222", User: "u", Password: "p"})
	if o.User != "u" || o.Password != "p" {
		t.Errorf("user/password not applied: %q", o.User)
	}

	pwFile := writeFile(t, "pw", "first\n")
	o = apply(t, Config{URL: "nats://127.0.0.1:4222", User: "u", PasswordFile: pwFile})
	if o.UserInfo == nil {
		t.Fatal("password file must be read per connect (UserInfoHandler)")
	}
	if u, p := o.UserInfo(); u != "u" || p != "first" {
		t.Errorf("UserInfo = %q/%q", u, p)
	}
	_ = os.WriteFile(pwFile, []byte("rotated\n"), 0o600)
	if _, p := o.UserInfo(); p != "rotated" {
		t.Errorf("a rotated password file was not re-read: %q", p)
	}

	o = apply(t, Config{URL: "nats://n:4222"})
	if o.IgnoreAuthErrorAbort || o.Nkey != "" || o.User != "" {
		t.Error("an unauthenticated config must add no auth options")
	}

	ca, cert, key := testPKI(t)
	o = apply(t, Config{URL: "tls://n:4222", TLS: TLSConfig{Enabled: true, CAFile: ca, CertFile: cert, KeyFile: key, ServerName: "nats"}})
	if !o.Secure || o.TLSConfig == nil || o.TLSConfig.MinVersion < 0x0303 || o.TLSConfig.ServerName != "nats" {
		t.Errorf("TLS base config not applied: secure=%v cfg=%+v", o.Secure, o.TLSConfig)
	}
	if o.TLSCertCB == nil || o.RootCAsCB == nil {
		t.Fatal("reloading cert/CA callbacks not installed")
	}
	if pool, err := o.RootCAsCB(); err != nil || pool == nil {
		t.Errorf("RootCAsCB = %v, %v", pool, err)
	}
	if c, err := o.TLSCertCB(); err != nil || len(c.Certificate) == 0 {
		t.Errorf("TLSCertCB = %v", err)
	}

	// System trust store: TLS on, no files, no callbacks.
	o = apply(t, Config{URL: "tls://n:4222", TLS: TLSConfig{Enabled: true}})
	if !o.Secure || o.RootCAsCB != nil || o.TLSCertCB != nil {
		t.Error("system-trust TLS must not install file callbacks")
	}
}

func TestConnectRejectsInvalidHandBuiltConfig(t *testing.T) {
	if _, err := Connect(Config{URL: "nats://n:4222", RequireAuth: true}, nil); err == nil {
		t.Error("Connect must validate a hand-built Config, not only ConfigFromEnv")
	}
}

// testPKI writes a throwaway CA and a client leaf it signed.
func testPKI(t *testing.T) (caPath, certPath, keyPath string) {
	t.Helper()
	dir := t.TempDir()
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	caTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test ca"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caCert, _ := x509.ParseCertificate(caDER)
	leafKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	leafTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "nexus-agent"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, caCert, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, _ := x509.MarshalPKCS8PrivateKey(leafKey)
	write := func(name, typ string, der []byte) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der}), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	return write("ca.pem", "CERTIFICATE", caDER), write("tls.crt", "CERTIFICATE", leafDER), write("tls.key", "PRIVATE KEY", keyDER)
}
