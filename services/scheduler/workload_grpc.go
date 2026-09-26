// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/khuriwalgroup/kgf-nexus/internal/store"
)

// The workload API's scheduler half (ADR-0032): the gateway fences each call on
// the lease token and forwards it here, where the store fences AGAIN inside the
// write. Same hand-written JSON descriptor as the rest of AttemptReportService.
//
// Status mapping, which the gateway relies on:
//   - a stale lease or a superseded checkpoint is a normal answer, not an error;
//   - invalid input is InvalidArgument (resending cannot help);
//   - migration 0036 missing, a busy attempt row, a database failure are
//     Unavailable/Internal, which the agent retries.

// RecordUsageRequest carries the attempt's cumulative usage totals.
type RecordUsageRequest struct {
	AttemptID  string           `json:"attempt_id"`
	LeaseToken string           `json:"lease_token"`
	Units      map[string]int64 `json:"units"`
}

// RecordUsageResponse reports the outcome.
type RecordUsageResponse struct {
	StaleLease bool `json:"stale_lease,omitempty"`
}

// AcquirePermitsRequest is one permit acquisition or settlement.
type AcquirePermitsRequest struct {
	AttemptID  string   `json:"attempt_id"`
	LeaseToken string   `json:"lease_token"`
	Keys       []string `json:"keys"`
	Requests   int64    `json:"requests,omitempty"`
	Tokens     int64    `json:"tokens,omitempty"`
	Settle     bool     `json:"settle,omitempty"`
}

// AcquirePermitsResponse reports the outcome.
type AcquirePermitsResponse struct {
	StaleLease    bool     `json:"stale_lease,omitempty"`
	Granted       bool     `json:"granted"`
	RetryAfterMs  int64    `json:"retry_after_ms,omitempty"`
	UnlimitedKeys []string `json:"unlimited_keys,omitempty"`
}

// SaveCheckpointRequest registers an already-written checkpoint object.
type SaveCheckpointRequest struct {
	AttemptID  string `json:"attempt_id"`
	LeaseToken string `json:"lease_token"`
	Seq        int64  `json:"seq"`
	Key        string `json:"key"`
	SHA256     string `json:"sha256"`
	SizeBytes  int64  `json:"size_bytes"`
}

// SaveCheckpointResponse reports the outcome.
type SaveCheckpointResponse struct {
	StaleLease bool `json:"stale_lease,omitempty"`
	Superseded bool `json:"superseded,omitempty"`
}

type workloadService interface {
	RecordUsage(context.Context, *RecordUsageRequest) (*RecordUsageResponse, error)
	AcquirePermits(context.Context, *AcquirePermitsRequest) (*AcquirePermitsResponse, error)
	SaveCheckpoint(context.Context, *SaveCheckpointRequest) (*SaveCheckpointResponse, error)
}

// workloadStatus maps a store error onto the status the gateway forwards.
func workloadStatus(op string, err error) error {
	switch {
	case errors.Is(err, store.ErrInvalidWorkloadReport):
		return status.Errorf(codes.InvalidArgument, "%s: %v", op, err)
	case errors.Is(err, store.ErrWorkloadUnavailable), errors.Is(err, store.ErrAttemptBusy):
		return status.Errorf(codes.Unavailable, "%s: %v", op, err)
	default:
		return status.Errorf(codes.Internal, "%s: %v", op, err)
	}
}

func parseAttempt(id string) (uuid.UUID, error) {
	u, err := uuid.Parse(id)
	if err != nil {
		return uuid.Nil, status.Errorf(codes.InvalidArgument, "attempt_id %q is not a uuid", id)
	}
	return u, nil
}

// RecordUsage merges the attempt's cumulative usage units.
func (r *ReportServer) RecordUsage(ctx context.Context, req *RecordUsageRequest) (*RecordUsageResponse, error) {
	id, err := parseAttempt(req.AttemptID)
	if err != nil {
		return nil, err
	}
	out, err := r.store.RecordUsage(ctx, id, req.LeaseToken, req.Units, r.now())
	if err != nil {
		result := "error"
		if errors.Is(err, store.ErrInvalidWorkloadReport) {
			result = "invalid"
			r.log.WarnContext(ctx, "usage report refused", "attempt_id", req.AttemptID, "error", err)
		}
		r.metrics.observeUsageReport(result, nil)
		return nil, workloadStatus("record usage", err)
	}
	switch {
	case out.StaleLease:
		r.metrics.observeUsageReport("stale", nil)
		r.log.WarnContext(ctx, "usage rejected on stale lease", "attempt_id", req.AttemptID)
		return &RecordUsageResponse{StaleLease: true}, nil
	case out.Changed:
		r.metrics.observeUsageReport("stored", out.Growth)
	default:
		r.metrics.observeUsageReport("unchanged", nil)
	}
	return &RecordUsageResponse{}, nil
}

// AcquirePermits takes or settles rate-limit permits.
func (r *ReportServer) AcquirePermits(ctx context.Context, req *AcquirePermitsRequest) (*AcquirePermitsResponse, error) {
	id, err := parseAttempt(req.AttemptID)
	if err != nil {
		return nil, err
	}
	started := time.Now()
	out, err := r.store.AcquirePermits(ctx, store.PermitRequest{
		AttemptID: id, Token: req.LeaseToken, Keys: req.Keys,
		Requests: req.Requests, Tokens: req.Tokens, Settle: req.Settle,
	})
	d := time.Since(started)
	if err != nil {
		result := "error"
		if errors.Is(err, store.ErrInvalidWorkloadReport) {
			result = "invalid"
		}
		r.metrics.observePermit(result, d)
		return nil, workloadStatus("acquire permits", err)
	}
	result := "granted"
	switch {
	case out.StaleLease:
		result = "stale"
	case req.Settle:
		result = "settled"
	case out.Limited == 0:
		result = "unlimited"
	case !out.Granted:
		result = "denied"
	}
	r.metrics.observePermit(result, d)
	if out.StaleLease {
		return &AcquirePermitsResponse{StaleLease: true}, nil
	}
	return &AcquirePermitsResponse{
		Granted: out.Granted, RetryAfterMs: out.RetryAfter.Milliseconds(), UnlimitedKeys: out.Unlimited,
	}, nil
}

// SaveCheckpoint moves the idempotency key's checkpoint pointer forward.
func (r *ReportServer) SaveCheckpoint(ctx context.Context, req *SaveCheckpointRequest) (*SaveCheckpointResponse, error) {
	id, err := parseAttempt(req.AttemptID)
	if err != nil {
		return nil, err
	}
	out, err := r.store.SaveCheckpoint(ctx, store.CheckpointSave{
		AttemptID: id, Token: req.LeaseToken, Seq: req.Seq, Key: req.Key,
		SHA256: req.SHA256, SizeBytes: req.SizeBytes,
	}, r.now())
	if err != nil {
		result := "error"
		if errors.Is(err, store.ErrInvalidWorkloadReport) {
			result = "invalid"
			r.log.WarnContext(ctx, "checkpoint refused", "attempt_id", req.AttemptID, "error", err)
		}
		r.metrics.observeCheckpoint(result)
		return nil, workloadStatus("save checkpoint", err)
	}
	switch {
	case out.StaleLease:
		r.metrics.observeCheckpoint("stale")
		r.log.WarnContext(ctx, "checkpoint rejected on stale lease", "attempt_id", req.AttemptID)
	case out.Superseded:
		r.metrics.observeCheckpoint("superseded")
		r.log.WarnContext(ctx, "checkpoint older than the stored one ignored", "attempt_id", req.AttemptID, "seq", req.Seq)
	default:
		r.metrics.observeCheckpoint("saved")
	}
	return &SaveCheckpointResponse{StaleLease: out.StaleLease, Superseded: out.Superseded}, nil
}

func recordUsageHandler(srv any, ctx context.Context, dec func(any) error, ic grpc.UnaryServerInterceptor) (any, error) {
	in := new(RecordUsageRequest)
	if err := dec(in); err != nil {
		return nil, err
	}
	if ic == nil {
		return srv.(workloadService).RecordUsage(ctx, in)
	}
	return ic(ctx, in, &grpc.UnaryServerInfo{Server: srv, FullMethod: "/nexus.v1.AttemptReportService/RecordUsage"},
		func(ctx context.Context, req any) (any, error) {
			return srv.(workloadService).RecordUsage(ctx, req.(*RecordUsageRequest))
		})
}

func acquirePermitsHandler(srv any, ctx context.Context, dec func(any) error, ic grpc.UnaryServerInterceptor) (any, error) {
	in := new(AcquirePermitsRequest)
	if err := dec(in); err != nil {
		return nil, err
	}
	if ic == nil {
		return srv.(workloadService).AcquirePermits(ctx, in)
	}
	return ic(ctx, in, &grpc.UnaryServerInfo{Server: srv, FullMethod: "/nexus.v1.AttemptReportService/AcquirePermits"},
		func(ctx context.Context, req any) (any, error) {
			return srv.(workloadService).AcquirePermits(ctx, req.(*AcquirePermitsRequest))
		})
}

func saveCheckpointHandler(srv any, ctx context.Context, dec func(any) error, ic grpc.UnaryServerInterceptor) (any, error) {
	in := new(SaveCheckpointRequest)
	if err := dec(in); err != nil {
		return nil, err
	}
	if ic == nil {
		return srv.(workloadService).SaveCheckpoint(ctx, in)
	}
	return ic(ctx, in, &grpc.UnaryServerInfo{Server: srv, FullMethod: "/nexus.v1.AttemptReportService/SaveCheckpoint"},
		func(ctx context.Context, req any) (any, error) {
			return srv.(workloadService).SaveCheckpoint(ctx, req.(*SaveCheckpointRequest))
		})
}
