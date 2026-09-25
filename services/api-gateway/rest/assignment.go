// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package rest

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/google/uuid"
	"github.com/khuriwalgroup/kgf-nexus/internal/domain"
	"github.com/khuriwalgroup/kgf-nexus/internal/store"
	"github.com/khuriwalgroup/kgf-nexus/services/api-gateway/authn"
)

// GET /v1/namespaces/{ns}/runs/{run}/attempts/{id}/assignment
//
// The resolved inputs and assignment of one attempt, so `nexusctl replay` can
// re-run a failed task on a laptop with exactly what the worker was given
// (ADR-0022). {run} may be "-" (any run of the namespace, the AIP-159 wildcard)
// because an operator usually holds only the attempt id from a log or an alert.
//
// What is NOT in it, deliberately:
//   - the lease token and lease expiry: a fencing credential for the live
//     attempt, useless to a replay and dangerous in a file on a laptop;
//   - secret VALUES: the spec only ever holds SecretRefs (name + provider URI),
//     which are passed through as references and re-emitted with nothing but
//     those two fields; resolving them is the replaying user's business;
//   - blob payloads: a blob input stays a reference (bucket/key/sha256) --
//     the gateway never reads task payloads (invariant 6).
//
// The idempotency key IS included, and a replay should use it: a handler that
// passes it to a downstream system is deduplicated there, whereas a fresh key
// would make the replay a second, distinct effect.

// assignmentRoute is the replay endpoint's pattern; its permissions are in
// rbac_routes.go with every other route's.
const assignmentRoute = "GET /v1/namespaces/{ns}/runs/{run}/attempts/{id}/assignment"

// AssignmentReader reads an attempt's assignment, namespace-scoped. Satisfied
// by the store; an interface so the handler is testable without Postgres.
type AssignmentReader interface {
	GetAttemptAssignment(ctx context.Context, namespace, runID, attemptID string) (store.AttemptAssignmentRow, error)
}

type storeAssignments struct{ db store.GWQuerier }

func (s storeAssignments) GetAttemptAssignment(ctx context.Context, ns, runID, attemptID string) (store.AttemptAssignmentRow, error) {
	return store.GatewayGetAttemptAssignment(ctx, s.db, ns, runID, attemptID)
}

// WithAssignmentReader replaces the store-backed reader (tests).
func (a *API) WithAssignmentReader(r AssignmentReader) *API {
	a.assignments = r
	return a
}

// WithInlineThreshold records the inline-versus-blob boundary the gateway
// hands agents (ADR-0016), so a replayed attempt stores its results the way the
// platform would. Zero means "not configured" and reports the default.
func (a *API) WithInlineThreshold(n int64) *API {
	a.inlineThreshold = n
	return a
}

func (a *API) attemptAssignment(w http.ResponseWriter, r *http.Request, _ authn.Principal) {
	ns, runID, attemptID := r.PathValue("ns"), r.PathValue("run"), r.PathValue("id")
	// Malformed ids are "not found", never a 500 from a failed uuid cast.
	if _, err := uuid.Parse(attemptID); err != nil {
		writeError(w, http.StatusNotFound, "not_found", "not found")
		return
	}
	if runID == "-" {
		runID = ""
	} else {
		if _, err := uuid.Parse(runID); err != nil {
			writeError(w, http.StatusNotFound, "not_found", "not found")
			return
		}
		// Namespace check BEFORE the read (CONTRACT-FINDINGS F101): the run in
		// the path must belong to the path's namespace -- the only namespace the
		// guard authorized -- or nothing about it is read.
		if _, err := store.GatewayGetRun(r.Context(), a.db, ns, runID); err != nil {
			a.fail(w, r, "get run", err)
			return
		}
	}
	reader := a.assignments
	if reader == nil {
		reader = storeAssignments{db: a.db}
	}
	// The read itself is namespace-scoped too (the predicate joins the run's
	// namespace), so the "-" wildcard cannot reach another tenant's attempt.
	row, err := reader.GetAttemptAssignment(r.Context(), ns, runID, attemptID)
	if err != nil {
		a.fail(w, r, "get attempt assignment", err)
		return
	}

	var spec map[string]any
	if err := json.Unmarshal(orEmptyObject(row.SpecJSON), &spec); err != nil {
		a.fail(w, r, "decode task spec", err)
		return
	}
	secrets := scrubSecrets(spec)
	var inputs map[string]json.RawMessage
	if err := json.Unmarshal(orEmptyObject(row.InputRefsJSON), &inputs); err != nil {
		a.fail(w, r, "decode input refs", err)
		return
	}
	if inputs == nil {
		inputs = map[string]json.RawMessage{}
	}

	assignment := map[string]any{
		"attempt_id":      row.AttemptID,
		"run_id":          row.RunID,
		"node_id":         row.NodeID,
		"attempt_number":  row.AttemptNumber,
		"namespace":       ns,
		"idempotency_key": row.IdempotencyKey,
		"spec":            spec,
		"inputs":          inputs,
	}
	attempt := map[string]any{
		"id": row.AttemptID, "run_id": row.RunID, "node_id": row.NodeID,
		"attempt_number": row.AttemptNumber, "state": row.State,
	}
	if row.MapIndex != nil {
		assignment["map_index"] = *row.MapIndex
		attempt["map_index"] = *row.MapIndex
	}
	if row.ParentNodeID != nil {
		attempt["parent_node_id"] = *row.ParentNodeID
	}
	threshold := a.inlineThreshold
	if threshold <= 0 {
		threshold = domain.DefaultInlineThresholdBytes
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"namespace":  ns,
		"workflow":   map[string]any{"name": row.SpecName, "version": row.SpecVersion},
		"attempt":    attempt,
		"assignment": assignment,
		// Inline ResultRef payloads are base64 (the proto3 JSON mapping of
		// bytes); int64 fields may be JSON strings. Stated so a client never has
		// to guess which encoding a string is in.
		"encoding":     "protojson",
		"result_store": map[string]any{"inline_threshold_bytes": domain.ClampInlineThreshold(threshold)},
		"secrets":      secrets,
		"omitted":      []string{"lease_token", "lease_expires_at_unix_ms", "start_to_close_deadline_unix_ms"},
	})
}

// scrubSecrets reduces runtime.secrets to references -- name and ref, nothing
// else -- and returns them. A SecretRef has no value field today; this keeps it
// that way at the one place a spec leaves the control plane in full.
func scrubSecrets(spec map[string]any) []map[string]string {
	out := []map[string]string{}
	rt, _ := spec["runtime"].(map[string]any)
	if rt == nil {
		return out
	}
	raw, _ := rt["secrets"].([]any)
	clean := make([]any, 0, len(raw))
	for _, s := range raw {
		m, _ := s.(map[string]any)
		if m == nil {
			continue
		}
		name, _ := m["name"].(string)
		ref, _ := m["ref"].(string)
		entry := map[string]string{"name": name, "ref": ref}
		out = append(out, entry)
		clean = append(clean, map[string]any{"name": name, "ref": ref})
	}
	if raw != nil {
		rt["secrets"] = clean
	}
	return out
}

func orEmptyObject(b []byte) []byte {
	if len(b) == 0 {
		return []byte("{}")
	}
	return b
}
