// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package sensors

import (
	"context"
	"errors"
	"fmt"

	"github.com/khuriwalgroup/kgf-nexus/internal/store"
	"github.com/khuriwalgroup/kgf-nexus/services/workflow-manager/runs"
)

// RunsAdapter lets runs.Service satisfy RunCreator, classifying failures that
// retrying cannot fix as ErrPermanent.
type RunsAdapter struct{ svc *runs.Service }

// NewRunsAdapter wraps the run service.
func NewRunsAdapter(svc *runs.Service) *RunsAdapter { return &RunsAdapter{svc: svc} }

// Create submits the run.
func (a *RunsAdapter) Create(ctx context.Context, req CreateRunRequest) (string, bool, error) {
	res, err := a.svc.Create(ctx, runs.CreateRequest{
		Namespace: req.Namespace, SpecName: req.SpecName, SpecVersion: req.SpecVersion,
		IdempotencyKey: req.IdempotencyKey, TriggerKind: TriggerKind, TriggerDetail: req.TriggerDetail,
		Params: req.Params, Priority: req.Priority,
	})
	if err != nil {
		if errors.Is(err, store.ErrNotFound) || errors.Is(err, runs.ErrMissingParams) {
			return "", false, fmt.Errorf("%w: %v", ErrPermanent, err)
		}
		// Quotas and max_active_runs are transient: the observation stays pending
		// and is retried on the next poll.
		return "", false, err
	}
	return res.Run.ID, res.Created, nil
}
