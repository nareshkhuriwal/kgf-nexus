// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package grpcapi

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/khuriwalgroup/kgf-nexus/internal/store"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// The schedule-mutation RPCs.
//
// Added to the existing hand-written WorkflowSpecService rather than given their own
// service, because they belong to the same owner and the same table group, and a
// second descriptor would be a second thing to replace when these become generated
// stubs. Registered on the same JSON codec as SetRunState, which the dag-engine
// already calls this way.
//
// WHY THESE ARE RPCs AT ALL. The gateway reads schedules directly -- ADR-0011 lets it
// read every table, and a listing is a projection like any other. But `schedules` is
// workflow-manager's table, so a WRITE has to go through the owner or migration
// 0005's grants refuse it at the database. That refusal is the point: it is what
// makes ownership real rather than a convention.

// CreateScheduleRequest creates a schedule.
//
// NextFireAt is supplied by the CALLER, because the cron arithmetic lives in
// internal/cron and the gateway has already parsed the expression to validate it and
// to return a preview. Recomputing it here would be a second place that decides when
// a schedule is first due, and two such places eventually disagree.
type CreateScheduleRequest struct {
	Namespace     string         `json:"namespace"`
	Name          string         `json:"name"`
	SpecName      string         `json:"spec_name"`
	SpecVersion   *int32         `json:"spec_version"`
	CronExpr      string         `json:"cron_expr"`
	Timezone      string         `json:"timezone"`
	Params        map[string]any `json:"params"`
	Priority      int32          `json:"priority"`
	OverlapPolicy string         `json:"overlap_policy"`
	CatchupLimit  int32          `json:"catchup_limit"`
	Enabled       bool           `json:"enabled"`
	CreatedBy     string         `json:"created_by"`
	NextFireAt    time.Time      `json:"next_fire_at"`
}

// ScheduleResponse is the stored row.
type ScheduleResponse struct {
	Schedule store.Schedule `json:"schedule"`
}

// SetScheduleEnabledRequest toggles a schedule.
type SetScheduleEnabledRequest struct {
	Namespace  string     `json:"namespace"`
	Name       string     `json:"name"`
	Enabled    bool       `json:"enabled"`
	NextFireAt *time.Time `json:"next_fire_at"`
}

// DeleteScheduleRequest removes one.
type DeleteScheduleRequest struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

// DeleteScheduleResponse is empty; the status carries the outcome.
type DeleteScheduleResponse struct{}

// CreateSchedule inserts the schedule.
func (s *SpecServer) CreateSchedule(ctx context.Context, req *CreateScheduleRequest) (*ScheduleResponse, error) {
	if req.Namespace == "" || req.Name == "" || req.SpecName == "" || req.CronExpr == "" {
		return nil, status.Error(codes.InvalidArgument, "namespace, name, spec_name and cron_expr are required")
	}
	if req.NextFireAt.IsZero() {
		// Refused rather than defaulted: a zero fire time would make the schedule
		// due at the Unix epoch, and the fire loop would immediately claim it and
		// fire a tick the author never asked for.
		return nil, status.Error(codes.InvalidArgument, "next_fire_at is required; the caller computes it from the cron expression")
	}
	row, err := store.WFMCreateSchedule(ctx, s.db, store.Schedule{
		Namespace:     req.Namespace,
		Name:          req.Name,
		SpecName:      req.SpecName,
		SpecVersion:   req.SpecVersion,
		CronExpr:      req.CronExpr,
		Timezone:      req.Timezone,
		Params:        req.Params,
		Priority:      req.Priority,
		OverlapPolicy: req.OverlapPolicy,
		CatchupLimit:  req.CatchupLimit,
		Enabled:       req.Enabled,
		CreatedBy:     req.CreatedBy,
	}, req.NextFireAt)
	if err != nil {
		// A duplicate name is the caller's mistake and the expected outcome of a
		// retried create, so it must not be a 500. Classified HERE, by SQLSTATE,
		// rather than by the gateway matching the message: a pgconn.PgError does
		// not survive a gRPC hop as a typed error, only as text, and matching on
		// text means a Postgres locale or wording change silently turns 409 back
		// into 500. AlreadyExists is the code gRPC has for exactly this.
		if isUniqueViolation(err) {
			return nil, status.Errorf(codes.AlreadyExists,
				"a schedule named %q already exists in namespace %s", req.Name, req.Namespace)
		}
		return nil, status.Errorf(codes.Internal, "create schedule: %v", err)
	}
	s.log.InfoContext(ctx, "schedule created",
		"schedule_id", row.ID, "namespace", row.Namespace, "name", row.Name,
		"cron", row.CronExpr, "enabled", row.Enabled, "next_fire_at", row.NextFireAt)
	return &ScheduleResponse{Schedule: row}, nil
}

// SetScheduleEnabled enables or disables a schedule.
func (s *SpecServer) SetScheduleEnabled(ctx context.Context, req *SetScheduleEnabledRequest) (*ScheduleResponse, error) {
	if req.Namespace == "" || req.Name == "" {
		return nil, status.Error(codes.InvalidArgument, "namespace and name are required")
	}
	if req.Enabled && req.NextFireAt == nil {
		return nil, status.Error(codes.InvalidArgument,
			"next_fire_at is required when enabling; re-enabling must schedule from now, not from a stale stored time")
	}
	row, err := store.WFMSetScheduleEnabled(ctx, s.db, req.Namespace, req.Name, req.Enabled, req.NextFireAt)
	if errors.Is(err, store.ErrScheduleNotFound) {
		return nil, status.Error(codes.NotFound, "schedule not found")
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "set schedule enabled: %v", err)
	}
	s.log.InfoContext(ctx, "schedule enabled state changed",
		"schedule_id", row.ID, "namespace", row.Namespace, "name", row.Name,
		"enabled", row.Enabled, "next_fire_at", row.NextFireAt)
	return &ScheduleResponse{Schedule: row}, nil
}

// DeleteSchedule removes a schedule and, by cascade, its tick history.
func (s *SpecServer) DeleteSchedule(ctx context.Context, req *DeleteScheduleRequest) (*DeleteScheduleResponse, error) {
	if req.Namespace == "" || req.Name == "" {
		return nil, status.Error(codes.InvalidArgument, "namespace and name are required")
	}
	err := store.WFMDeleteSchedule(ctx, s.db, req.Namespace, req.Name)
	if errors.Is(err, store.ErrScheduleNotFound) {
		return nil, status.Error(codes.NotFound, "schedule not found")
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "delete schedule: %v", err)
	}
	// WARN, not INFO: deleting a schedule discards its tick history, so a backfill
	// over the same interval would re-run work that already ran. Worth finding in a
	// log later.
	s.log.WarnContext(ctx, "schedule deleted; its tick history is gone with it",
		"namespace", req.Namespace, "name", req.Name)
	return &DeleteScheduleResponse{}, nil
}

func createScheduleHandler(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
	in := new(CreateScheduleRequest)
	if err := dec(in); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return srv.(specService).CreateSchedule(ctx, in)
	}
	return interceptor(ctx, in, &grpc.UnaryServerInfo{
		Server: srv, FullMethod: "/nexus.v1.WorkflowSpecService/CreateSchedule",
	}, func(ctx context.Context, req any) (any, error) {
		return srv.(specService).CreateSchedule(ctx, req.(*CreateScheduleRequest))
	})
}

func setScheduleEnabledHandler(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
	in := new(SetScheduleEnabledRequest)
	if err := dec(in); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return srv.(specService).SetScheduleEnabled(ctx, in)
	}
	return interceptor(ctx, in, &grpc.UnaryServerInfo{
		Server: srv, FullMethod: "/nexus.v1.WorkflowSpecService/SetScheduleEnabled",
	}, func(ctx context.Context, req any) (any, error) {
		return srv.(specService).SetScheduleEnabled(ctx, req.(*SetScheduleEnabledRequest))
	})
}

func deleteScheduleHandler(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
	in := new(DeleteScheduleRequest)
	if err := dec(in); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return srv.(specService).DeleteSchedule(ctx, in)
	}
	return interceptor(ctx, in, &grpc.UnaryServerInfo{
		Server: srv, FullMethod: "/nexus.v1.WorkflowSpecService/DeleteSchedule",
	}, func(ctx context.Context, req any) (any, error) {
		return srv.(specService).DeleteSchedule(ctx, req.(*DeleteScheduleRequest))
	})
}

// isUniqueViolation reports whether err is a Postgres unique-constraint violation.
//
// Checked by SQLSTATE, never by message text: the message carries the constraint
// name and the server's locale and is not a stable contract.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "23505"
	}
	return false
}

// ── backfills ───────────────────────────────────────────────────────────────

// StartBackfillRequest asks for a bounded historical catch-up.
type StartBackfillRequest struct {
	Namespace  string    `json:"namespace"`
	ScheduleID string    `json:"schedule_id"`
	From       time.Time `json:"from"`
	To         time.Time `json:"to"`
	MaxActive  int32     `json:"max_active"`
	MaxRuns    int32     `json:"max_runs"`
	CreatedBy  string    `json:"created_by"`
}

// BackfillResponse is the stored row.
type BackfillResponse struct {
	Backfill store.Backfill `json:"backfill"`
}

// BackfillActionRequest names a backfill to pause, resume or cancel.
type BackfillActionRequest struct {
	Namespace string `json:"namespace"`
	ID        string `json:"id"`
	Action    string `json:"action"`
}

// BackfillService is the subset of the schedule service these RPCs need.
//
// An interface so the gRPC layer does not import the concrete service and create a
// cycle: schedules already depends on store, and the server depends on both.
type BackfillService interface {
	StartBackfill(ctx context.Context, req BackfillStart) (store.Backfill, error)
	PauseBackfill(ctx context.Context, namespace, id string) (store.Backfill, error)
	ResumeBackfill(ctx context.Context, namespace, id string) (store.Backfill, error)
	CancelBackfill(ctx context.Context, namespace, id string) (store.Backfill, error)
}

// BackfillStart mirrors schedules.BackfillRequest without importing it.
type BackfillStart struct {
	Namespace  string
	ScheduleID string
	From       time.Time
	To         time.Time
	MaxActive  int32
	MaxRuns    int32
	CreatedBy  string
}

// StartBackfill validates through the service and creates the job.
func (s *SpecServer) StartBackfill(ctx context.Context, req *StartBackfillRequest) (*BackfillResponse, error) {
	if s.backfills == nil {
		return nil, status.Error(codes.Unimplemented, "this workflow-manager has no backfill service configured")
	}
	bf, err := s.backfills.StartBackfill(ctx, BackfillStart{
		Namespace: req.Namespace, ScheduleID: req.ScheduleID,
		From: req.From, To: req.To,
		MaxActive: req.MaxActive, MaxRuns: req.MaxRuns, CreatedBy: req.CreatedBy,
	})
	if err != nil {
		// InvalidArgument, because every rejection the service produces is something
		// the operator asked for wrongly -- an inverted interval, a cap over the
		// ceiling, a window with no tick in it. Returning Internal would tell the
		// gateway to answer 500 for a request the caller can fix.
		return nil, status.Errorf(codes.InvalidArgument, "start backfill: %v", err)
	}
	return &BackfillResponse{Backfill: bf}, nil
}

// BackfillAction pauses, resumes or cancels.
func (s *SpecServer) BackfillAction(ctx context.Context, req *BackfillActionRequest) (*BackfillResponse, error) {
	if s.backfills == nil {
		return nil, status.Error(codes.Unimplemented, "this workflow-manager has no backfill service configured")
	}
	var (
		bf  store.Backfill
		err error
	)
	switch req.Action {
	case "pause":
		bf, err = s.backfills.PauseBackfill(ctx, req.Namespace, req.ID)
	case "resume":
		bf, err = s.backfills.ResumeBackfill(ctx, req.Namespace, req.ID)
	case "cancel":
		bf, err = s.backfills.CancelBackfill(ctx, req.Namespace, req.ID)
	default:
		return nil, status.Errorf(codes.InvalidArgument, "unknown backfill action %q", req.Action)
	}
	if errors.Is(err, store.ErrBackfillNotFound) {
		return nil, status.Error(codes.NotFound, "backfill not found")
	}
	if errors.Is(err, store.ErrBackfillStateConflict) {
		// FailedPrecondition, the code the run lifecycle uses for the same
		// refusal: the request was well formed, the backfill's state forbids it.
		return nil, status.Errorf(codes.FailedPrecondition, "cannot %s: %v", req.Action, err)
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "%s backfill: %v", req.Action, err)
	}
	return &BackfillResponse{Backfill: bf}, nil
}

func startBackfillHandler(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
	in := new(StartBackfillRequest)
	if err := dec(in); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return srv.(specService).StartBackfill(ctx, in)
	}
	return interceptor(ctx, in, &grpc.UnaryServerInfo{
		Server: srv, FullMethod: "/nexus.v1.WorkflowSpecService/StartBackfill",
	}, func(ctx context.Context, req any) (any, error) {
		return srv.(specService).StartBackfill(ctx, req.(*StartBackfillRequest))
	})
}

func backfillActionHandler(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
	in := new(BackfillActionRequest)
	if err := dec(in); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return srv.(specService).BackfillAction(ctx, in)
	}
	return interceptor(ctx, in, &grpc.UnaryServerInfo{
		Server: srv, FullMethod: "/nexus.v1.WorkflowSpecService/BackfillAction",
	}, func(ctx context.Context, req any) (any, error) {
		return srv.(specService).BackfillAction(ctx, req.(*BackfillActionRequest))
	})
}

func (s *SpecServer) UpdateSchedule(ctx context.Context, req *CreateScheduleRequest) (*ScheduleResponse, error) {
	if req.Namespace == "" || req.Name == "" || req.SpecName == "" || req.CronExpr == "" {
		return nil, status.Error(codes.InvalidArgument, "namespace, name, spec_name and cron_expr are required")
	}
	if req.NextFireAt.IsZero() {
		// Refused rather than defaulted: a zero fire time would make the schedule
		// due at the Unix epoch, and the fire loop would immediately claim it and
		// fire a tick the author never asked for.
		return nil, status.Error(codes.InvalidArgument, "next_fire_at is required; the caller computes it from the cron expression")
	}
	row, err := store.WFMUpdateSchedule(ctx, s.db, store.Schedule{
		Namespace:     req.Namespace,
		Name:          req.Name,
		SpecName:      req.SpecName,
		SpecVersion:   req.SpecVersion,
		CronExpr:      req.CronExpr,
		Timezone:      req.Timezone,
		Params:        req.Params,
		Priority:      req.Priority,
		OverlapPolicy: req.OverlapPolicy,
		CatchupLimit:  req.CatchupLimit,
		Enabled:       req.Enabled,
		CreatedBy:     req.CreatedBy,
	}, req.NextFireAt)
	if err != nil {
		if errors.Is(err, store.ErrScheduleNotFound) {
			return nil, status.Error(codes.NotFound, "schedule not found")
		}
		return nil, status.Errorf(codes.Internal, "update schedule: %v", err)
	}
	s.log.InfoContext(ctx, "schedule updated",
		"schedule_id", row.ID, "namespace", row.Namespace, "name", row.Name,
		"cron", row.CronExpr, "enabled", row.Enabled, "next_fire_at", row.NextFireAt)
	return &ScheduleResponse{Schedule: row}, nil
}

func updateScheduleHandler(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
	in := new(CreateScheduleRequest)
	if err := dec(in); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return srv.(specService).UpdateSchedule(ctx, in)
	}
	return interceptor(ctx, in, &grpc.UnaryServerInfo{
		Server: srv, FullMethod: "/nexus.v1.WorkflowSpecService/UpdateSchedule",
	}, func(ctx context.Context, req any) (any, error) {
		return srv.(specService).UpdateSchedule(ctx, req.(*CreateScheduleRequest))
	})
}
