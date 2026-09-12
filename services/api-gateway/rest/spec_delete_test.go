// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package rest

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/khuriwalgroup/kgf-nexus/internal/store"
	"github.com/khuriwalgroup/kgf-nexus/services/api-gateway/authn"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Tests for DELETE /v1/namespaces/{ns}/specs/{name}.
//
// Driven over HTTP rather than against the handler function, because every
// assertion here is about the HTTP contract: which STATUS CODE a refusal produces
// (409, not 500 -- the operator can act on a 409), which credential is permitted,
// and whether the refusal names its blocker. Asserting at the store level would
// leave all three untested.

// newSpecDeleteAPI wires identity as well as the workflow manager, because the
// audit assertions need somewhere for the rows to land. Without WithIdentity the
// gateway drops audit writes silently by design, and a test that did not wire it
// would pass whether or not the handler audited anything.
func newSpecDeleteAPI(t *testing.T) (http.Handler, *fakeWFM, *fakeIdentityAdmin) {
	t.Helper()
	db := newRestDB()
	db.addKey(adminKey, "team-ml", "ci", "admin")
	db.addKey(readerKey, "team-ml", "dashboard", "reader")
	wfm := &fakeWFM{}
	ids := newFakeIdentityAdmin()
	api := New(db, authn.New(db), wfm, fakeRM{}, nil,
		slog.New(slog.NewTextHandler(io.Discard, nil))).WithIdentity(ids)
	return api.Routes(), wfm, ids
}

func TestDeleteSpecSucceedsAndReportsVersionCount(t *testing.T) {
	h, wfm, _ := newSpecDeleteAPI(t)

	rec := doJSON(t, h, http.MethodDelete, "/v1/namespaces/team-ml/specs/train", adminKey, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200. body=%s", rec.Code, rec.Body)
	}

	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// 200 with a count rather than 204: how much was destroyed is information the
	// operator cannot recover afterwards, and a 204 would throw it away.
	if got["versions_deleted"] != float64(3) {
		t.Errorf("versions_deleted = %v, want 3. body=%s", got["versions_deleted"], rec.Body)
	}
	if len(wfm.specDeletes) != 1 || wfm.specDeletes[0] != "team-ml/train" {
		t.Errorf("owner received %v, want one delete of team-ml/train", wfm.specDeletes)
	}
}

// TestDeleteSpecRefusesWhenReferenced is the important one.
//
// workflow_runs.spec_id is a hard FK and schedules.spec_name is NOT -- so one
// blocker is enforced by the database and the other only by application code. The
// handler must turn either into a 409 an operator can act on, never a 500 that
// tells them to file a bug, and never a success that breaks a live schedule.
func TestDeleteSpecRefusesWhenReferenced(t *testing.T) {
	h, wfm, _ := newSpecDeleteAPI(t)
	wfm.deleteSpecFn = func(_, _ string) (int, error) {
		return 0, status.Error(codes.FailedPrecondition,
			`workflow team-ml/train is still referenced by 12 run(s) and 1 schedule(s)`)
	}

	rec := doJSON(t, h, http.MethodDelete, "/v1/namespaces/team-ml/specs/train", adminKey, "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("status %d, want 409. body=%s", rec.Code, rec.Body)
	}

	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["error"] != "workflow_referenced" {
		t.Errorf("error code = %v, want workflow_referenced", got["error"])
	}
	// The message must NAME the blocker. A refusal that says only "cannot delete"
	// sends the operator hunting, and an error they cannot act on is one they
	// learn to ignore.
	msg, _ := got["message"].(string)
	if msg == "" {
		t.Fatalf("refusal carried no message: %s", rec.Body)
	}
	for _, want := range []string{"12 run", "1 schedule"} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal message %q does not name %q", msg, want)
		}
	}
}

// TestDeleteSpecRefusalIsAudited: an audit log that records only what happened
// cannot show what someone TRIED to do, and an attempt to delete a workflow with
// live history is exactly the event worth finding later.
func TestDeleteSpecRefusalIsAudited(t *testing.T) {
	h, wfm, ids := newSpecDeleteAPI(t)
	wfm.deleteSpecFn = func(_, _ string) (int, error) {
		return 0, status.Error(codes.FailedPrecondition, "still referenced by 4 run(s) and 0 schedule(s)")
	}

	if rec := doJSON(t, h, http.MethodDelete, "/v1/namespaces/team-ml/specs/train", adminKey, ""); rec.Code != http.StatusConflict {
		t.Fatalf("status %d, want 409", rec.Code)
	}

	e := findAudit(t, ids, "spec.delete")
	if e.Outcome != store.AuditDenied {
		t.Errorf("refused delete audited with outcome %q, want %q", e.Outcome, store.AuditDenied)
	}
	if e.TargetID != "team-ml/train" {
		t.Errorf("audit target = %q, want team-ml/train", e.TargetID)
	}
}

func TestDeleteSpecSuccessIsAudited(t *testing.T) {
	h, _, ids := newSpecDeleteAPI(t)

	if rec := doJSON(t, h, http.MethodDelete, "/v1/namespaces/team-ml/specs/train", adminKey, ""); rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200", rec.Code)
	}

	e := findAudit(t, ids, "spec.delete")
	if e.Outcome != store.AuditSuccess {
		t.Errorf("outcome = %q, want %q", e.Outcome, store.AuditSuccess)
	}
	// The version count is the only remaining evidence of how much was destroyed,
	// so it belongs in the audit detail rather than only in the response the
	// caller keeps to themselves.
	if e.Detail["versions_deleted"] != 3 {
		t.Errorf("audit detail = %v, want versions_deleted=3", e.Detail)
	}
}

// TestDeleteSpecRequiresAdminWrite pins the AUTHORIZATION BOUNDARY.
//
// Submitting a spec is ActionWrite because it is additive and reversible: a bad
// version is fixed by submitting a better one and the old one is still there.
// Deleting one destroys the document irrecoverably and removes a name the rest of
// the namespace creates runs by -- the same class of authority as draining a
// worker, which is already ActionAdminWrite. A reader credential (read only) and
// the boundary between write and admin_write are both asserted here, because a
// route silently registered at ActionWrite would let every CI deploy credential
// delete workflows.
func TestDeleteSpecRequiresAdminWrite(t *testing.T) {
	h, wfm, _ := newSpecDeleteAPI(t)

	// A reader must be refused outright.
	if rec := doJSON(t, h, http.MethodDelete, "/v1/namespaces/team-ml/specs/train", readerKey, ""); rec.Code != http.StatusForbidden {
		t.Errorf("reader got status %d, want 403", rec.Code)
	}
	// An unauthenticated caller gets 401, not 403: it has presented nothing to
	// authorize, and conflating the two hides a missing credential.
	if rec := doJSON(t, h, http.MethodDelete, "/v1/namespaces/team-ml/specs/train", "", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("anonymous got status %d, want 401", rec.Code)
	}
	// A credential for ANOTHER namespace must not reach this one. Namespaces are
	// the tenancy boundary; knowing a workflow name must not grant cross-tenant
	// destruction.
	if rec := doJSON(t, h, http.MethodDelete, "/v1/namespaces/team-other/specs/train", adminKey, ""); rec.Code != http.StatusForbidden {
		t.Errorf("cross-namespace admin got status %d, want 403", rec.Code)
	}
	// Nothing reached the owner on any of the three refusals. Authorization that
	// rejects AFTER the side effect is not authorization.
	if len(wfm.specDeletes) != 0 {
		t.Errorf("%d deletes reached the owner despite being unauthorized: %v", len(wfm.specDeletes), wfm.specDeletes)
	}
}

func TestDeleteSpecNotFound(t *testing.T) {
	h, wfm, _ := newSpecDeleteAPI(t)
	wfm.deleteSpecFn = func(_, _ string) (int, error) {
		return 0, status.Error(codes.NotFound, "no such workflow")
	}
	rec := doJSON(t, h, http.MethodDelete, "/v1/namespaces/team-ml/specs/ghost", adminKey, "")
	if rec.Code != http.StatusNotFound {
		t.Errorf("status %d, want 404. body=%s", rec.Code, rec.Body)
	}
}

// ── schedule delete auditing ──────────────────────────────────────────────

// TestDeleteScheduleIsAudited covers a gap found while adding spec deletion:
// deleteSchedule wrote no audit row at all. Creating a schedule is recorded and
// destroying one was not, so the trail could not answer "who stopped the nightly
// job" -- and deleting a schedule cascades its tick history, so a later backfill
// over the same interval re-runs work that already ran.
func TestDeleteScheduleIsAudited(t *testing.T) {
	db := newRestDB()
	db.addKey(adminKey, "team-ml", "ci", "admin")
	ids := newFakeIdentityAdmin()
	api := New(db, authn.New(db), &fakeWFM{}, fakeRM{}, nil,
		slog.New(slog.NewTextHandler(io.Discard, nil))).
		WithIdentity(ids).WithSchedules(&fakeSchedules{})

	rec := doJSON(t, api.Routes(), http.MethodDelete, "/v1/namespaces/team-ml/schedules/nightly", adminKey, "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status %d, want 204. body=%s", rec.Code, rec.Body)
	}

	e := findAudit(t, ids, "schedule.delete")
	if e.Outcome != store.AuditSuccess {
		t.Errorf("outcome = %q, want %q", e.Outcome, store.AuditSuccess)
	}
	if e.TargetID != "team-ml/nightly" {
		t.Errorf("audit target = %q, want team-ml/nightly", e.TargetID)
	}
}

// findAudit returns the first audit entry with the given action, failing if none.
func findAudit(t *testing.T, ids *fakeIdentityAdmin, action string) store.AuditEntry {
	t.Helper()
	for _, e := range ids.audit {
		if e.Action == action {
			return e
		}
	}
	t.Fatalf("no audit entry with action %q; recorded: %v", action, ids.audit)
	return store.AuditEntry{}
}
