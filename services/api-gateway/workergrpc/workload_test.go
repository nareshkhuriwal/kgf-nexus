// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package workergrpc

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
	"github.com/khuriwalgroup/kgf-nexus/internal/store"
	"github.com/khuriwalgroup/kgf-nexus/services/api-gateway/upstream"
)

// workloadSched is a Scheduler that also serves the workload API (ADR-0032).
type workloadSched struct {
	fakeSched
	mu         sync.Mutex
	usage      []map[string]int64
	usageErr   error
	usageStale bool
	permits    []upstream.PermitCall
	permitRes  upstream.PermitResult
	ckpt       upstream.CheckpointResult
	ckptErr    error
}

func (w *workloadSched) RecordUsage(ctx context.Context, attemptID, token string, units map[string]int64) (bool, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.usage = append(w.usage, units)
	return w.usageStale, w.usageErr
}

func (w *workloadSched) AcquirePermits(ctx context.Context, call upstream.PermitCall) (upstream.PermitResult, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.permits = append(w.permits, call)
	return w.permitRes, nil
}

func (w *workloadSched) SaveCheckpoint(ctx context.Context, call upstream.CheckpointCall) (upstream.CheckpointResult, error) {
	return w.ckpt, w.ckptErr
}

func workloadServer(sch Scheduler, api *nexusv1.WorkloadApiConfig) (*Server, *leaseDB) {
	db := newLeaseDB()
	db.tokens["a1"] = "tok"
	s := testServer(db, &fakeRM{exists: true, state: "ACTIVE"}, sch)
	s.cfg.WorkloadAPI = api
	return s, db
}

func TestWorkloadRPCsAreUnimplementedWhenNotOffered(t *testing.T) {
	ctx := workerCtx()
	for name, s := range map[string]*Server{
		// A scheduler client that cannot serve them (an older build).
		"old scheduler": func() *Server { s, _ := workloadServer(&fakeSched{}, DefaultWorkloadAPI()); return s }(),
		// A gateway configured without the workload API.
		"disabled": func() *Server { s, _ := workloadServer(&workloadSched{}, nil); return s }(),
	} {
		if _, err := s.ReportUsage(ctx, &nexusv1.ReportUsageRequest{AttemptId: "a1", LeaseToken: "tok"}); status.Code(err) != codes.Unimplemented {
			t.Errorf("%s ReportUsage: %v", name, err)
		}
		if _, err := s.AcquirePermits(ctx, &nexusv1.AcquirePermitsRequest{AttemptId: "a1", LeaseToken: "tok"}); status.Code(err) != codes.Unimplemented {
			t.Errorf("%s AcquirePermits: %v", name, err)
		}
		if _, err := s.GetCheckpoint(ctx, &nexusv1.GetCheckpointRequest{AttemptId: "a1", LeaseToken: "tok"}); status.Code(err) != codes.Unimplemented {
			t.Errorf("%s GetCheckpoint: %v", name, err)
		}
		if s.advertisedWorkloadAPI() != nil {
			t.Errorf("%s: Register would advertise a workload API it cannot serve", name)
		}
	}
	// Only usage offered: permits stay Unimplemented.
	s, _ := workloadServer(&workloadSched{}, &nexusv1.WorkloadApiConfig{UsageReporting: true})
	if _, err := s.AcquirePermits(ctx, &nexusv1.AcquirePermitsRequest{AttemptId: "a1", LeaseToken: "tok"}); status.Code(err) != codes.Unimplemented {
		t.Errorf("permits not offered: %v", err)
	}
}

func TestWorkloadRPCsAreFencedAndForwarded(t *testing.T) {
	ctx := workerCtx()
	sch := &workloadSched{permitRes: upstream.PermitResult{Granted: false, RetryAfterMs: 1500, UnlimitedKeys: []string{"x"}}}
	s, _ := workloadServer(sch, DefaultWorkloadAPI())

	// Fenced at the gateway: a stale token never reaches the scheduler.
	ack, err := s.ReportUsage(ctx, &nexusv1.ReportUsageRequest{AttemptId: "a1", LeaseToken: "old", Units: map[string]int64{"requests:a/b": 1}})
	if err != nil || ack.GetAccepted() || ack.GetReason() != store.ReasonStaleLeaseToken {
		t.Fatalf("stale usage: %+v %v", ack, err)
	}
	resp, err := s.AcquirePermits(ctx, &nexusv1.AcquirePermitsRequest{AttemptId: "a1", LeaseToken: "old", Keys: []string{"a"}, Requests: 1})
	if err != nil || resp.GetAccepted() || resp.GetReason() != store.ReasonStaleLeaseToken {
		t.Fatalf("stale permits: %+v %v", resp, err)
	}
	if len(sch.usage) != 0 || len(sch.permits) != 0 {
		t.Fatal("a stale call reached the scheduler")
	}

	// Current token: forwarded verbatim.
	ack, err = s.ReportUsage(ctx, &nexusv1.ReportUsageRequest{AttemptId: "a1", LeaseToken: "tok", Units: map[string]int64{"requests:a/b": 3}})
	if err != nil || !ack.GetAccepted() || sch.usage[0]["requests:a/b"] != 3 {
		t.Fatalf("usage: %+v %v %v", ack, err, sch.usage)
	}
	resp, err = s.AcquirePermits(ctx, &nexusv1.AcquirePermitsRequest{AttemptId: "a1", LeaseToken: "tok", Keys: []string{"a", "x"}, Requests: 1, Tokens: 50})
	if err != nil || !resp.GetAccepted() || resp.GetGranted() || resp.GetRetryAfterMs() != 1500 || len(resp.GetUnlimitedKeys()) != 1 {
		t.Fatalf("permits: %+v %v", resp, err)
	}
	if c := sch.permits[0]; c.Tokens != 50 || c.LeaseToken != "tok" || len(c.Keys) != 2 {
		t.Fatalf("permit call forwarded as %+v", c)
	}

	// Invalid usage from the scheduler is a protocol outcome (the agent stops
	// resending), a stale answer from the scheduler's own fence is stale, and
	// an unavailable scheduler is a retryable status.
	sch.usageErr = fmt.Errorf("record usage: %w", status.Error(codes.InvalidArgument, "bad key"))
	if ack, err := s.ReportUsage(ctx, &nexusv1.ReportUsageRequest{AttemptId: "a1", LeaseToken: "tok"}); err != nil || ack.GetReason() != "invalid_usage" {
		t.Fatalf("invalid usage: %+v %v", ack, err)
	}
	sch.usageErr = fmt.Errorf("record usage: %w", status.Error(codes.Unavailable, "migration 0036"))
	if _, err := s.ReportUsage(ctx, &nexusv1.ReportUsageRequest{AttemptId: "a1", LeaseToken: "tok"}); status.Code(err) != codes.Unavailable {
		t.Fatalf("unavailable scheduler: %v", err)
	}
	sch.usageErr, sch.usageStale = nil, true
	if ack, _ := s.ReportUsage(ctx, &nexusv1.ReportUsageRequest{AttemptId: "a1", LeaseToken: "tok"}); ack.GetReason() != store.ReasonStaleLeaseToken {
		t.Fatalf("scheduler-side stale: %+v", ack)
	}

	// Checkpoints: superseded is its own reason; oversize is refused before
	// the scheduler is called.
	sch.ckpt = upstream.CheckpointResult{Superseded: true}
	if ack, _ := s.SaveCheckpoint(ctx, &nexusv1.SaveCheckpointRequest{AttemptId: "a1", LeaseToken: "tok", Seq: 1, SizeBytes: 10}); ack.GetReason() != store.ReasonStaleCheckpoint {
		t.Fatalf("superseded: %+v", ack)
	}
	if ack, _ := s.SaveCheckpoint(ctx, &nexusv1.SaveCheckpointRequest{AttemptId: "a1", LeaseToken: "tok", Seq: 2, SizeBytes: DefaultMaxCheckpointBytes + 1}); ack.GetReason() != "invalid_checkpoint" {
		t.Fatalf("oversize: %+v", ack)
	}
	if got := s.advertisedWorkloadAPI(); !got.GetUsageReporting() || !got.GetRateLimits() || !got.GetCheckpoints() {
		t.Fatalf("advertised %+v", got)
	}
}
