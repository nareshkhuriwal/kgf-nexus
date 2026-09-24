// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package upstream

import (
	"context"
	"fmt"
	"time"

	"google.golang.org/grpc"

	"github.com/khuriwalgroup/kgf-nexus/internal/store"
	rmv1 "github.com/khuriwalgroup/kgf-nexus/services/resource-manager/gen/nexus/rm/v1"
)

// QuarantineWorker asks the resource-manager (owner of `workers`) to take a
// worker out of the placement pool. duration 0 means indefinite.
func (c *ResourceManagerClient) QuarantineWorker(
	ctx context.Context, namespace, workerID, reason, actor string, duration time.Duration,
) (bool, error) {
	return c.quarantineCall(ctx, workerID, rmv1.WorkerState_WORKER_STATE_QUARANTINED, reason, actor, duration)
}

// ReleaseWorker returns a QUARANTINED worker to ACTIVE.
func (c *ResourceManagerClient) ReleaseWorker(ctx context.Context, namespace, workerID, reason, actor string) (bool, error) {
	return c.quarantineCall(ctx, workerID, rmv1.WorkerState_WORKER_STATE_ACTIVE, reason, actor, 0)
}

func (c *ResourceManagerClient) quarantineCall(
	ctx context.Context, workerID string, state rmv1.WorkerState, reason, actor string, d time.Duration,
) (bool, error) {
	if c.grpc == nil {
		return false, fmt.Errorf("quarantine worker: %w (resource-manager owns workers)", ErrUpstreamNotConfigured)
	}
	ctx, cancel := withCallTimeout(ctx)
	defer cancel()
	resp, err := c.grpc.client.SetWorkerState(ctx, &rmv1.SetWorkerStateRequest{
		WorkerId: workerID, State: state, Reason: reason, Actor: actor, QuarantineMs: d.Milliseconds(),
	})
	if err != nil {
		return false, fmt.Errorf("set worker %s state to %s: %w", workerID, state, err)
	}
	return resp.GetApplied(), nil
}

type dagInvalidateRequest struct {
	Namespace   string `json:"namespace"`
	TaskRef     string `json:"task_ref,omitempty"`
	TaskVersion *int32 `json:"task_version,omitempty"`
	CacheKey    string `json:"cache_key,omitempty"`
	Reason      string `json:"reason,omitempty"`
	Actor       string `json:"actor"`
}

type dagInvalidateResponse struct {
	InvalidationID int64     `json:"invalidation_id"`
	InvalidatedAt  time.Time `json:"invalidated_at"`
}

// InvalidateCache records a memo invalidation through the DAG engine, which
// owns memo_invalidations.
func (c *SchedulerClient) InvalidateCache(ctx context.Context, inv store.MemoInvalidation) (int64, time.Time, error) {
	if c.dag == nil {
		return 0, time.Time{}, fmt.Errorf("invalidate cache: %w (dag-engine owns memo_invalidations)", ErrUpstreamNotConfigured)
	}
	ctx, cancel := withCallTimeout(ctx)
	defer cancel()
	var resp dagInvalidateResponse
	if err := c.dag.conn.Invoke(ctx, "/nexus.v1.DagEngineService/InvalidateCache",
		&dagInvalidateRequest{
			Namespace: inv.Namespace, TaskRef: inv.TaskRef, TaskVersion: inv.TaskVersion,
			CacheKey: inv.CacheKey, Reason: inv.Reason, Actor: inv.Actor,
		}, &resp, grpc.ForceCodec(wfmJSONCodec{})); err != nil {
		return 0, time.Time{}, fmt.Errorf("invalidate cache in %s: %w", inv.Namespace, err)
	}
	return resp.InvalidationID, resp.InvalidatedAt, nil
}
