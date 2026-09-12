package schedules

import (
	"context"
	"time"

	"github.com/khuriwalgroup/kgf-nexus/internal/store"
	"github.com/khuriwalgroup/kgf-nexus/services/workflow-manager/grpcapi"
	"github.com/khuriwalgroup/kgf-nexus/services/workflow-manager/runs"
)

// RunsAdapter lets runs.Service satisfy RunCreator.
//
// An adapter rather than widening the interface so the dependency stays one-way:
// schedules knows how to ask for a run, and runs knows nothing about schedules.
// Without it, either runs would import schedules (a cycle waiting to happen) or the
// fire logic would take the concrete service and stop being testable without a
// database.
type RunsAdapter struct{ svc *runs.Service }

// NewRunsAdapter wraps the run service.
func NewRunsAdapter(svc *runs.Service) *RunsAdapter { return &RunsAdapter{svc: svc} }

// Create submits the run, translating the schedule's request shape.
func (a *RunsAdapter) Create(ctx context.Context, req CreateRunRequest) (string, bool, error) {
	logical := req.LogicalDate
	var logicalPtr *time.Time
	if !logical.IsZero() {
		logicalPtr = &logical
	}
	res, err := a.svc.Create(ctx, runs.CreateRequest{
		Namespace: req.Namespace,
		// SpecName, not SpecID: a schedule tracks the current definition of its
		// workflow, and runs.Create pins the version it resolves so the run stays
		// reproducible (ADR-0004).
		SpecName:       req.SpecName,
		IdempotencyKey: req.IdempotencyKey,
		TriggerKind:    req.TriggerKind,
		TriggerDetail:  req.TriggerDetail,
		Params:         req.Params,
		LogicalDate:    logicalPtr,
		Priority:       req.Priority,
	})
	if err != nil {
		return "", false, err
	}
	return res.Run.ID, res.Created, nil
}

// BackfillAdapter lets Service satisfy grpcapi.BackfillService.
//
// The same one-way-dependency argument as RunsAdapter: the gRPC layer defines the
// shape it needs and this translates, so schedules does not import grpcapi and
// grpcapi does not import schedules.
type BackfillAdapter struct{ svc *Service }

// NewBackfillAdapter wraps the schedule service.
func NewBackfillAdapter(svc *Service) *BackfillAdapter { return &BackfillAdapter{svc: svc} }

// StartBackfill translates the request shape.
func (a *BackfillAdapter) StartBackfill(ctx context.Context, req grpcapi.BackfillStart) (store.Backfill, error) {
	return a.svc.StartBackfill(ctx, BackfillRequest{
		Namespace: req.Namespace, ScheduleID: req.ScheduleID,
		From: req.From, To: req.To,
		MaxActive: req.MaxActive, MaxRuns: req.MaxRuns, CreatedBy: req.CreatedBy,
	})
}

func (a *BackfillAdapter) PauseBackfill(ctx context.Context, ns, id string) (store.Backfill, error) {
	return a.svc.PauseBackfill(ctx, ns, id)
}

func (a *BackfillAdapter) ResumeBackfill(ctx context.Context, ns, id string) (store.Backfill, error) {
	return a.svc.ResumeBackfill(ctx, ns, id)
}

func (a *BackfillAdapter) CancelBackfill(ctx context.Context, ns, id string) (store.Backfill, error) {
	return a.svc.CancelBackfill(ctx, ns, id)
}
