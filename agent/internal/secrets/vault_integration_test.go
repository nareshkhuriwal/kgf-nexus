// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package secrets

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
)

// Integration tests against a REAL HashiCorp Vault in dev mode (`make
// test-vault` starts the pinned container). They exercise the token and
// AppRole login methods, KV v2 reads with fields and versions, Vault's own
// policy denial, and a sealed Vault. Kubernetes auth needs a TokenReview-
// capable cluster and is covered by the protocol test with a fake server.
const (
	VaultAddrVar  = "NEXUS_TEST_VAULT_ADDR"
	VaultTokenVar = "NEXUS_TEST_VAULT_ROOT_TOKEN"
	// VaultSealVar opts in to sealing the server at the end: irreversible for
	// a dev-mode container, so only the make target, which throws the
	// container away, sets it.
	VaultSealVar = "NEXUS_TEST_VAULT_ALLOW_SEAL"
	// VaultSkipMarker is greppable, like testdb.SkipMarker (F85).
	VaultSkipMarker = "skipping integration test: no test Vault"
)

type vaultAdmin struct {
	t     *testing.T
	addr  string
	token string
}

func (v vaultAdmin) call(method, path string, body any) map[string]any {
	v.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, v.addr+"/v1/"+path, rd)
	req.Header.Set("X-Vault-Token", v.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		v.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		v.t.Fatalf("%s %s: %d %s", method, path, resp.StatusCode, raw)
	}
	out := map[string]any{}
	_ = json.Unmarshal(raw, &out)
	return out
}

func TestVaultIntegration(t *testing.T) {
	addr, root := os.Getenv(VaultAddrVar), os.Getenv(VaultTokenVar)
	if addr == "" || root == "" {
		t.Skipf("%s: %s and %s are not set", VaultSkipMarker, VaultAddrVar, VaultTokenVar)
	}
	admin := vaultAdmin{t: t, addr: strings.TrimRight(addr, "/"), token: root}
	if resp, err := http.Get(admin.addr + "/v1/sys/health"); err != nil {
		t.Skipf("%s: %s is set but unreachable: %v", VaultSkipMarker, VaultAddrVar, err)
	} else {
		resp.Body.Close()
	}
	id := uuid.NewString()[:8]
	mount := "kv-" + id
	dir := t.TempDir()

	// A fresh KV v2 mount per run, a tenant path and a foreign one.
	admin.call("POST", "sys/mounts/"+mount, map[string]any{"type": "kv", "options": map[string]string{"version": "2"}})
	admin.call("POST", mount+"/data/team-a/db", map[string]any{"data": map[string]any{"password": "first-pw", "user": "app"}})
	admin.call("POST", mount+"/data/team-a/db", map[string]any{"data": map[string]any{"password": "second-pw", "user": "app"}})
	admin.call("POST", mount+"/data/team-a/secret-in-policy-gap", map[string]any{"data": map[string]any{"k": "v"}})
	admin.call("POST", mount+"/data/team-b/db", map[string]any{"data": map[string]any{"password": "b-pw"}})
	// Vault's own policy lets team-a read only team-a/db: the agent-side
	// allow-list below is deliberately broader, so Vault's refusal is what
	// the "policy gap" read exercises.
	policy := `path "` + mount + `/data/team-a/db" { capabilities = ["read"] }`
	admin.call("PUT", "sys/policies/acl/team-a-"+id, map[string]any{"policy": policy})

	// ── token auth ──
	tok := admin.call("POST", "auth/token/create", map[string]any{"policies": []string{"team-a-" + id}, "ttl": "10m"})
	tokenFile := writeFile(t, dir, "token", tok["auth"].(map[string]any)["client_token"].(string))

	tokenProv, err := NewVaultProvider(VaultConfig{Address: admin.addr, Mounts: []string{mount},
		AllowPaths: []string{mount + "/team-a"}, Auth: VaultAuth{Method: "token", TokenFile: tokenFile}})
	if err != nil {
		t.Fatal(err)
	}
	resolver := NewResolver(0, tokenProv)
	got, err := resolver.Resolve(context.Background(), []*nexusv1.SecretRef{
		{Name: "DB_PASSWORD", Ref: "vault://" + mount + "/team-a/db#password"},
		{Name: "DB_PASSWORD_V1", Ref: "vault://" + mount + "/team-a/db?version=1#password"},
		{Name: "db.json", Ref: "vault://" + mount + "/team-a/db", Inject: "file"},
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if string(got[0].Value) != "second-pw" || string(got[1].Value) != "first-pw" {
		t.Errorf("values %q %q", got[0].Value, got[1].Value)
	}
	var whole map[string]any
	if json.Unmarshal(got[2].Value, &whole) != nil || whole["user"] != "app" || got[2].Inject != InjectFile {
		t.Errorf("whole secret %s", got[2].Value)
	}

	expect := func(p Provider, raw string, cls nexusv1.ErrorClass) {
		t.Helper()
		r, perr := ParseRef(raw)
		if perr != nil {
			t.Fatal(perr)
		}
		_, err := p.Resolve(context.Background(), r)
		var se *Error
		if !errors.As(err, &se) || se.Class != cls {
			t.Errorf("%s: %v, want %v", raw, err, cls)
			return
		}
		if strings.Contains(se.Error(), "pw") {
			t.Errorf("%s: error mentions a value: %v", raw, se)
		}
	}
	expect(tokenProv, "vault://"+mount+"/team-a/secret-in-policy-gap", nexusv1.ErrorClass_ERROR_CLASS_PERMISSION_DENIED)
	expect(tokenProv, "vault://"+mount+"/team-b/db", nexusv1.ErrorClass_ERROR_CLASS_PERMISSION_DENIED)
	expect(tokenProv, "vault://"+mount+"/team-a/db?version=9#password", nexusv1.ErrorClass_ERROR_CLASS_NOT_FOUND)
	expect(tokenProv, "vault://"+mount+"/team-a/db#nope", nexusv1.ErrorClass_ERROR_CLASS_VALIDATION)

	// A revoked token is a clear PERMISSION_DENIED at login.
	revoked := admin.call("POST", "auth/token/create", map[string]any{"policies": []string{"team-a-" + id}, "ttl": "10m"})
	revokedTok := revoked["auth"].(map[string]any)["client_token"].(string)
	admin.call("POST", "auth/token/revoke", map[string]any{"token": revokedTok})
	revokedProv, _ := NewVaultProvider(VaultConfig{Address: admin.addr, Mounts: []string{mount},
		AllowPaths: []string{mount + "/team-a"}, Auth: VaultAuth{Method: "token", TokenFile: writeFile(t, dir, "revoked", revokedTok)}})
	expect(revokedProv, "vault://"+mount+"/team-a/db#password", nexusv1.ErrorClass_ERROR_CLASS_PERMISSION_DENIED)

	// ── AppRole auth, on its own mount ──
	approle := "approle-" + id
	admin.call("POST", "sys/auth/"+approle, map[string]any{"type": "approle"})
	admin.call("POST", "auth/"+approle+"/role/nexus-a", map[string]any{"token_policies": []string{"team-a-" + id}, "token_ttl": "5m"})
	roleID := admin.call("GET", "auth/"+approle+"/role/nexus-a/role-id", nil)["data"].(map[string]any)["role_id"].(string)
	secretID := admin.call("POST", "auth/"+approle+"/role/nexus-a/secret-id", map[string]any{})["data"].(map[string]any)["secret_id"].(string)
	arProv, err := NewVaultProvider(VaultConfig{Address: admin.addr, Mounts: []string{mount}, AllowPaths: []string{mount + "/team-a"},
		Auth: VaultAuth{Method: "approle", Mount: approle, RoleIDFile: writeFile(t, dir, "role-id", roleID),
			SecretIDFile: writeFile(t, dir, "secret-id", secretID)}})
	if err != nil {
		t.Fatal(err)
	}
	r, _ := ParseRef("vault://" + mount + "/team-a/db#password")
	if v, err := arProv.Resolve(context.Background(), r); err != nil || string(v) != "second-pw" {
		t.Fatalf("approle read: %q %v", v, err)
	}
	badAR, _ := NewVaultProvider(VaultConfig{Address: admin.addr, Mounts: []string{mount}, AllowPaths: []string{mount + "/team-a"},
		Auth: VaultAuth{Method: "approle", Mount: approle, RoleID: roleID, SecretIDFile: writeFile(t, dir, "bad-secret-id", "00000000-0000-0000-0000-000000000000")}})
	expect(badAR, "vault://"+mount+"/team-a/db#password", nexusv1.ErrorClass_ERROR_CLASS_PERMISSION_DENIED)

	// ── sealed: TRANSIENT and retryable ──
	if os.Getenv(VaultSealVar) != "1" {
		t.Logf("not sealing the Vault (%s != 1)", VaultSealVar)
		return
	}
	admin.call("PUT", "sys/seal", nil)
	fresh, _ := NewVaultProvider(VaultConfig{Address: admin.addr, Mounts: []string{mount}, AllowPaths: []string{mount + "/team-a"},
		Auth: VaultAuth{Method: "token", TokenFile: tokenFile}})
	_, err = fresh.Resolve(context.Background(), r)
	var se *Error
	if !errors.As(err, &se) || se.Class != nexusv1.ErrorClass_ERROR_CLASS_TRANSIENT || !se.Retryable {
		t.Fatalf("sealed vault: %v", err)
	}
	t.Logf("sealed vault answered: %v", err)
}
