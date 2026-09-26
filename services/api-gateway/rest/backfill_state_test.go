// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package rest

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"testing"

	"github.com/khuriwalgroup/kgf-nexus/internal/store"
	"github.com/khuriwalgroup/kgf-nexus/services/api-gateway/authn"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type fakeBackfills struct{ err error }

func (f fakeBackfills) StartBackfill(context.Context, BackfillStartRequest) (store.Backfill, error) {
	return store.Backfill{}, f.err
}
func (f fakeBackfills) PauseBackfill(_ context.Context, ns, id string) (store.Backfill, error) {
	return store.Backfill{ID: id, Namespace: ns, State: store.BackfillPaused}, f.err
}
func (f fakeBackfills) ResumeBackfill(_ context.Context, ns, id string) (store.Backfill, error) {
	return store.Backfill{ID: id, Namespace: ns, State: store.BackfillRunning}, f.err
}
func (f fakeBackfills) CancelBackfill(_ context.Context, ns, id string) (store.Backfill, error) {
	return store.Backfill{ID: id, Namespace: ns, State: store.BackfillCancelled}, f.err
}

// TestBackfillActionConflictIs409 is F119's gateway half: the owner's refusal
// of a transition the backfill's state forbids reaches the client as 409
// backfill_state_conflict, whether it arrives as the store sentinel or as the
// FailedPrecondition status the owner sends over gRPC; a missing backfill is
// backfill_not_found, not the schedule_not_found failSchedule used to answer.
func TestBackfillActionConflictIs409(t *testing.T) {
	conflict := &store.BackfillStateConflict{ID: "bf-1", To: store.BackfillRunning,
		From: []string{store.BackfillPaused}, Current: store.BackfillCancelled}
	cases := []struct {
		name     string
		err      error
		wantCode int
		wantErr  string
	}{
		{"sentinel", conflict, http.StatusConflict, "backfill_state_conflict"},
		{"grpc", status.Error(codes.FailedPrecondition, "cannot resume: "+conflict.Error()), http.StatusConflict, "backfill_state_conflict"},
		{"missing sentinel", store.ErrBackfillNotFound, http.StatusNotFound, "backfill_not_found"},
		{"missing grpc", status.Error(codes.NotFound, "backfill not found"), http.StatusNotFound, "backfill_not_found"},
		{"internal", errors.New("boom"), http.StatusInternalServerError, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			db := newRestDB()
			db.addKey(adminKey, "team-ml", "ci", "admin")
			api := New(db, authn.New(db), &fakeWFM{}, fakeRM{}, nil,
				slog.New(slog.NewTextHandler(io.Discard, nil))).WithBackfills(fakeBackfills{err: c.err})
			rec := post(t, api.Routes(), "/v1/namespaces/team-ml/backfills/bf-1/resume", adminKey, "", `{}`)
			if rec.Code != c.wantCode {
				t.Fatalf("status %d, want %d. body=%s", rec.Code, c.wantCode, rec.Body)
			}
			if c.wantErr == "" {
				return
			}
			var body map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("body: %v", err)
			}
			if body["error"] != c.wantErr || body["message"] == "" {
				t.Errorf("body = %v, want error %q with a message", body, c.wantErr)
			}
		})
	}
}
