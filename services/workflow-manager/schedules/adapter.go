package schedules

import (
	"context"
	"time"

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
