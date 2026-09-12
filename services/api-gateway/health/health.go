// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

// Package health provides the /healthz and /readyz endpoints every service must
// expose (ADR-0011 rule 3).
package health

import (
	"context"
	"encoding/json"
	"net/http"
	"sync/atomic"
	"time"
)

// Check reports whether one dependency is usable.
type Check func(ctx context.Context) error

// Handler serves liveness and readiness.
//
// The two are deliberately DIFFERENT, because conflating them causes outages. A
// liveness probe that checks the database will fail every replica when the database
// blips, and the orchestrator will then restart every replica simultaneously —
// turning a recoverable dependency failure into a full outage. So:
//
//   - /healthz is process liveness ONLY. It says "this process is not wedged".
//     Restarting on a failure here is correct.
//   - /readyz checks dependencies. A failure means "do not send me traffic yet",
//     which removes the replica from the load balancer WITHOUT killing it, so it
//     can rejoin the moment the dependency recovers.
type Handler struct {
	checks   map[string]Check
	draining atomic.Bool
	// Timeout bounds the readiness probe. An unbounded check would let a hung
	// database turn readiness itself into a hang, and the probe would time out at
	// the orchestrator with no diagnostic.
	Timeout time.Duration
}

// New builds a health handler.
func New(checks map[string]Check) *Handler {
	return &Handler{checks: checks, Timeout: 2 * time.Second}
}

// BeginDraining makes /readyz fail while the process keeps serving.
//
// This is what makes a graceful shutdown actually graceful: the replica is pulled
// from the load balancer first, drains its in-flight requests, and only then exits.
// Without it, shutdown races the load balancer's health check and clients see reset
// connections on every deploy.
func (h *Handler) BeginDraining() { h.draining.Store(true) }

// Routes registers both endpoints on a mux.
func (h *Handler) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /healthz", h.live)
	mux.HandleFunc("GET /readyz", h.ready)
}

func (h *Handler) live(w http.ResponseWriter, _ *http.Request) {
	// Deliberately checks NOTHING external. If this handler runs at all, the
	// process is alive.
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

func (h *Handler) ready(w http.ResponseWriter, r *http.Request) {
	if h.draining.Load() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"status": "draining",
			"detail": "shutting down; not accepting new traffic",
		})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), h.Timeout)
	defer cancel()

	results := make(map[string]string, len(h.checks))
	ok := true
	for name, check := range h.checks {
		if err := check(ctx); err != nil {
			// The error text is included: a readiness endpoint is read by an
			// operator during an incident, and "not ready" with no reason wastes
			// the minutes that matter most.
			results[name] = err.Error()
			ok = false
			continue
		}
		results[name] = "ok"
	}

	status := http.StatusOK
	state := "ready"
	if !ok {
		status = http.StatusServiceUnavailable
		state = "not_ready"
	}
	writeJSON(w, status, map[string]any{"status": state, "checks": results})
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}
