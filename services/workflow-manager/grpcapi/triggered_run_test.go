// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package grpcapi

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/khuriwalgroup/kgf-nexus/internal/store"
	"github.com/khuriwalgroup/kgf-nexus/services/workflow-manager/runs"
)

// recordingRuns implements RunService.Create only; the other methods are not
// reached by CreateTriggeredRun.
type recordingRuns struct {
	RunService
	got runs.CreateRequest
	err error
}

func (r *recordingRuns) Create(_ context.Context, req runs.CreateRequest) (runs.CreateResult, error) {
	r.got = req
	if r.err != nil {
		return runs.CreateResult{}, r.err
	}
	return runs.CreateResult{Run: store.RunRow{ID: "run-1"}, Created: true}, nil
}

func TestCreateTriggeredRunPassesTypedParamsAndDetail(t *testing.T) {
	rr := &recordingRuns{}
	s := &SpecServer{runs: rr, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	v := int32(3)
	resp, err := s.CreateTriggeredRun(context.Background(), &CreateTriggeredRunRequest{
		Namespace: "ns", SpecName: "etl", SpecVersion: &v, IdempotencyKey: "webhook:t:d-1", TriggerKind: "webhook",
		TriggerDetail: map[string]any{"trigger_id": "t"}, Params: map[string]any{"n": float64(3)}, Priority: 7,
	})
	if err != nil || resp.RunID != "run-1" || !resp.Created {
		t.Fatalf("%+v %v", resp, err)
	}
	if rr.got.TriggerKind != "webhook" || *rr.got.SpecVersion != 3 || rr.got.Params["n"] != float64(3) ||
		rr.got.TriggerDetail["trigger_id"] != "t" || rr.got.Priority != 7 {
		t.Fatalf("request %+v", rr.got)
	}
}

func TestCreateTriggeredRunRefusesForeignKindsAndKeys(t *testing.T) {
	s := &SpecServer{runs: &recordingRuns{}, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	for _, req := range []*CreateTriggeredRunRequest{
		{Namespace: "ns", SpecName: "etl", IdempotencyKey: "schedule:x", TriggerKind: "schedule"},
		{Namespace: "ns", SpecName: "etl", IdempotencyKey: "other:x", TriggerKind: "webhook"},
		{Namespace: "", SpecName: "etl", IdempotencyKey: "webhook:x", TriggerKind: "webhook"},
	} {
		if _, err := s.CreateTriggeredRun(context.Background(), req); status.Code(err) != codes.InvalidArgument {
			t.Errorf("%+v: %v", req, err)
		}
	}
}

func TestCreateTriggeredRunMapsRunErrors(t *testing.T) {
	for want, err := range map[codes.Code]error{
		codes.NotFound:          store.ErrNotFound,
		codes.InvalidArgument:   runs.ErrMissingParams,
		codes.ResourceExhausted: store.ErrMaxActiveRuns,
	} {
		s := &SpecServer{runs: &recordingRuns{err: err}, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
		_, got := s.CreateTriggeredRun(context.Background(), &CreateTriggeredRunRequest{
			Namespace: "ns", SpecName: "etl", IdempotencyKey: "sensor:x", TriggerKind: "sensor"})
		if status.Code(got) != want {
			t.Errorf("%v -> %v, want %v", err, status.Code(got), want)
		}
	}
}
