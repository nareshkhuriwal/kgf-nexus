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
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/khuriwalgroup/kgf-nexus/internal/store"
	"github.com/khuriwalgroup/kgf-nexus/services/api-gateway/authn"
)

// TestIntegrationFairShareWeightNeedsPlatformOperator pins ADR-0017's one
// authorization rule: a namespace admin can save its platform policy, but
// changing its OWN fair-share weight takes dispatch share from every other
// namespace, so that one field needs a platform operator. It also pins that a
// client which omits the field (pre-ADR-0017) is treated as "leave the default"
// rather than rejected, and that the saved weight is what the scheduler's view
// reads back.
func TestIntegrationFairShareWeightNeedsPlatformOperator(t *testing.T) {
	ctx := context.Background()
	tx, err := attentionPool(t).Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	ns := fmt.Sprintf("fs-weight-%d", time.Now().UnixNano())
	if _, err = tx.Exec(ctx, `INSERT INTO tenants(namespace,name) VALUES($1,'weight test')`, ns); err != nil {
		t.Fatal(err)
	}
	key := "nx_fs_weight_" + ns
	if _, err = store.GatewayCreateAPIKey(ctx, tx, ns, "test", "admin", key); err != nil {
		t.Fatal(err)
	}
	auth := authn.New(tx).WithRBAC().WithIdentity(authn.StoreIdentity{DB: tx})
	h := New(tx, auth, nil, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil))).WithIdentity(NewStoreIdentityAdmin(tx)).Routes()
	path := "/v1/namespaces/" + ns + "/platform/config"

	put := func(body string, want int) map[string]any {
		t.Helper()
		r := httptest.NewRequest("PUT", path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+key)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("PUT %s: got %d want %d: %s", body, w.Code, want, w.Body)
		}
		out := map[string]any{}
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return out
	}
	sched := func(extra string) string {
		return `{"min_replicas":1,"max_replicas":4,"target_cpu_percent":65,"scale_down_seconds":300` + extra + `}`
	}

	// Omitted weight: an old console. Accepted, stored as the default 1.
	saved := put(`{"revision":0,"scheduler":`+sched("")+`,"pools":[],"profiles":[]}`, 200)
	if w := saved["scheduler"].(map[string]any)["fair_share_weight"]; w != float64(1) {
		t.Fatalf("omitted weight saved as %v, want the default 1", w)
	}
	// Unchanged weight: an ordinary policy save by a namespace admin.
	put(`{"revision":1,"scheduler":`+sched(`,"fair_share_weight":1`)+`,"pools":[],"profiles":[]}`, 200)
	// Raised weight by the same admin API key: refused, it is not an operator.
	put(`{"revision":2,"scheduler":`+sched(`,"fair_share_weight":50`)+`,"pools":[],"profiles":[]}`, 403)
	// Out of range is a validation error before authorization is even asked.
	put(`{"revision":2,"scheduler":`+sched(`,"fair_share_weight":101`)+`,"pools":[],"profiles":[]}`, 400)

	// The view the scheduler reads agrees with what the API stored.
	var weight int
	if err := tx.QueryRow(ctx, `SELECT weight FROM scheduler_namespace_weights WHERE namespace=$1`, ns).Scan(&weight); err != nil {
		t.Fatal(err)
	}
	if weight != 1 {
		t.Fatalf("scheduler view reads weight %d, want 1 (the refused raise must not have landed)", weight)
	}
}
