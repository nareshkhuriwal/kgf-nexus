package upstream

import (
	"context"
	"fmt"
	"time"

	"github.com/khuriwalgroup/kgf-nexus/internal/store"
	"google.golang.org/grpc"
)

// Schedule WRITES go through workflow-manager, which owns the table.
//
// Reads do not: the gateway reads `schedules` directly, because ADR-0011 permits it
// to read every table and a listing is a projection like any other. The asymmetry is
// not a shortcut -- migration 0005's grants refuse a write from any other role at the
// database, so routing one locally would fail there rather than at review, which is
// the whole point of having the grants.

// wfmScheduleResponse mirrors grpcapi.ScheduleResponse.
type wfmScheduleResponse struct {
	Schedule store.Schedule `json:"schedule"`
}

type wfmCreateScheduleRequest struct {
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

type wfmSetScheduleEnabledRequest struct {
	Namespace  string     `json:"namespace"`
	Name       string     `json:"name"`
	Enabled    bool       `json:"enabled"`
	NextFireAt *time.Time `json:"next_fire_at"`
}

type wfmDeleteScheduleRequest struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

type wfmDeleteScheduleResponse struct{}

// CreateSchedule creates a schedule through its owner.
//
// nextFireAt is computed by the CALLER and sent, because the gateway has already
// parsed the cron to validate it and to return the author a preview. Recomputing it
// in the owner would be a second place deciding when a schedule is first due, and
// two such places eventually disagree.
func (c *WorkflowManagerClient) CreateSchedule(ctx context.Context, s store.Schedule, nextFireAt time.Time) (store.Schedule, error) {
	if c.grpc == nil {
		return store.Schedule{}, fmt.Errorf("create schedule: %w (workflow-manager owns schedules)", ErrUpstreamNotConfigured)
	}
	ctx, cancel := withCallTimeout(ctx)
	defer cancel()

	var resp wfmScheduleResponse
	if err := c.grpc.conn.Invoke(ctx, "/nexus.v1.WorkflowSpecService/CreateSchedule",
		&wfmCreateScheduleRequest{
			Namespace: s.Namespace, Name: s.Name, SpecName: s.SpecName,
			SpecVersion: s.SpecVersion, CronExpr: s.CronExpr, Timezone: s.Timezone,
			Params: s.Params, Priority: s.Priority, OverlapPolicy: s.OverlapPolicy,
			CatchupLimit: s.CatchupLimit, Enabled: s.Enabled, CreatedBy: s.CreatedBy,
			NextFireAt: nextFireAt,
		}, &resp, grpc.ForceCodec(wfmJSONCodec{})); err != nil {
		return store.Schedule{}, fmt.Errorf("create schedule %s/%s: %w", s.Namespace, s.Name, err)
	}
	return resp.Schedule, nil
}

// SetScheduleEnabled toggles a schedule through its owner.
func (c *WorkflowManagerClient) SetScheduleEnabled(ctx context.Context, namespace, name string, enabled bool, nextFireAt *time.Time) (store.Schedule, error) {
	if c.grpc == nil {
		return store.Schedule{}, fmt.Errorf("set schedule enabled: %w (workflow-manager owns schedules)", ErrUpstreamNotConfigured)
	}
	ctx, cancel := withCallTimeout(ctx)
	defer cancel()

	var resp wfmScheduleResponse
	if err := c.grpc.conn.Invoke(ctx, "/nexus.v1.WorkflowSpecService/SetScheduleEnabled",
		&wfmSetScheduleEnabledRequest{
			Namespace: namespace, Name: name, Enabled: enabled, NextFireAt: nextFireAt,
		}, &resp, grpc.ForceCodec(wfmJSONCodec{})); err != nil {
		return store.Schedule{}, fmt.Errorf("set schedule %s/%s enabled=%v: %w", namespace, name, enabled, err)
	}
	return resp.Schedule, nil
}

// DeleteSchedule removes a schedule through its owner.
func (c *WorkflowManagerClient) DeleteSchedule(ctx context.Context, namespace, name string) error {
	if c.grpc == nil {
		return fmt.Errorf("delete schedule: %w (workflow-manager owns schedules)", ErrUpstreamNotConfigured)
	}
	ctx, cancel := withCallTimeout(ctx)
	defer cancel()

	var resp wfmDeleteScheduleResponse
	if err := c.grpc.conn.Invoke(ctx, "/nexus.v1.WorkflowSpecService/DeleteSchedule",
		&wfmDeleteScheduleRequest{Namespace: namespace, Name: name},
		&resp, grpc.ForceCodec(wfmJSONCodec{})); err != nil {
		return fmt.Errorf("delete schedule %s/%s: %w", namespace, name, err)
	}
	return nil
}
