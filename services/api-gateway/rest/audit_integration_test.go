// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0
//go:build integration

package rest

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/khuriwalgroup/kgf-nexus/internal/auditchain"
	"github.com/khuriwalgroup/kgf-nexus/internal/kgfctx"
	"github.com/khuriwalgroup/kgf-nexus/internal/store"
	"github.com/khuriwalgroup/kgf-nexus/internal/testdb"
	"github.com/khuriwalgroup/kgf-nexus/services/api-gateway/auditexport"
	"github.com/khuriwalgroup/kgf-nexus/services/api-gateway/authn"
)

// TestIntegrationAuditAPI drives the real routes against the real schema:
// security grants leave chained rows, the NDJSON export verifies, CSV is
// formula-safe, paging walks the trail, the policy and legal-hold routes are
// revisioned and audited, sinks show their secret once, and an Auditor can
// read but not reconfigure.
func TestIntegrationAuditAPI(t *testing.T) {
	pool := attentionPool(t)
	ctx := context.Background()
	ns := fmt.Sprintf("audit-api-%d", time.Now().UnixNano())
	testdb.RegisterTenant(t, pool, ns)
	key := "nx_audit_test_" + ns
	if _, err := store.GatewayCreateAPIKey(ctx, pool, ns, "=cmd|' /C calc'!A0", "admin", key); err != nil {
		t.Fatal(err)
	}
	keys, _ := auditexport.NewKeyring(bytes.Repeat([]byte{9}, 32))
	auth := authn.New(pool).WithRBAC().WithIdentity(authn.StoreIdentity{DB: pool})
	h := New(pool, auth, nil, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil))).
		WithIdentity(NewStoreIdentityAdmin(pool)).WithAuditExport(keys, auditexport.URLPolicy{}).Routes()
	call := func(method, path, body, bearer string, status int) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+bearer)
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != status {
			t.Fatalf("%s %s: got %d expected %d: %s", method, path, w.Code, status, w.Body.String())
		}
		return w
	}
	base := "/v1/namespaces/" + ns

	// Security grants through the API, each audited in its own transaction.
	for i := 0; i < 4; i++ {
		call("POST", base+"/tokens", fmt.Sprintf(`{"name":"ci-%d","role":"reader","scopes":["read"]}`, i), key, 201)
	}
	call("POST", base+"/users", `{"email":"audited@example.test","password":"a-long-enough-password"}`, key, 201)

	// Export verifies end to end.
	w := call("GET", base+"/audit/export", "", key, 200)
	if ct := w.Header().Get("Content-Type"); ct != "application/x-ndjson" {
		t.Errorf("content type %q", ct)
	}
	res, err := auditchain.Verify(bytes.NewReader(w.Body.Bytes()))
	if err != nil || !res.OK(false) || res.Chained < 5 || !res.Complete {
		t.Fatalf("export verification: %+v err=%v\n%s", res, err, w.Body.String())
	}

	// CSV neutralises the formula in the actor label.
	w = call("GET", base+"/audit/export?format=csv&action=token.create", "", key, 200)
	rows, err := csv.NewReader(bytes.NewReader(w.Body.Bytes())).ReadAll()
	if err != nil || len(rows) != 5 || rows[1][6] != `'=cmd|' /C calc'!A0` {
		t.Fatalf("csv rows=%d label=%q err=%v", len(rows), rows[1][6], err)
	}

	// Paging walks every row once.
	seen := map[float64]bool{}
	cursor := ""
	for page := 0; page < 10; page++ {
		path := base + "/audit?limit=2"
		if cursor != "" {
			path += "&cursor=" + cursor
		}
		var body struct {
			Entries    []map[string]any `json:"entries"`
			NextCursor *string          `json:"next_cursor"`
		}
		_ = json.Unmarshal(call("GET", path, "", key, 200).Body.Bytes(), &body)
		for _, e := range body.Entries {
			id := e["id"].(float64)
			if seen[id] {
				t.Fatalf("row %v served twice", id)
			}
			seen[id] = true
			if e["row_hash"] == nil {
				t.Errorf("entry without row_hash: %v", e)
			}
		}
		if body.NextCursor == nil {
			break
		}
		cursor = *body.NextCursor
	}
	if int64(len(seen)) != res.Rows {
		t.Errorf("paging saw %d rows, export had %d", len(seen), res.Rows)
	}
	call("GET", base+"/audit?cursor=forged", "", key, 400)

	// Policy and legal hold: revisioned, validated, audited.
	call("PUT", base+"/audit/policy", `{"retention_days":400,"revision":0}`, key, 200)
	call("PUT", base+"/audit/policy", `{"retention_days":30,"revision":0}`, key, 409)
	call("PUT", base+"/audit/policy", `{"retention_days":0,"revision":1}`, key, 422)
	call("POST", base+"/audit/legal-hold", `{"hold":true,"revision":1}`, key, 422)
	call("POST", base+"/audit/legal-hold", `{"hold":true,"reason":"case 7","revision":1}`, key, 200)
	call("POST", base+"/audit/purge", `{}`, key, 409)
	var pol store.AuditPolicy
	_ = json.Unmarshal(call("GET", base+"/audit/policy", "", key, 200).Body.Bytes(), &pol)
	if !pol.LegalHold || pol.LegalHoldReason != "case 7" || *pol.RetentionDays != 400 || pol.Revision != 2 {
		t.Errorf("policy = %+v", pol)
	}

	// Sinks: an internal URL is refused; the secret is shown once only.
	call("POST", base+"/audit/sinks", `{"name":"meta","url":"https://169.254.169.254/x"}`, key, 422)
	var created map[string]any
	_ = json.Unmarshal(call("POST", base+"/audit/sinks", `{"name":"siem","url":"https://siem.example.com/in"}`, key, 201).Body.Bytes(), &created)
	if s, _ := created["signing_secret"].(string); !strings.HasPrefix(s, "whsec_") {
		t.Fatalf("created sink = %v", created)
	}
	list := call("GET", base+"/audit/sinks", "", key, 200).Body.String()
	if strings.Contains(list, created["signing_secret"].(string)) || strings.Contains(list, "ciphertext") {
		t.Fatalf("the sink list leaks the secret: %s", list)
	}
	id := created["id"].(string)
	call("PUT", base+"/audit/sinks/"+id, `{"enabled":false,"revision":1}`, key, 200)
	rot := call("POST", base+"/audit/sinks/"+id+"/rotate-secret", `{"revision":2}`, key, 200).Body.String()
	if !strings.Contains(rot, `"signing_secret":"whsec_`) {
		t.Errorf("rotation did not return the new secret once: %s", rot)
	}
	call("DELETE", base+"/audit/sinks/"+id, "", key, 200)

	// Every configuration change above is in the verified chain.
	w = call("GET", base+"/audit/export?action=audit.sink.create", "", key, 200)
	if !strings.Contains(w.Body.String(), `"action":"audit.sink.create"`) {
		t.Error("sink creation not audited")
	}
	res, _ = auditchain.Verify(bytes.NewReader(call("GET", base+"/audit/export", "", key, 200).Body.Bytes()))
	if !res.OK(false) {
		t.Fatalf("final verification: %v", res.Problems)
	}

	// KGF Cloud attribution: a verified X-KGF-Context lands in the chained
	// row and in the KGF-shaped export; a forged one contributes nothing.
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	kv, err := kgfctx.FromEnv(func(k string) string {
		return map[string]string{kgfctx.EnvPublicKeys: base64.RawURLEncoding.EncodeToString(pub), kgfctx.EnvUserIssuers: "gateway"}[k]
	})
	if err != nil {
		t.Fatal(err)
	}
	hk := New(pool, auth, nil, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil))).
		WithIdentity(NewStoreIdentityAdmin(pool)).WithAuditExport(keys, auditexport.URLPolicy{}).WithKGFContext(kv).Routes()
	kgfToken := func(key ed25519.PrivateKey) string {
		hdr, _ := json.Marshal(map[string]string{"alg": "EdDSA", "typ": "JWT", "kid": kgfctx.KeyID(pub)})
		// What KGF's gateway forwards for a signed-in operator.
		cl, _ := json.Marshal(map[string]any{"org": "org-kgf", "sub": "sub-9", "rg": "rg-data", "principal": "user:ana",
			"role": "operator", "request_id": "req_kgf_1", "iss": "gateway", "aud": "nexus",
			"iat": time.Now().Unix(), "exp": time.Now().Add(time.Minute).Unix()})
		s := base64.RawURLEncoding.EncodeToString(hdr) + "." + base64.RawURLEncoding.EncodeToString(cl)
		return s + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, []byte(s)))
	}
	send := func(token string, name string) {
		r := httptest.NewRequest("POST", base+"/tokens", strings.NewReader(`{"name":"`+name+`","role":"reader","scopes":["read"]}`))
		r.Header.Set("Authorization", "Bearer "+key)
		r.Header.Set(kgfctx.Header, token)
		r.Header.Set(kgfctx.RequestIDHeader, "req_caller_supplied")
		rw := httptest.NewRecorder()
		hk.ServeHTTP(rw, r)
		if rw.Code != 201 {
			t.Fatalf("token via KGF: %d %s", rw.Code, rw.Body)
		}
	}
	send(kgfToken(priv), "via-kgf")
	_, forged, _ := ed25519.GenerateKey(rand.Reader)
	send(kgfToken(forged), "forged-kgf")
	var events []auditchain.KGFEvent
	for _, line := range strings.Split(strings.TrimSpace(call("GET", base+"/audit/export?format=kgf&action=token.create", "", key, 200).Body.String()), "\n") {
		var ev auditchain.KGFEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("kgf line %q: %v", line, err)
		}
		events = append(events, ev)
	}
	var viaKGF, viaForged *auditchain.KGFEvent
	for i := range events {
		switch d := string(events[i].Nexus.Detail); {
		case strings.Contains(d, `"via-kgf"`):
			viaKGF = &events[i]
		case strings.Contains(d, `"forged-kgf"`):
			viaForged = &events[i]
		}
	}
	if viaKGF == nil || viaKGF.OrganizationID != "org-kgf" || viaKGF.Principal != "user:ana" ||
		viaKGF.RequestID != "req_kgf_1" || !viaKGF.Attributed || viaKGF.Chain == nil ||
		!strings.HasPrefix(viaKGF.ResourceID, "/organizations/org-kgf/subscriptions/sub-9/resourceGroups/rg-data/providers/KGF.Nexus/") {
		t.Fatalf("verified KGF attribution = %+v", viaKGF)
	}
	if viaForged == nil || viaForged.OrganizationID != "" || viaForged.Attributed || viaForged.RequestID != "req_caller_supplied" {
		t.Fatalf("a forged KGF context was attributed: %+v", viaForged)
	}
	if res, _ := auditchain.Verify(bytes.NewReader(call("GET", base+"/audit/export", "", key, 200).Body.Bytes())); !res.OK(false) {
		t.Fatalf("chain with KGF attribution: %v", res.Problems)
	}

	// An Auditor reads and exports, but cannot reconfigure.
	auditor, err := store.GatewayCreateUserWithoutPassword(ctx, pool, ns, "auditor@example.test", "Auditor", "reader")
	if err != nil {
		t.Fatal(err)
	}
	call("PUT", base+"/users/"+auditor.ID+"/roles", `{"roles":["Auditor"],"revision":0}`, key, 200)
	tok, _, err := store.GatewayCreateSession(ctx, pool, auditor.ID, time.Hour, "t", "")
	if err != nil {
		t.Fatal(err)
	}
	call("GET", base+"/audit/export", "", tok, 200)
	call("PUT", base+"/audit/policy", `{"retention_days":1,"revision":2}`, tok, 403)
	call("POST", base+"/audit/legal-hold", `{"hold":false,"revision":2}`, tok, 403)
	call("POST", base+"/audit/sinks", `{"name":"x","url":"https://siem.example.com/"}`, tok, 403)
}
