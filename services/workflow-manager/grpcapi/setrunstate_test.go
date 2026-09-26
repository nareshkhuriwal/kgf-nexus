// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package grpcapi

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/khuriwalgroup/kgf-nexus/internal/store"
)

// terminalRuns is a RunService whose terminal transition fails with err.
type terminalRuns struct {
	RunService
	err error
}

func (r *terminalRuns) OnNodesTerminal(context.Context, string) (store.RunRow, error) {
	return store.RunRow{}, r.err
}

// F160: SetRunState used to answer applied=false for ANY error, so a terminal
// publish lost to a workflow-manager database fault looked, to the engine,
// exactly like a refusal it should drop -- and the run stayed RUNNING. Only the
// state machine's refusals (and a run that no longer exists) are "not
// applied"; a fault must come back as an error, so the engine keeps its durable
// reconcile request and publishes again.
func TestSetRunStateSeparatesRefusalsFromFaults(t *testing.T) {
	cases := []struct {
		name        string
		err         error
		wantErr     codes.Code // codes.OK = no error
		wantApplied bool
	}{
		{"applied", nil, codes.OK, true},
		{"illegal transition", fmt.Errorf("%w: RUNNING -> x", store.ErrRunStateConflict), codes.OK, false},
		{"run gone", store.ErrNotFound, codes.OK, false},
		{"database fault", errors.New("connect: connection refused"), codes.Unavailable, false},
		{"deadline", context.DeadlineExceeded, codes.Unavailable, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := &SpecServer{runs: &terminalRuns{err: c.err}, log: quietLogger()}
			resp, err := srv.SetRunState(context.Background(), &SetRunStateRequest{RunID: "r1", State: "SUCCEEDED"})
			if c.wantErr != codes.OK {
				if status.Code(err) != c.wantErr {
					t.Fatalf("err = %v (code %s), want code %s: a fault reported as not-applied is dropped by the engine",
						err, status.Code(err), c.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("err = %v, want none", err)
			}
			if resp.Applied != c.wantApplied {
				t.Errorf("applied = %v, want %v", resp.Applied, c.wantApplied)
			}
		})
	}
}
