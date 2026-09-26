// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package rest

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/khuriwalgroup/kgf-nexus/internal/store"
)

const (
	// maxSpecBytes bounds a spec submission. Generous enough for a large generated
	// workflow, bounded so an unauthenticated-sized body cannot be used to exhaust
	// gateway memory. The limit is enforced by MaxBytesReader rather than by
	// checking Content-Length, which a client controls and can lie about.
	maxSpecBytes = 4 << 20
	// maxRunBytes bounds run params. Task PAYLOADS never transit the control plane
	// (invariant 6) — params are a small binding of workflow inputs, so a large
	// body here means the caller is misusing the field.
	maxRunBytes = 256 << 10

	sseInterval  = 500 * time.Millisecond
	sseKeepalive = 20 * time.Second
	sseBatch     = 200
)

func writeJSON(w http.ResponseWriter, status int, payload any) {
	body, err := json.Marshal(payload)
	if err != nil {
		// Marshalling our own response failed, which is a programming error. The
		// status line is already decided, so emit a minimal valid body rather than
		// a half-written one.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"internal","message":"could not encode response"}`))
		return
	}
	writeRaw(w, status, body)
}

// writeRaw emits an already-encoded body, which is how a cached idempotent
// response is replayed BYTE-FOR-BYTE rather than re-derived. Re-deriving it could
// produce a different answer if the run has since changed state, and that would
// defeat the point of treating the retry as the original call.
func writeRaw(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{"error": code, "message": message})
}

// bodyTooLargeError is a request body over its route's limit.
//
// A distinct type because the two ways a body read fails have different
// fixes: over the limit, the client must send less (413); a read that failed
// part way -- a reset connection, a truncated chunked encoding -- is a
// transport problem the client may simply retry (400). The gateway used to
// report both as "exceeds N bytes or could not be read" and answer 413 for
// either, and the routes that decoded straight from the request answered an
// oversized body with 400 instead.
type bodyTooLargeError struct{ limit int64 }

func (e *bodyTooLargeError) Error() string {
	return fmt.Sprintf("request body exceeds %d bytes", e.limit)
}

// isBodyTooLarge reports whether err is a request body over its limit, from
// readBody or from a decoder reading through http.MaxBytesReader.
func isBodyTooLarge(err error) bool {
	var tl *bodyTooLargeError
	var mbe *http.MaxBytesError
	return errors.As(err, &tl) || errors.As(err, &mbe)
}

// bodyError normalises a body read or decode failure: an over-size read
// becomes *bodyTooLargeError whatever produced it, so every caller reports the
// limit the same way.
func bodyError(err error) error {
	var mbe *http.MaxBytesError
	if errors.As(err, &mbe) {
		return &bodyTooLargeError{limit: mbe.Limit}
	}
	return err
}

// readBody reads a bounded request body. An over-size body is a
// *bodyTooLargeError; any other failure is a read error.
func readBody(r *http.Request, limit int64) ([]byte, error) {
	body, err := io.ReadAll(http.MaxBytesReader(nil, r.Body, limit))
	if err != nil {
		if isBodyTooLarge(err) {
			return nil, &bodyTooLargeError{limit: limit}
		}
		return nil, fmt.Errorf("request body could not be read: %w", err)
	}
	return body, nil
}

// writeBodyError answers a failed body read: 413 body_too_large for a body
// over the limit, 400 unreadable_body for anything else. Every route answers
// these two the same way; writeBodyErrorAs keeps the two routes whose 413
// carried a route-specific code from before this was unified.
func writeBodyError(w http.ResponseWriter, err error) {
	writeBodyErrorAs(w, err, "body_too_large")
}

func writeBodyErrorAs(w http.ResponseWriter, err error, tooLargeCode string) {
	err = bodyError(err)
	if isBodyTooLarge(err) {
		writeError(w, http.StatusRequestEntityTooLarge, tooLargeCode, err.Error())
		return
	}
	writeError(w, http.StatusBadRequest, "unreadable_body", err.Error())
}

// rejectBody answers a body a decoder refused: 413 when it was over the limit,
// otherwise the caller's own status, code and message for a malformed body.
func rejectBody(w http.ResponseWriter, err error, status int, code, message string) {
	if isBodyTooLarge(err) {
		writeBodyError(w, err)
		return
	}
	writeError(w, status, code, message)
}

// nonEmptyJSON substitutes an empty object for an empty body, so a POST with no
// body is treated as "no fields" rather than as a parse error. The distinction
// matters for the lifecycle endpoints, which legitimately take no body.
func nonEmptyJSON(b []byte) []byte {
	if len(strings.TrimSpace(string(b))) == 0 {
		return []byte("{}")
	}
	return b
}

func contentType(r *http.Request) string {
	ct := r.Header.Get("Content-Type")
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	return strings.ToLower(strings.TrimSpace(ct))
}

func wantsSSE(r *http.Request) bool {
	if r.URL.Query().Get("stream") == "true" {
		return true
	}
	return strings.Contains(strings.ToLower(r.Header.Get("Accept")), "text/event-stream")
}

// listLimit reads a list endpoint's `limit` query parameter.
//
// One rule for every list endpoint (CONTRACT-FINDINGS F155):
//
//   - absent: def;
//   - a positive integer: that many, CAPPED at max -- a client asking for more
//     than the page size gets the largest page, never a smaller default one
//     (limit=600 on the run list used to answer 100 rows, fewer than limit=101);
//   - anything else (zero, negative, not an integer): 400 invalid_limit. These
//     used to fall back to the default silently, which hid the client bug
//     behind a page that was merely the wrong size.
//
// It writes the 400 itself; the caller returns when ok is false.
func listLimit(w http.ResponseWriter, r *http.Request, def, max int) (n int, ok bool) {
	return boundedIntParam(w, r, "limit", def, max)
}

// boundedIntParam is listLimit for a parameter with another name
// (recent_limit, tail).
func boundedIntParam(w http.ResponseWriter, r *http.Request, name string, def, max int) (int, bool) {
	v := r.URL.Query().Get(name)
	if v == "" {
		return def, true
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		writeError(w, http.StatusBadRequest, "invalid_"+name,
			fmt.Sprintf("%s must be a positive integer (at most %d; larger values are capped)", name, max))
		return 0, false
	}
	if n > max {
		n = max
	}
	return n, true
}

// revisionOK enforces the one revision rule every revisioned write shares
// (api/openapi.yaml, "Revisions"; CONTRACT-FINDINGS F155): `revision` is the
// value the client last read, 0 meaning "I read nothing: create it only if it
// does not exist". A negative revision names no state the server ever had.
func revisionOK(w http.ResponseWriter, revision int64) bool {
	if revision < 0 {
		writeError(w, http.StatusBadRequest, "invalid_revision",
			"revision must be the revision last read, or 0 to create only if absent")
		return false
	}
	return true
}

func intParam(r *http.Request, name string, def int) int {
	v := r.URL.Query().Get(name)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return def
	}
	return n
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// rawJSON passes a stored JSONB column through without re-encoding it.
//
// json.RawMessage rather than unmarshal-then-remarshal: the round trip would cost
// two allocations per field for no benefit, and would silently reformat numbers the
// database stored exactly.
func rawJSON(b []byte) json.RawMessage {
	if len(b) == 0 {
		return json.RawMessage("null")
	}
	return json.RawMessage(b)
}

func runPayload(r store.RunRow) map[string]any {
	return map[string]any{
		"id":        r.ID,
		"namespace": r.Namespace,
		"spec_id":   r.SpecID,
		"spec_name": r.SpecName,
		// The pinned version, so a caller can tell exactly which definition this
		// run executes even after the workflow has been edited many times since.
		"spec_version":  r.SpecVersion,
		"state":         r.State,
		"priority":      r.Priority,
		"trigger_kind":  r.TriggerKind,
		"params":        rawJSON(r.Params),
		"deadline_at":   r.DeadlineAt,
		"created_at":    r.CreatedAt,
		"started_at":    r.StartedAt,
		"ended_at":      r.EndedAt,
		"state_version": r.StateVersion,
	}
}

func attemptPayload(a store.AttemptRow) map[string]any {
	return map[string]any{
		"id":             a.ID,
		"node_id":        a.NodeID,
		"attempt_number": a.AttemptNumber,
		"state":          a.State,
		"queue":          a.Queue,
		"worker_id":      a.WorkerID,
		// The worker's hostname, so a run page can say WHICH machine ran a task
		// without a round trip to the fleet listing. Null until dispatched.
		"worker_hostname": a.WorkerHostname,
		// pending_reason is why a PENDING attempt has not dispatched, e.g.
		// "waiting_for_resources: gpu (need 4, free 3)". Without surfacing it the UI
		// can only show an unexplained stall.
		"pending_reason": a.PendingReason,
		"error":          rawJSON(a.Error),
		"metrics":        rawJSON(a.Metrics),
		"scheduled_at":   a.ScheduledAt,
		// When the scheduler placed it. Additive (F120): without it the console
		// cannot tell queue wait from a slow worker start.
		"dispatched_at": a.DispatchedAt,
		"started_at":    a.StartedAt,
		"ended_at":      a.EndedAt,
		// Set when the attempt reused a memoized result (§6.4) instead of
		// executing; the console marks such nodes "cached" rather than "ran".
		"cache_hit_of": a.CacheHitOf,
		// The latest status the WORKER reported for this attempt (F96), e.g.
		// "pod Pending: Unschedulable: 0/3 nodes are available: insufficient
		// cpu" from the Kubernetes executor. Deliberately a separate field from
		// pending_reason: that one is the scheduler explaining why it has not
		// placed the attempt, this one is the executor explaining what the
		// substrate is doing with an attempt that WAS placed. Sanitized by the
		// scheduler before storage (one line, no control characters, bounded);
		// null when the worker never sent a message.
		"progress_message": a.ProgressMessage,
		// In [0, 1], or null when no valid fraction was reported.
		"progress_fraction": a.ProgressFraction,
		// Control-plane time of the latest progress report, and of the latest
		// CHANGE of message ("pod Pending since ...").
		"progress_at":         a.ProgressAt,
		"progress_message_at": a.ProgressMessageAt,
		// Usage units the TASK reported through the workload API (ADR-0032
		// §2): cumulative totals keyed tokens:<provider>/<model>:input and so
		// on. {} when nothing was reported. These are the numbers copied into
		// the usage ledger when the attempt ended.
		"usage_units":       usageUnitsJSON(a.UsageUnits),
		"usage_reported_at": a.UsageReportedAt,
	}
}

// usageUnitsJSON is the stored units object, or {} for none.
func usageUnitsJSON(b []byte) json.RawMessage {
	if len(b) == 0 {
		return json.RawMessage(`{}`)
	}
	return json.RawMessage(b)
}

func eventPayload(e store.EventRow) map[string]any {
	return map[string]any{
		"seq":         e.Seq,
		"node_id":     e.NodeID,
		"attempt_id":  e.AttemptID,
		"kind":        e.Kind,
		"payload":     rawJSON(e.Payload),
		"actor":       e.Actor,
		"occurred_at": e.OccurredAt,
	}
}

// workerPayload is the fleet row, shared by the listing and the detail read.
//
// Extracted rather than left inline in listWorkers because the detail handler
// must not answer with a DIFFERENT shape for the same entity: the console's
// Worker type is one type, and a detail response missing `executors` (or
// spelling `labels` as an object where the list spells it raw) would be a bug
// visible only on one of the two screens.
func workerPayload(wk store.WorkerRow) map[string]any {
	return map[string]any{
		"id": wk.ID, "hostname": wk.Hostname, "queues": wk.Queues,
		"executors": wk.Executors, "state": wk.State,
		"labels": rawJSON(wk.Labels), "capacity": rawJSON(wk.Capacity),
		"committed":     rawJSON(wk.Committed),
		"agent_version": wk.AgentVersion,
		"registered_at": wk.RegisteredAt, "last_heartbeat_at": wk.LastHeartbeatAt,
		"active_attempts": wk.ActiveAttempts,
	}
}

// workerAttemptPayload is one task attributed to a worker.
//
// Distinct from attemptPayload, which omits run_id: on a run page the run is
// ambient, but read from a worker the run id IS the answer — it is the link an
// operator follows from "this worker is busy" to "with what". task_ref comes
// along for the same reason, since `node_id` alone ("step-3") names a position
// in a DAG the reader is not currently looking at.
func workerAttemptPayload(a store.WorkerAttemptRow) map[string]any {
	return map[string]any{
		"id":             a.ID,
		"run_id":         a.RunID,
		"node_id":        a.NodeID,
		"task_ref":       a.TaskRef,
		"attempt_number": a.AttemptNumber,
		"state":          a.State,
		"queue":          a.Queue,
		"scheduled_at":   a.ScheduledAt,
		"started_at":     a.StartedAt,
		"ended_at":       a.EndedAt,
	}
}
