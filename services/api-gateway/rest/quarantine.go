// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package rest

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/khuriwalgroup/kgf-nexus/internal/store"
	"github.com/khuriwalgroup/kgf-nexus/services/api-gateway/authn"
)

// WorkerQuarantiner is the resource-manager's quarantine API as the gateway
// needs it. Discovered on the ResourceManagerAPI by type assertion rather than
// added to that interface, so every existing implementation (and test fake)
// keeps compiling and a gateway wired to a client without it answers 501.
type WorkerQuarantiner interface {
	QuarantineWorker(ctx context.Context, namespace, workerID, reason, actor string, duration time.Duration) (applied bool, err error)
	ReleaseWorker(ctx context.Context, namespace, workerID, reason, actor string) (applied bool, err error)
}

// CacheInvalidator records an operator's memo invalidation (§6.4). The DAG
// engine owns memo_invalidations, so this is a call, not a write.
type CacheInvalidator interface {
	InvalidateCache(ctx context.Context, inv store.MemoInvalidation) (id int64, at time.Time, err error)
}

// maxQuarantine bounds an operator-set duration. Indefinite is expressed as 0,
// not as a very large number, so the record says what was meant.
const maxQuarantine = 30 * 24 * time.Hour

// actorOf renders the principal for the attribution columns of the records
// these handlers cause (worker_quarantines.actor, memo_invalidations.actor).
func actorOf(p authn.Principal) string {
	if p.UserID != "" {
		return "user:" + p.UserID
	}
	if p.KeyID != "" {
		return "key:" + p.KeyID
	}
	return "principal:" + p.Name
}

type quarantineRequest struct {
	Reason string `json:"reason"`
	// DurationMs is how long before the quarantine lifts by itself; 0 or absent
	// means indefinite.
	DurationMs int64 `json:"duration_ms"`
}

// quarantineWorker takes a worker out of the placement pool (§5.5). Work it
// already holds finishes; it simply receives nothing new until released or
// until the duration elapses.
func (a *API) quarantineWorker(w http.ResponseWriter, r *http.Request, p authn.Principal) {
	q, ok := a.rm.(WorkerQuarantiner)
	if !ok {
		writeError(w, http.StatusNotImplemented, "not_implemented", "worker quarantine is not wired in this gateway")
		return
	}
	ns, id := r.PathValue("ns"), r.PathValue("id")
	body, err := readBody(r, 16<<10)
	if err != nil {
		writeBodyError(w, err)
		return
	}
	var req quarantineRequest
	if err := json.Unmarshal(nonEmptyJSON(body), &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	req.Reason = strings.TrimSpace(req.Reason)
	if req.Reason == "" || len(req.Reason) > 1000 {
		writeError(w, http.StatusBadRequest, "invalid_reason", "reason is required (1-1000 characters): a quarantine nobody can explain later is one nobody dares release")
		return
	}
	dur := time.Duration(req.DurationMs) * time.Millisecond
	if req.DurationMs < 0 || dur > maxQuarantine {
		writeError(w, http.StatusBadRequest, "invalid_duration", "duration_ms must be between 0 (indefinite) and 30 days")
		return
	}
	if _, err := store.GatewayGetWorker(r.Context(), a.db, ns, id); err != nil {
		a.fail(w, r, "get worker", err)
		return
	}
	applied, err := q.QuarantineWorker(r.Context(), ns, id, req.Reason, actorOf(p), dur)
	if err != nil {
		a.auditPrincipal(r, p, "worker.quarantine", "worker", id, "ERROR", map[string]any{"reason": req.Reason})
		a.fail(w, r, "quarantine worker", err)
		return
	}
	outcome := "OK"
	if !applied {
		outcome = "NOOP"
	}
	a.auditPrincipal(r, p, "worker.quarantine", "worker", id, outcome,
		map[string]any{"reason": req.Reason, "duration_ms": req.DurationMs})
	if !applied {
		writeError(w, http.StatusConflict, "not_active", "only an ACTIVE worker can be quarantined")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"worker_id": id, "state": "QUARANTINED", "duration_ms": req.DurationMs})
}

// releaseWorker returns a QUARANTINED worker to the pool, closing every open
// episode for its host.
func (a *API) releaseWorker(w http.ResponseWriter, r *http.Request, p authn.Principal) {
	q, ok := a.rm.(WorkerQuarantiner)
	if !ok {
		writeError(w, http.StatusNotImplemented, "not_implemented", "worker quarantine is not wired in this gateway")
		return
	}
	ns, id := r.PathValue("ns"), r.PathValue("id")
	body, err := readBody(r, 16<<10)
	if err != nil {
		writeBodyError(w, err)
		return
	}
	var req quarantineRequest
	if err := json.Unmarshal(nonEmptyJSON(body), &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if _, err := store.GatewayGetWorker(r.Context(), a.db, ns, id); err != nil {
		a.fail(w, r, "get worker", err)
		return
	}
	applied, err := q.ReleaseWorker(r.Context(), ns, id, strings.TrimSpace(req.Reason), actorOf(p))
	if err != nil {
		a.fail(w, r, "release worker", err)
		return
	}
	outcome := "OK"
	if !applied {
		outcome = "NOOP"
	}
	a.auditPrincipal(r, p, "worker.release", "worker", id, outcome, map[string]any{"reason": req.Reason})
	if !applied {
		writeError(w, http.StatusConflict, "not_quarantined", "the worker is not QUARANTINED")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"worker_id": id, "state": "ACTIVE"})
}

func (a *API) listWorkerQuarantines(w http.ResponseWriter, r *http.Request, _ authn.Principal) {
	ns, id := r.PathValue("ns"), r.PathValue("id")
	if _, err := store.GatewayGetWorker(r.Context(), a.db, ns, id); err != nil {
		a.fail(w, r, "get worker", err)
		return
	}
	rows, err := store.GatewayListWorkerQuarantines(r.Context(), a.db, ns, id)
	if err != nil {
		a.fail(w, r, "list worker quarantines", err)
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, q := range rows {
		out = append(out, map[string]any{
			"id": q.ID, "trigger": q.Trigger, "reason": q.Reason, "evidence": rawJSON(q.Evidence),
			"actor": q.Actor, "started_at": q.StartedAt, "expires_at": q.ExpiresAt,
			"released_at": q.ReleasedAt, "released_by": q.ReleasedBy, "release_reason": q.ReleaseReason,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"quarantines": out})
}

type invalidateRequest struct {
	TaskRef     string `json:"task_ref"`
	TaskVersion *int32 `json:"task_version"`
	CacheKey    string `json:"cache_key"`
	Reason      string `json:"reason"`
}

// invalidateCache stops memoized results from being reused (§6.4). It removes
// nothing: results produced before the cutoff simply stop being eligible, and
// the next run of the task executes it.
func (a *API) invalidateCache(w http.ResponseWriter, r *http.Request, p authn.Principal) {
	if a.cache == nil {
		writeError(w, http.StatusNotImplemented, "not_implemented", "cache invalidation is not wired in this gateway")
		return
	}
	body, err := readBody(r, 16<<10)
	if err != nil {
		writeBodyError(w, err)
		return
	}
	var req invalidateRequest
	if err := json.Unmarshal(nonEmptyJSON(body), &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if req.TaskRef == "" && req.CacheKey == "" {
		writeError(w, http.StatusBadRequest, "invalid_selector", "task_ref or cache_key is required")
		return
	}
	if req.TaskVersion != nil && req.TaskRef == "" {
		writeError(w, http.StatusBadRequest, "invalid_selector", "task_version needs a task_ref")
		return
	}
	ns := r.PathValue("ns")
	id, at, err := a.cache.InvalidateCache(r.Context(), store.MemoInvalidation{
		Namespace: ns, TaskRef: req.TaskRef, TaskVersion: req.TaskVersion, CacheKey: req.CacheKey,
		Reason: strings.TrimSpace(req.Reason), Actor: actorOf(p),
	})
	if err != nil {
		a.auditPrincipal(r, p, "cache.invalidate", "task_ref", req.TaskRef, "ERROR", nil)
		if errors.Is(err, context.DeadlineExceeded) {
			writeError(w, http.StatusServiceUnavailable, "unavailable", "the DAG engine did not answer")
			return
		}
		a.fail(w, r, "invalidate cache", err)
		return
	}
	a.auditPrincipal(r, p, "cache.invalidate", "task_ref", req.TaskRef, "OK", map[string]any{
		"task_version": req.TaskVersion, "cache_key": req.CacheKey, "reason": req.Reason, "invalidation_id": id,
	})
	writeJSON(w, http.StatusOK, map[string]any{"invalidation_id": id, "invalidated_at": at})
}

// WithCacheInvalidator wires memo invalidation, enabling its route.
func (a *API) WithCacheInvalidator(c CacheInvalidator) *API {
	a.cache = c
	return a
}
