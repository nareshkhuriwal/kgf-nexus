// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0
//go:build integration

package rest

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/khuriwalgroup/kgf-nexus/internal/store"
	"github.com/khuriwalgroup/kgf-nexus/internal/testdb"
	"github.com/khuriwalgroup/kgf-nexus/services/api-gateway/authn"
)

// The live infrastructure routes (ADR-0037) through the real router, guard
// and database.

type infraFixture struct {
	t    *testing.T
	ctx  context.Context
	db   *pgxpool.Pool
	ns   string
	keys map[string]string // role -> API key
}

func newInfraFixture(t *testing.T) *infraFixture {
	t.Helper()
	f := &infraFixture{t: t, ctx: context.Background(), db: attentionPool(t),
		ns: fmt.Sprintf("infra-%d", time.Now().UnixNano()), keys: map[string]string{}}
	testdb.RegisterTenant(t, f.db, f.ns)
	for _, role := range []string{"admin", "reader"} {
		key := "nx_infra_" + role + "_" + f.ns
		if _, err := store.GatewayCreateAPIKey(f.ctx, f.db, f.ns, "infra-"+role, role, key); err != nil {
			t.Fatal(err)
		}
		f.keys[role] = key
	}
	t.Cleanup(func() { _, _ = f.db.Exec(f.ctx, `DELETE FROM api_keys WHERE namespace = $1`, f.ns) })
	return f
}

func (f *infraFixture) get(api *API, ns, path, credential string, want int) []byte {
	f.t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/v1/namespaces/"+ns+path, nil)
	r.Header.Set("Authorization", "Bearer "+credential)
	w := httptest.NewRecorder()
	api.Routes().ServeHTTP(w, r)
	if w.Code != want {
		f.t.Fatalf("GET %s: %d %s, want %d", path, w.Code, w.Body, want)
	}
	return w.Body.Bytes()
}

func (f *infraFixture) api() *API {
	return New(f.db, authn.New(f.db).WithRBAC().WithIdentity(authn.StoreIdentity{DB: f.db}), nil, nil, nil,
		slog.New(slog.NewTextHandler(io.Discard, nil))).WithIdentity(NewStoreIdentityAdmin(f.db))
}

type componentsBody struct {
	GeneratedAt       string `json:"generated_at"`
	StaleAfterSeconds int    `json:"stale_after_seconds"`
	TopologyVisible   bool   `json:"topology_visible"`
	Components        []struct {
		Component string         `json:"component"`
		Summary   map[string]int `json:"summary"`
		Instances []struct {
			Instance     string         `json:"instance"`
			PodNamespace string         `json:"pod_namespace"`
			Node         string         `json:"node"`
			Cluster      string         `json:"cluster"`
			Version      string         `json:"version"`
			StartedAt    string         `json:"started_at"`
			LastSeenAt   string         `json:"last_seen_at"`
			State        string         `json:"state"`
			Detail       map[string]any `json:"detail"`
		} `json:"instances"`
	} `json:"components"`
}

func TestIntegrationPlatformComponents(t *testing.T) {
	f := newInfraFixture(t)
	// The registry is platform-wide and shared with every other test, so this
	// test seeds instances under a cluster name of its own and looks only at
	// those.
	cluster := "it-" + f.ns
	t.Cleanup(func() { _, _ = f.db.Exec(f.ctx, `DELETE FROM service_instances WHERE cluster = $1`, cluster) })
	seed := func(component, pod string, ready bool, silentFor time.Duration) {
		t.Helper()
		if err := store.UpsertServiceInstance(f.ctx, f.db, store.ServiceInstance{
			Component: component, Instance: pod, PodNamespace: "kgf-nexus", Node: "aks-system-vmss000001",
			Cluster: cluster, Version: "1.9.0", StartedAt: time.Now().Add(-time.Hour), Ready: ready,
			Detail: map[string]any{"readiness": "ok", "queue": "default", "claims_total": 42, "heartbeat_interval_seconds": 10},
		}); err != nil {
			t.Fatal(err)
		}
		if silentFor > 0 {
			if _, err := f.db.Exec(f.ctx, `UPDATE service_instances SET last_seen_at = now() - make_interval(secs => $2)
				WHERE cluster = $1 AND instance = $3`, cluster, silentFor.Seconds(), pod); err != nil {
				t.Fatal(err)
			}
		}
	}
	seed("scheduler", "kgf-nexus-scheduler-0", true, 0)
	seed("scheduler", "kgf-nexus-scheduler-1", true, 45*time.Second)
	seed("api-gateway", "kgf-nexus-api-gateway-7d9f-abcde", false, 0)

	mine := func(b componentsBody, component string) (n, ready, stale int, insts []string) {
		for _, c := range b.Components {
			if c.Component != component {
				continue
			}
			for _, i := range c.Instances {
				if (b.TopologyVisible && i.Cluster != cluster) || (!b.TopologyVisible && i.Version != "1.9.0") {
					continue
				}
				n++
				insts = append(insts, i.Instance+"/"+i.State)
				switch i.State {
				case "ready":
					ready++
				case "stale":
					stale++
				}
			}
		}
		return
	}

	// An admin key is not a platform operator: health only, masked.
	var masked componentsBody
	raw := f.get(f.api(), f.ns, "/platform/components", f.keys["admin"], http.StatusOK)
	if err := json.Unmarshal(raw, &masked); err != nil {
		t.Fatal(err)
	}
	if masked.TopologyVisible || masked.StaleAfterSeconds != 30 || len(masked.Components) != 6 {
		t.Fatalf("masked header: visible=%v stale_after=%d components=%d", masked.TopologyVisible, masked.StaleAfterSeconds, len(masked.Components))
	}
	t.Logf("GET /v1/namespaces/%s/platform/components (masked):\n%s", f.ns, raw)
	if strings.Contains(string(raw), cluster) || strings.Contains(string(raw), "kgf-nexus-scheduler-0") ||
		strings.Contains(string(raw), "aks-system-vmss000001") {
		t.Fatal("the masked view leaked a pod, node or cluster name")
	}
	for _, c := range masked.Components {
		for _, i := range c.Instances {
			if _, ok := i.Detail["claims_total"]; ok {
				t.Fatalf("the masked view leaked platform activity: %v", i.Detail)
			}
		}
	}

	// The opt-in for a single-organisation installation: full view.
	var full componentsBody
	raw = f.get(f.api().WithComponentTopology(TopologyInfrastructureViewers), f.ns, "/platform/components", f.keys["admin"], http.StatusOK)
	if err := json.Unmarshal(raw, &full); err != nil {
		t.Fatal(err)
	}
	if !full.TopologyVisible {
		t.Fatal("infrastructure-viewers did not show the topology")
	}
	t.Logf("GET /v1/namespaces/%s/platform/components (topology visible):\n%s", f.ns, raw)
	if n, ready, stale, insts := mine(full, "scheduler"); n != 2 || ready != 1 || stale != 1 {
		t.Fatalf("schedulers = %v (n=%d ready=%d stale=%d)", insts, n, ready, stale)
	}
	if n, ready, _, insts := mine(full, "api-gateway"); n != 1 || ready != 0 || insts[0] != "kgf-nexus-api-gateway-7d9f-abcde/not_ready" {
		t.Fatalf("gateways = %v", insts)
	}
	for _, c := range full.Components {
		if c.Component == "scheduler" {
			for _, i := range c.Instances {
				if i.Cluster == cluster && (i.Node != "aks-system-vmss000001" || i.PodNamespace != "kgf-nexus" || i.Detail["queue"] != "default") {
					t.Fatalf("full instance = %+v", i)
				}
			}
		}
	}
	if _, err := time.Parse(time.RFC3339, full.GeneratedAt); err != nil {
		t.Errorf("generated_at %q: %v", full.GeneratedAt, err)
	}

	// infrastructure.view is required: a reader key, and another namespace's
	// admin, are refused.
	f.get(f.api(), f.ns, "/platform/components", f.keys["reader"], http.StatusForbidden)
	other := newInfraFixture(t)
	f.get(f.api(), f.ns, "/platform/components", other.keys["admin"], http.StatusForbidden)
	f.get(f.api(), f.ns, "/platform/components", "", http.StatusUnauthorized)
}

// TestIntegrationPlatformOperatorSeesTheTopology: a platform operator's
// session sees placement without the opt-in; the same account before
// enrolment does not.
func TestIntegrationPlatformOperatorSeesTheTopology(t *testing.T) {
	f := newInfraFixture(t)
	api := f.api()
	email := fmt.Sprintf("operator-%d@example.test", time.Now().UnixNano())
	body := `{"name":"Operator","email":"` + email + `","password":"strong-test-password"}`
	r := httptest.NewRequest(http.MethodPost, "/v1/auth/register", strings.NewReader(body))
	w := httptest.NewRecorder()
	api.Routes().ServeHTTP(w, r)
	if w.Code != http.StatusCreated {
		t.Fatalf("register: %d %s", w.Code, w.Body)
	}
	var reg struct {
		Tenant store.TenantRow `json:"tenant"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &reg); err != nil {
		t.Fatal(err)
	}
	ns := reg.Tenant.Namespace
	r = httptest.NewRequest(http.MethodPost, "/v1/auth/login",
		strings.NewReader(`{"namespace":"`+ns+`","email":"`+email+`","password":"strong-test-password"}`))
	w = httptest.NewRecorder()
	api.Routes().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("login: %d %s", w.Code, w.Body)
	}
	var login struct {
		Token string `json:"token"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &login)

	var visible bool
	check := func() {
		t.Helper()
		var b componentsBody
		if err := json.Unmarshal(f.get(api, ns, "/platform/components", login.Token, http.StatusOK), &b); err != nil {
			t.Fatal(err)
		}
		visible = b.TopologyVisible
	}
	check()
	if visible {
		t.Fatal("a namespace administrator who is not a platform operator saw the topology")
	}
	var userID string
	if err := f.db.QueryRow(f.ctx, `SELECT owner_user_id::text FROM tenants WHERE namespace = $1`, ns).Scan(&userID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(f.ctx, `INSERT INTO platform_operators (user_id) VALUES ($1)`, userID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = f.db.Exec(f.ctx, `DELETE FROM platform_operators WHERE user_id = $1`, userID) })
	check()
	if !visible {
		t.Fatal("an enrolled platform operator did not see the topology")
	}
}

func TestIntegrationTaskRunnersScopingAndLimits(t *testing.T) {
	f := newInfraFixture(t)
	other := newInfraFixture(t)
	running := func(ns string) string {
		t.Helper()
		id := testdb.NewAttempt(t, f.db, ns)
		if _, err := f.db.Exec(f.ctx, `UPDATE task_attempts SET state = 'DISPATCHED', dispatched_at = now() WHERE id = $1`, id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	mineA, mineB := running(f.ns), running(f.ns)
	theirs := running(other.ns)

	var body struct {
		Summary struct {
			Running    int            `json:"running"`
			ByExecutor map[string]int `json:"by_executor"`
			ByCluster  map[string]int `json:"by_cluster"`
		} `json:"summary"`
		Runners []struct {
			AttemptID           string  `json:"attempt_id"`
			Executor            string  `json:"executor"`
			Pod                 string  `json:"pod"`
			State               string  `json:"state"`
			StartedAt           *string `json:"started_at"`
			PlacementReportedAt *string `json:"placement_reported_at"`
		} `json:"runners"`
	}
	raw := f.get(f.api(), f.ns, "/platform/task-runners", f.keys["admin"], http.StatusOK)
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	if body.Summary.Running != 2 || body.Summary.ByExecutor[""] != 2 || body.Summary.ByCluster[""] != 2 {
		t.Fatalf("summary = %+v", body.Summary)
	}
	ids := map[string]bool{}
	for _, r := range body.Runners {
		ids[r.AttemptID] = true
		if r.State != "DISPATCHED" || r.Executor != "" || r.Pod != "" || r.StartedAt != nil || r.PlacementReportedAt != nil {
			t.Errorf("unreported runner = %+v", r)
		}
	}
	if !ids[mineA] || !ids[mineB] || ids[theirs] || len(ids) != 2 {
		t.Fatalf("runners = %v; want exactly this namespace's two", ids)
	}

	// The limit bounds the page, never the summary; a larger value is capped,
	// a non-positive one refused.
	if err := json.Unmarshal(f.get(f.api(), f.ns, "/platform/task-runners?limit=1", f.keys["admin"], http.StatusOK), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Runners) != 1 || body.Summary.Running != 2 {
		t.Fatalf("limit=1: %d runners, running %d", len(body.Runners), body.Summary.Running)
	}
	f.get(f.api(), f.ns, "/platform/task-runners?limit=5000", f.keys["admin"], http.StatusOK)
	for _, bad := range []string{"0", "-1", "x"} {
		var e map[string]any
		_ = json.Unmarshal(f.get(f.api(), f.ns, "/platform/task-runners?limit="+bad, f.keys["admin"], http.StatusBadRequest), &e)
		if e["error"] != "invalid_limit" {
			t.Errorf("limit=%s: %v", bad, e)
		}
	}
	f.get(f.api(), f.ns, "/platform/task-runners", f.keys["reader"], http.StatusForbidden)
	f.get(f.api(), f.ns, "/platform/task-runners", other.keys["admin"], http.StatusForbidden)
}
