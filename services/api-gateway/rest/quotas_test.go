// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package rest

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/khuriwalgroup/kgf-nexus/internal/store"
	"github.com/khuriwalgroup/kgf-nexus/services/api-gateway/authn"
	"google.golang.org/grpc/status"
)

// TestCreateRunNamespaceQuotaIs429: a run refused by a namespace quota answers
// 429 namespace_quota_exceeded with the dimension and numbers, and Retry-After
// for the hourly rate -- including when the refusal crossed the gRPC hop from
// the workflow-manager, which is how it arrives in production.
func TestCreateRunNamespaceQuotaIs429(t *testing.T) {
	h, _, wfm := newTestAPI(t)
	q := &store.RunQuotaError{Namespace: "team-ml", Dimension: "runs_per_hour", Used: 10, Limit: 10, RetryAfter: 125 * time.Second}
	// Exactly what upstream.reattachSentinel produces from the owner's status.
	wire := store.RunQuotaFromStatus(status.Convert(store.RunQuotaStatus("create run", q)))
	wfm.createFn = func() (store.RunRow, bool, error) { return store.RunRow{}, false, wire }

	rec := post(t, h, "/v1/namespaces/team-ml/runs", adminKey, "", `{"spec_name":"train"}`)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status %d, want 429: %s", rec.Code, rec.Body)
	}
	if got := rec.Header().Get("Retry-After"); got != "125" {
		t.Errorf("Retry-After = %q, want 125", got)
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["error"] != "namespace_quota_exceeded" || body["dimension"] != "runs_per_hour" ||
		body["used"] != float64(10) || body["limit"] != float64(10) ||
		body["message"] != "namespace_quota_exceeded: runs_per_hour (used 10 of 10)" {
		t.Errorf("body = %v", body)
	}

	// Active runs: 429 without Retry-After (runs free up when they finish).
	wfm.createFn = func() (store.RunRow, bool, error) {
		return store.RunRow{}, false, &store.RunQuotaError{Dimension: "active_runs", Used: 3, Limit: 3}
	}
	rec = post(t, h, "/v1/namespaces/team-ml/runs", adminKey, "", `{"spec_name":"train"}`)
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") != "" {
		t.Fatalf("active_runs: status %d Retry-After %q", rec.Code, rec.Header().Get("Retry-After"))
	}

	// The per-workflow cap keeps its own code.
	wfm.createFn = func() (store.RunRow, bool, error) { return store.RunRow{}, false, store.ErrMaxActiveRuns }
	rec = post(t, h, "/v1/namespaces/team-ml/runs", adminKey, "", `{"spec_name":"train"}`)
	var b2 map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &b2)
	if rec.Code != http.StatusTooManyRequests || b2["error"] != "max_active_runs" {
		t.Fatalf("max_active_runs: %d %v", rec.Code, b2)
	}
}

// TestQuotaPermissionsInCatalogAndTemplates pins the RBAC decisions of
// ADR-0021: the three new permissions exist, the templates grant them as
// documented, and a legacy reader sees quotas but not usage (cost data).
func TestQuotaPermissionsInCatalogAndTemplates(t *testing.T) {
	catalog := map[string]bool{}
	for _, p := range authn.Catalog() {
		catalog[p] = true
	}
	for _, p := range []string{"quotas.view", "quotas.manage", "usage.view"} {
		if !catalog[p] {
			t.Errorf("%s missing from the catalog", p)
		}
	}
	has := func(role, perm string) bool {
		for _, g := range authn.Builtins()[role] {
			if g == perm {
				return true
			}
		}
		return false
	}
	want := map[string]map[string]bool{
		"Viewer":                  {"quotas.view": true, "quotas.manage": false, "usage.view": false},
		"Workflow Operator":       {"quotas.view": true, "quotas.manage": false, "usage.view": false},
		"Infrastructure Operator": {"quotas.view": true, "quotas.manage": true, "usage.view": true},
		"Auditor":                 {"quotas.view": true, "quotas.manage": false, "usage.view": true},
		authn.AdministratorRole:   {"quotas.view": true, "quotas.manage": true, "usage.view": true},
	}
	for role, perms := range want {
		for perm, w := range perms {
			if has(role, perm) != w {
				t.Errorf("%s has %s = %v, want %v", role, perm, !w, w)
			}
		}
		grants := map[string]bool{}
		for _, g := range authn.Builtins()[role] {
			grants[g] = true
		}
		if err := authn.ValidateGrants(grants); err != nil {
			t.Errorf("template %s is not a valid grant set: %v", role, err)
		}
	}

	reader := authn.Principal{Role: authn.RoleReader, Scopes: []authn.Action{authn.ActionRead}}
	admin := authn.Principal{Role: authn.RoleAdmin, Scopes: []authn.Action{authn.ActionRead, authn.ActionWrite, authn.ActionAdminWrite}}
	if !reader.Allows("quotas.view") || reader.Allows("usage.view") || reader.Allows("quotas.manage") {
		t.Error("legacy reader: want quotas.view only")
	}
	if !admin.Allows("quotas.manage") || !admin.Allows("usage.view") {
		t.Error("legacy admin must manage quotas and view usage")
	}
	for _, route := range []string{"GET /v1/namespaces/{ns}/quotas", "PUT /v1/namespaces/{ns}/quotas", "GET /v1/namespaces/{ns}/usage", "GET /v1/platform/usage"} {
		if _, ok := routePermissions[route]; !ok {
			t.Errorf("%s has no permission mapping (it would answer 403 to everyone)", route)
		}
	}
}
