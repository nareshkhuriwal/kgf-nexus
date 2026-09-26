// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package rest

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"unicode"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/khuriwalgroup/kgf-nexus/internal/dag"
	"github.com/khuriwalgroup/kgf-nexus/internal/store"
	"github.com/khuriwalgroup/kgf-nexus/services/api-gateway/authn"
)

// maxRetryNodes bounds one retry's selection. Generous -- an expanded map can
// have thousands of children -- but finite, so a request body cannot make the
// gateway and the engine each walk an unbounded list.
const maxRetryNodes = 10000

// retryRequest is POST .../runs/{id}/retry. Every field is optional: an empty
// body is a whole-run retry, which is what the endpoint always meant.
type retryRequest struct {
	// Nodes selects the nodes to retry; their non-succeeded dependents are
	// retried with them and succeeded upstream work is never re-run.
	Nodes []string `json:"nodes"`
}

// retryRun retries a failed run, whole or from selected nodes.
//
// WHY THIS IS NOT a.lifecycle. Until F100 it was, and lifecycle takes a run id and
// nothing else -- so the `nodes` the CLI and SDK send were read by nobody, and a
// "retry these two nodes" silently became "retry the whole run". This handler
// reads the body, and reads it STRICTLY: an unknown field is a 400, because a
// silently ignored field is exactly the failure being fixed.
//
// The selection is checked here against the run's current nodes before anything
// is asked of the owner, so the common mistakes (a typo'd id, a node that
// succeeded) come back as a precise 4xx naming the offending nodes. The engine
// checks again, authoritatively, inside its transaction; this check is for the
// operator's benefit and a race between the two can only produce a refusal
// there, never a partial retry.
func (a *API) retryRun(w http.ResponseWriter, r *http.Request, p authn.Principal) {
	ns, id := r.PathValue("ns"), r.PathValue("id")

	body, err := readBody(r, 1<<20)
	if err != nil {
		writeBodyError(w, err)
		return
	}
	var req retryRequest
	dec := json.NewDecoder(bytes.NewReader(nonEmptyJSON(body)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body",
			fmt.Sprintf("retry body must be {\"nodes\": [\"<node id>\", ...]} or empty: %v", err))
		return
	}
	nodes, verr := normalizeRetryNodes(req.Nodes)
	if verr != nil {
		writeError(w, http.StatusBadRequest, "invalid_nodes", verr.Error())
		return
	}

	// Namespace-scoped existence check BEFORE the mutation: the owner acts on a
	// run id alone, so without this a principal of namespace A could retry a run
	// of namespace B by knowing its UUID.
	run, err := store.GatewayGetRun(r.Context(), a.db, ns, id)
	if err != nil {
		a.fail(w, r, "get run", err)
		return
	}

	audit := map[string]any{"nodes": nodes}
	if len(nodes) > 0 {
		if !a.checkRetrySelection(w, r, p, run.ID, nodes, audit) {
			return
		}
	}

	run, err = a.wfm.RetryRun(r.Context(), ns, id, nodes, actorOf(p))
	if err != nil {
		a.auditPrincipal(r, p, "run.retry", "run", ns+"/"+id, store.AuditError,
			withDetail(audit, "error", err.Error()))
		a.failRetry(w, r, err)
		return
	}
	a.auditPrincipal(r, p, "run.retry", "run", ns+"/"+id, store.AuditSuccess, audit)

	out := runPayload(run)
	out["retried_nodes"] = nodes
	writeJSON(w, http.StatusOK, out)
}

// checkRetrySelection rejects unknown and non-retryable nodes with a response
// that names each one. It reports whether the request may proceed.
func (a *API) checkRetrySelection(w http.ResponseWriter, r *http.Request, p authn.Principal,
	runID string, nodes []string, audit map[string]any) bool {
	rows, err := store.GatewayListRunNodes(r.Context(), a.db, runID)
	if err != nil {
		a.fail(w, r, "list run nodes", err)
		return false
	}
	state := make(map[string]string, len(rows))
	for _, n := range rows {
		state[n.NodeID] = n.State
	}

	var unknown []string
	var wrong []map[string]string
	for _, id := range nodes {
		s, ok := state[id]
		switch {
		case !ok:
			unknown = append(unknown, id)
		case !dag.RetryableSelection(dag.NodeState(s)):
			wrong = append(wrong, map[string]string{"node_id": id, "state": s})
		}
	}
	target := r.PathValue("ns") + "/" + r.PathValue("id")
	if len(unknown) > 0 {
		a.auditPrincipal(r, p, "run.retry", "run", target, store.AuditError, withDetail(audit, "unknown_nodes", unknown))
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error":   "unknown_nodes",
			"message": "this run has no node named " + strings.Join(unknown, ", "),
			"nodes":   unknown,
		})
		return false
	}
	if len(wrong) > 0 {
		a.auditPrincipal(r, p, "run.retry", "run", target, store.AuditError, withDetail(audit, "not_retryable", wrong))
		parts := make([]string, len(wrong))
		for i, n := range wrong {
			parts[i] = n["node_id"] + " is " + n["state"]
		}
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": "nodes_not_retryable",
			"message": "only FAILED, UPSTREAM_FAILED and CANCELLED nodes can be retried; " +
				strings.Join(parts, ", "),
			"nodes": wrong,
		})
		return false
	}
	return true
}

// failRetry maps the owner's answer onto HTTP. Each refusal gets its own code
// because each asks something different of the operator: fix the request (422),
// accept that the run cannot be retried like that (409), try again (409
// retry_conflict), upgrade the engine (501), or re-read the run (503).
func (a *API) failRetry(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "not found")
	case errors.Is(err, store.ErrInvalidRequest):
		writeError(w, http.StatusUnprocessableEntity, "unknown_nodes", err.Error())
	case errors.Is(err, store.ErrRunStateConflict):
		writeError(w, http.StatusConflict, "retry_refused", err.Error())
	case status.Code(err) == codes.Aborted:
		writeError(w, http.StatusConflict, "retry_conflict",
			"the run changed while the retry was being applied and nothing was retried; re-read it and try again")
	case status.Code(err) == codes.Unimplemented:
		writeError(w, http.StatusNotImplemented, "retry_unavailable", err.Error())
	case status.Code(err) == codes.Unavailable, status.Code(err) == codes.DeadlineExceeded:
		// Not "failed": the owner may have committed the retry before the answer
		// was lost, and the run converges on the truth either way. Telling the
		// operator to retry blindly would be wrong in half the cases.
		writeError(w, http.StatusServiceUnavailable, "retry_outcome_unknown",
			"the retry may or may not have been applied; re-read the run before retrying again")
	default:
		a.fail(w, r, "retry run", err)
	}
}

// normalizeRetryNodes validates a selection and returns it sorted and
// de-duplicated, so the same request always produces the same audit record and
// the same engine call.
func normalizeRetryNodes(in []string) ([]string, error) {
	if len(in) > maxRetryNodes {
		return nil, fmt.Errorf("a retry may name at most %d nodes, got %d", maxRetryNodes, len(in))
	}
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, id := range in {
		if strings.TrimSpace(id) == "" {
			return nil, errors.New("node ids must be non-empty")
		}
		if id != strings.TrimSpace(id) || strings.IndexFunc(id, unicode.IsControl) >= 0 {
			return nil, fmt.Errorf("node id %q contains whitespace or control characters", id)
		}
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out, nil
}

// withDetail returns a copy of an audit detail map with one more key.
func withDetail(base map[string]any, k string, v any) map[string]any {
	out := make(map[string]any, len(base)+1)
	for bk, bv := range base {
		out[bk] = bv
	}
	out[k] = v
	return out
}
