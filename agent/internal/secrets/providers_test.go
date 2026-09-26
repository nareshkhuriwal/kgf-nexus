// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package secrets

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
)

func writeFile(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// fakeVault is a minimal KV v2 + kubernetes-auth server that asserts the
// protocol details the provider must get right.
type fakeVault struct {
	t          *testing.T
	logins     atomic.Int32
	reads      atomic.Int32
	status     int // forced status for reads, 0 = normal
	sawNS      atomic.Value
	lastJWT    atomic.Value
	lastRole   atomic.Value
	revokeOnce atomic.Bool
}

func (f *fakeVault) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/auth/k8s-auth/login", func(w http.ResponseWriter, r *http.Request) {
		f.logins.Add(1)
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.lastJWT.Store(body["jwt"])
		f.lastRole.Store(body["role"])
		if body["role"] != "nexus-a" {
			w.WriteHeader(400)
			_, _ = w.Write([]byte(`{"errors":["invalid role name \"` + body["role"] + `\""]}`))
			return
		}
		_, _ = w.Write([]byte(`{"auth":{"client_token":"tok-` + string(rune('0'+f.logins.Load())) + `","lease_duration":3600}}`))
	})
	mux.HandleFunc("GET /v1/kv/data/", func(w http.ResponseWriter, r *http.Request) {
		f.reads.Add(1)
		f.sawNS.Store(r.Header.Get("X-Vault-Namespace"))
		if f.status != 0 {
			w.WriteHeader(f.status)
			_, _ = w.Write([]byte(`{"errors":["Vault is sealed"]}`))
			return
		}
		// First read with the first token: pretend it was revoked.
		if r.Header.Get("X-Vault-Token") == "tok-1" && f.revokeOnce.CompareAndSwap(false, true) {
			w.WriteHeader(403)
			_, _ = w.Write([]byte(`{"errors":["permission denied"]}`))
			return
		}
		switch strings.TrimPrefix(r.URL.Path, "/v1/kv/data/") {
		case "team-a/db":
			_, _ = w.Write([]byte(`{"data":{"data":{"password":"pw-123","port":5432},"metadata":{"version":2}}}`))
		case "team-a/gone":
			_, _ = w.Write([]byte(`{"data":{"data":null,"metadata":{"deletion_time":"2026-01-01T00:00:00Z"}}}`))
		default:
			w.WriteHeader(404)
			_, _ = w.Write([]byte(`{"errors":[]}`))
		}
	})
	return mux
}

func TestVaultProviderProtocol(t *testing.T) {
	fv := &fakeVault{t: t}
	srv := httptest.NewTLSServer(fv.handler())
	defer srv.Close()
	dir := t.TempDir()
	caFile := writeFile(t, dir, "ca.pem", string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})))
	jwt := writeFile(t, dir, "jwt", "sa-token-xyz\n")

	p, err := NewVaultProvider(VaultConfig{
		Address: srv.URL, Namespace: "team-a-ns", Mounts: []string{"kv"}, AllowPaths: []string{"kv/team-a"},
		Auth: VaultAuth{Method: "kubernetes", Mount: "k8s-auth", Role: "nexus-a", JWTFile: jwt},
		TLS:  TLSConfig{CAFile: caFile},
	})
	if err != nil {
		t.Fatal(err)
	}
	get := func(raw string) ([]byte, error) {
		r, err := ParseRef(raw)
		if err != nil {
			t.Fatal(err)
		}
		return p.Resolve(context.Background(), r)
	}

	// A revoked token is re-authenticated once, transparently.
	v, err := get("vault://kv/team-a/db#password")
	if err != nil || string(v) != "pw-123" {
		t.Fatalf("field read: %q %v", v, err)
	}
	if fv.logins.Load() != 2 || fv.lastJWT.Load() != "sa-token-xyz" || fv.sawNS.Load() != "team-a-ns" {
		t.Errorf("logins=%d jwt=%v ns=%v", fv.logins.Load(), fv.lastJWT.Load(), fv.sawNS.Load())
	}
	// The token is cached: no login for the next read.
	v, err = get("vault://kv/team-a/db")
	if err != nil || fv.logins.Load() != 2 {
		t.Fatalf("whole secret: %v logins=%d", err, fv.logins.Load())
	}
	var m map[string]any
	if json.Unmarshal(v, &m) != nil || m["password"] != "pw-123" {
		t.Errorf("whole secret JSON = %s", v)
	}
	if v, err := get("vault://kv/team-a/db#port"); err != nil || string(v) != "5432" {
		t.Errorf("non-string field: %q %v", v, err)
	}

	want := map[string]nexusv1.ErrorClass{
		"vault://kv/team-a/db#nope":      nexusv1.ErrorClass_ERROR_CLASS_VALIDATION,
		"vault://kv/team-a/missing":      nexusv1.ErrorClass_ERROR_CLASS_NOT_FOUND,
		"vault://kv/team-a/gone":         nexusv1.ErrorClass_ERROR_CLASS_NOT_FOUND,
		"vault://kv/team-b/db":           nexusv1.ErrorClass_ERROR_CLASS_PERMISSION_DENIED,
		"vault://kv/team-a/../team-b/db": nexusv1.ErrorClass_ERROR_CLASS_VALIDATION,
		"vault://other/team-a/db":        nexusv1.ErrorClass_ERROR_CLASS_VALIDATION,
		"vault://kv/team-a/db?version=x": nexusv1.ErrorClass_ERROR_CLASS_VALIDATION,
		"vault://kv/team-a/db?cas=1":     nexusv1.ErrorClass_ERROR_CLASS_VALIDATION,
	}
	for raw, cls := range want {
		if _, err := get(raw); err == nil || classOf(t, err) != cls {
			t.Errorf("%s: err=%v, want %v", raw, err, cls)
		}
	}

	// Sealed / unavailable is TRANSIENT and retryable, and the error text
	// quotes only Vault's errors array.
	fv.status = 503
	_, err = get("vault://kv/team-a/db#password")
	var se *Error
	if !errors.As(err, &se) || se.Class != nexusv1.ErrorClass_ERROR_CLASS_TRANSIENT || !se.Retryable || !strings.Contains(se.Error(), "sealed") {
		t.Errorf("503: %v", err)
	}
	fv.status = 0

	// A wrong role is a login refusal: PERMISSION_DENIED, not retried.
	bad, _ := NewVaultProvider(VaultConfig{Address: srv.URL, Mounts: []string{"kv"}, AllowPaths: []string{"kv/team-a"},
		Auth: VaultAuth{Method: "kubernetes", Mount: "k8s-auth", Role: "wrong", JWTFile: jwt}, TLS: TLSConfig{CAFile: caFile}})
	r, _ := ParseRef("vault://kv/team-a/db")
	if _, err := bad.Resolve(context.Background(), r); err == nil || classOf(t, err) != nexusv1.ErrorClass_ERROR_CLASS_PERMISSION_DENIED {
		t.Errorf("wrong role: %v", err)
	}

	// Without the CA the TLS handshake fails: TRANSIENT (and never insecure).
	noCA, _ := NewVaultProvider(VaultConfig{Address: srv.URL, Mounts: []string{"kv"}, AllowPaths: []string{"kv/team-a"},
		Auth: VaultAuth{Method: "kubernetes", Mount: "k8s-auth", Role: "nexus-a", JWTFile: jwt}})
	if _, err := noCA.Resolve(context.Background(), r); err == nil || classOf(t, err) != nexusv1.ErrorClass_ERROR_CLASS_TRANSIENT {
		t.Errorf("untrusted server: %v", err)
	}
	// Unreachable: TRANSIENT.
	down, _ := NewVaultProvider(VaultConfig{Address: "https://127.0.0.1:1", AllowPaths: []string{"kv"},
		Auth: VaultAuth{Method: "token", TokenFile: jwt}})
	if _, err := down.Resolve(context.Background(), r); err == nil || classOf(t, err) != nexusv1.ErrorClass_ERROR_CLASS_TRANSIENT {
		t.Errorf("unreachable: %v", err)
	}
}

func TestVaultConfigRefusesInsecureSettings(t *testing.T) {
	base := VaultConfig{AllowPaths: []string{"kv"}, Auth: VaultAuth{Method: "token", TokenFile: "/x"}}
	for name, mut := range map[string]func(*VaultConfig){
		"plain http remote":      func(c *VaultConfig) { c.Address = "http://vault.example:8200" },
		"skip verify remote":     func(c *VaultConfig) { c.Address = "https://vault.example:8200"; c.TLS.InsecureSkipVerify = true },
		"no allow paths":         func(c *VaultConfig) { c.Address = "https://v:8200"; c.AllowPaths = nil },
		"approle without secret": func(c *VaultConfig) { c.Address = "https://v:8200"; c.Auth = VaultAuth{Method: "approle", RoleID: "r"} },
		"unknown method":         func(c *VaultConfig) { c.Address = "https://v:8200"; c.Auth = VaultAuth{Method: "userpass"} },
		"k8s without role":       func(c *VaultConfig) { c.Address = "https://v:8200"; c.Auth = VaultAuth{Method: "kubernetes"} },
	} {
		c := base
		mut(&c)
		if _, err := NewVaultProvider(c); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// Loopback plain http is allowed (a dev Vault, Vault Agent on localhost).
	c := base
	c.Address = "http://127.0.0.1:8200"
	if _, err := NewVaultProvider(c); err != nil {
		t.Errorf("loopback http: %v", err)
	}
}

type fakeAKV struct {
	secrets map[string]string
	err     error
	urls    []string
}

func (f *fakeAKV) GetSecret(_ context.Context, name, version string) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	v, ok := f.secrets[name+"@"+version]
	if !ok {
		return "", &azcore.ResponseError{StatusCode: 404, ErrorCode: "SecretNotFound"}
	}
	return v, nil
}

func TestAKVProviderWithAFakeVault(t *testing.T) {
	fake := &fakeAKV{secrets: map[string]string{"db-pass@": "latest-pw", "db-pass@v1": "old-pw"}}
	p := newAKVProviderWith(AKVConfig{AllowVaults: []string{"team-a-kv"}}, func(u string) (AKVGetter, error) {
		fake.urls = append(fake.urls, u)
		return fake, nil
	})
	get := func(raw string) ([]byte, error) {
		r, err := ParseRef(raw)
		if err != nil {
			t.Fatal(err)
		}
		return p.Resolve(context.Background(), r)
	}
	if v, err := get("akv://team-a-kv/db-pass"); err != nil || string(v) != "latest-pw" {
		t.Fatalf("latest: %q %v", v, err)
	}
	if v, err := get("akv://Team-A-KV/db-pass/v1"); err != nil || string(v) != "old-pw" {
		t.Fatalf("version: %q %v", v, err)
	}
	if len(fake.urls) != 1 || fake.urls[0] != "https://team-a-kv.vault.azure.net/" {
		t.Errorf("clients built for %v; want one, cached, for the public-cloud URL", fake.urls)
	}
	cases := map[string]nexusv1.ErrorClass{
		"akv://team-b-kv/db-pass":   nexusv1.ErrorClass_ERROR_CLASS_PERMISSION_DENIED,
		"akv://team-a-kv/missing":   nexusv1.ErrorClass_ERROR_CLASS_NOT_FOUND,
		"akv://team-a-kv/bad_name":  nexusv1.ErrorClass_ERROR_CLASS_VALIDATION,
		"akv://team-a-kv":           nexusv1.ErrorClass_ERROR_CLASS_VALIDATION,
		"akv://team-a-kv/db-pass#f": nexusv1.ErrorClass_ERROR_CLASS_VALIDATION,
		"akv://team-a-kv/a/b/c":     nexusv1.ErrorClass_ERROR_CLASS_VALIDATION,
	}
	for raw, cls := range cases {
		if _, err := get(raw); err == nil || classOf(t, err) != cls {
			t.Errorf("%s: %v, want %v", raw, err, cls)
		}
	}
	for status, cls := range map[int]nexusv1.ErrorClass{
		403: nexusv1.ErrorClass_ERROR_CLASS_PERMISSION_DENIED,
		401: nexusv1.ErrorClass_ERROR_CLASS_PERMISSION_DENIED,
		429: nexusv1.ErrorClass_ERROR_CLASS_TRANSIENT,
		503: nexusv1.ErrorClass_ERROR_CLASS_TRANSIENT,
	} {
		fake.err = &azcore.ResponseError{StatusCode: status, ErrorCode: "X"}
		if _, err := get("akv://team-a-kv/db-pass"); err == nil || classOf(t, err) != cls {
			t.Errorf("status %d: %v, want %v", status, err, cls)
		}
	}
	fake.err = errors.New("dial tcp: lookup team-a-kv.vault.azure.net: no such host")
	if _, err := get("akv://team-a-kv/db-pass"); err == nil || classOf(t, err) != nexusv1.ErrorClass_ERROR_CLASS_TRANSIENT {
		t.Errorf("network: %v", err)
	}
}

// TestAKVFollowsTheKGFPlatformVaultConvention: one shared platform vault
// (kgf-platform-kv-<env>) named by default_vault, and per-tenant secret-name
// prefixes as the isolation inside it.
func TestAKVFollowsTheKGFPlatformVaultConvention(t *testing.T) {
	fake := &fakeAKV{secrets: map[string]string{"nexus-team-a-db-password@": "pw-a", "nexus-team-b-db-password@": "pw-b"}}
	p := newAKVProviderWith(AKVConfig{AllowVaults: []string{"kgf-platform-kv-dev"}, DefaultVault: "kgf-platform-kv-dev",
		AllowSecretPrefixes: []string{"nexus-team-a-"}}, func(u string) (AKVGetter, error) { return fake, nil })
	get := func(raw string) ([]byte, error) {
		r, err := ParseRef(raw)
		if err != nil {
			t.Fatal(err)
		}
		return p.Resolve(context.Background(), r)
	}
	if v, err := get("akv:///nexus-team-a-db-password"); err != nil || string(v) != "pw-a" {
		t.Fatalf("default vault: %q %v", v, err)
	}
	if v, err := get("akv://kgf-platform-kv-dev/NEXUS-TEAM-A-db-password"); err == nil && string(v) == "pw-b" {
		t.Fatal("case folding crossed tenants")
	}
	if _, err := get("akv:///nexus-team-b-db-password"); err == nil || classOf(t, err) != nexusv1.ErrorClass_ERROR_CLASS_PERMISSION_DENIED {
		t.Errorf("another tenant's prefix: %v", err)
	}
	noDefault := newAKVProviderWith(AKVConfig{AllowVaults: []string{"kgf-platform-kv-dev"}}, func(string) (AKVGetter, error) { return fake, nil })
	r, _ := ParseRef("akv:///nexus-team-a-db-password")
	if _, err := noDefault.Resolve(context.Background(), r); err == nil || classOf(t, err) != nexusv1.ErrorClass_ERROR_CLASS_VALIDATION {
		t.Errorf("no default vault: %v", err)
	}
	if _, err := NewAKVProvider(AKVConfig{AllowVaults: []string{"a-vault"}, DefaultVault: "other-vault",
		Auth: AKVAuth{Method: "managed_identity"}}); err == nil {
		t.Error("a default vault outside allow_vaults was accepted")
	}
}

func TestAKVConfigValidation(t *testing.T) {
	if _, err := NewAKVProvider(AKVConfig{AllowVaults: []string{"ok-vault"}, Auth: AKVAuth{Method: "client_secret"}}); err == nil {
		t.Error("client_secret without tenant/client/file accepted")
	}
	if _, err := NewAKVProvider(AKVConfig{AllowVaults: []string{"x"}, Auth: AKVAuth{Method: "managed_identity"}}); err == nil {
		t.Error("invalid vault name accepted")
	}
	if _, err := NewAKVProvider(AKVConfig{AllowVaults: []string{"ok-vault"}, VaultURLs: map[string]string{"ok-vault": "http://x"},
		Auth: AKVAuth{Method: "managed_identity"}}); err == nil {
		t.Error("http vault URL accepted")
	}
	dir := t.TempDir()
	sec := writeFile(t, dir, "cs", "client-secret\n")
	if _, err := NewAKVProvider(AKVConfig{AllowVaults: []string{"ok-vault"},
		Auth: AKVAuth{Method: "client_secret", TenantID: "00000000-0000-0000-0000-000000000001", ClientID: "c", ClientSecretFile: sec}}); err != nil {
		t.Errorf("valid client_secret config: %v", err)
	}
	if _, err := NewAKVProvider(AKVConfig{AllowVaults: []string{"ok-vault"}, Auth: AKVAuth{Method: "managed_identity", ClientID: "c"}}); err != nil {
		t.Errorf("managed identity: %v", err)
	}
}
