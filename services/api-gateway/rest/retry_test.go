// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package rest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/khuriwalgroup/kgf-nexus/internal/store"
)

// The retry handler, against the restDB fake. F100: the CLI and SDK sent
// {"nodes": [...]} and the gateway read nobody's body, so a node-level retry
// silently became a whole-run retry. These pin that the selection now reaches
// the owner, and that a bad selection is refused here, precisely, before it
// does.

const retryPath = "/v1/namespaces/team-ml/runs/run-1/retry"

func failedRun(db *restDB) {
	db.addRun("team-ml", "run-1", "FAILED", map[string]string{
		"extract": "SUCCEEDED", "transform": "FAILED", "load": "UPSTREAM_FAILED",
		"audit": "CANCELLED", "report": "SKIPPED", "notify": "PENDING",
	})
}

func okRetry(nodes []string) (store.RunRow, error) {
	return store.RunRow{ID: "run-1", Namespace: "team-ml", State: "RUNNING"}, nil
}

func decodeBody(t *testing.T, raw string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("decode %q: %v", raw, err)
	}
	return m
}

func TestRetrySendsTheSelectionToTheOwner(t *testing.T) {
	h, db, wfm := newTestAPI(t)
	failedRun(db)
	wfm.retryFn = okRetry

	rec := post(t, h, retryPath, adminKey, "", `{"nodes":["transform","audit","transform"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200. body=%s", rec.Code, rec.Body)
	}
	if len(wfm.retries) != 1 {
		t.Fatalf("owner saw %d retries, want 1", len(wfm.retries))
	}
	got := wfm.retries[0]
	// Sorted and de-duplicated: the same request always makes the same call.
	if !reflect.DeepEqual(got.nodes, []string{"audit", "transform"}) {
		t.Errorf("owner received nodes %v, want [audit transform]", got.nodes)
	}
	if got.ns != "team-ml" || got.id != "run-1" {
		t.Errorf("owner received %s/%s", got.ns, got.id)
	}
	if got.actor == "" {
		t.Error("retry reached the owner with no actor; the run history could not say who asked")
	}
	body := decodeBody(t, rec.Body.String())
	if body["state"] != "RUNNING" {
		t.Errorf("state = %v, want RUNNING", body["state"])
	}
	if fmt.Sprint(body["retried_nodes"]) != "[audit transform]" {
		t.Errorf("retried_nodes = %v", body["retried_nodes"])
	}
}

func TestRetryWithoutABodyIsAWholeRunRetry(t *testing.T) {
	for _, body := range []string{"", "{}", `{"nodes":[]}`} {
		h, db, wfm := newTestAPI(t)
		failedRun(db)
		wfm.retryFn = okRetry
		rec := post(t, h, retryPath, adminKey, "", body)
		if rec.Code != http.StatusOK {
			t.Fatalf("body %q: status %d, want 200. body=%s", body, rec.Code, rec.Body)
		}
		if len(wfm.retries) != 1 || len(wfm.retries[0].nodes) != 0 {
			t.Errorf("body %q: owner saw %+v, want one whole-run retry", body, wfm.retries)
		}
	}
}

func TestRetryRefusesABadSelectionBeforeReachingTheOwner(t *testing.T) {
	cases := []struct {
		name, body string
		wantStatus int
		wantCode   string
		wantNodes  string
	}{
		{"unknown field", `{"node":["transform"]}`, 400, "invalid_body", ""},
		{"not a list", `{"nodes":"transform"}`, 400, "invalid_body", ""},
		{"empty id", `{"nodes":["transform",""]}`, 400, "invalid_nodes", ""},
		{"padded id", `{"nodes":[" transform"]}`, 400, "invalid_nodes", ""},
		{"unknown node", `{"nodes":["transform","nope","zzz"]}`, 422, "unknown_nodes", "[nope zzz]"},
		{"succeeded node", `{"nodes":["extract","transform"]}`, 409, "nodes_not_retryable", "extract"},
		{"skipped node", `{"nodes":["report"]}`, 409, "nodes_not_retryable", "report"},
		{"pending node", `{"nodes":["notify"]}`, 409, "nodes_not_retryable", "notify"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h, db, wfm := newTestAPI(t)
			failedRun(db)
			wfm.retryFn = okRetry
			rec := post(t, h, retryPath, adminKey, "", c.body)
			if rec.Code != c.wantStatus {
				t.Fatalf("status %d, want %d. body=%s", rec.Code, c.wantStatus, rec.Body)
			}
			body := decodeBody(t, rec.Body.String())
			if body["error"] != c.wantCode {
				t.Errorf("error = %v, want %s", body["error"], c.wantCode)
			}
			if c.wantNodes != "" && !strings.Contains(fmt.Sprint(body["nodes"]), c.wantNodes) {
				t.Errorf("nodes = %v, want it to name %s", body["nodes"], c.wantNodes)
			}
			if len(wfm.retries) != 0 {
				t.Errorf("a refused selection reached the owner: %+v", wfm.retries)
			}
		})
	}
}

func TestRetryOfARunInAnotherNamespaceIsNotFound(t *testing.T) {
	h, db, wfm := newTestAPI(t)
	db.addRun("other-team", "run-1", "FAILED", map[string]string{"a": "FAILED"})
	wfm.retryFn = okRetry
	rec := post(t, h, retryPath, adminKey, "", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status %d, want 404. body=%s", rec.Code, rec.Body)
	}
	if len(wfm.retries) != 0 {
		t.Error("a retry of another namespace's run reached the owner")
	}
}

func TestRetryNeedsWriteAccess(t *testing.T) {
	h, db, wfm := newTestAPI(t)
	failedRun(db)
	wfm.retryFn = okRetry
	rec := post(t, h, retryPath, readerKey, "", `{"nodes":["transform"]}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("reader retry: status %d, want 403", rec.Code)
	}
	if len(wfm.retries) != 0 {
		t.Error("a reader's retry reached the owner")
	}
}

// Each of the owner's answers gets the HTTP status that tells the operator what
// to do next.
func TestRetryMapsTheOwnersAnswers(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
	}{
		{"state conflict", fmt.Errorf("%w: retry refused: node load would fail again", store.ErrRunStateConflict), 409, "retry_refused"},
		{"unknown nodes", fmt.Errorf("%w: unknown nodes: x", store.ErrInvalidRequest), 422, "unknown_nodes"},
		{"lost race", fmt.Errorf("RetryRun run-1: %w", status.Error(codes.Aborted, "node moved")), 409, "retry_conflict"},
		{"old engine", fmt.Errorf("RetryRun run-1: %w", status.Error(codes.Unimplemented, "no ResetNodes")), 501, "retry_unavailable"},
		{"outcome unknown", fmt.Errorf("RetryRun run-1: %w", status.Error(codes.Unavailable, "engine timed out")), 503, "retry_outcome_unknown"},
		{"not found", store.ErrNotFound, 404, "not_found"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h, db, wfm := newTestAPI(t)
			failedRun(db)
			wfm.retryFn = func([]string) (store.RunRow, error) { return store.RunRow{}, c.err }
			rec := post(t, h, retryPath, adminKey, "", `{"nodes":["transform"]}`)
			if rec.Code != c.wantStatus {
				t.Fatalf("status %d, want %d. body=%s", rec.Code, c.wantStatus, rec.Body)
			}
			if body := decodeBody(t, rec.Body.String()); body["error"] != c.wantCode {
				t.Errorf("error = %v, want %s", body["error"], c.wantCode)
			}
		})
	}
}
