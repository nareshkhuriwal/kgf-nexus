// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0
package store

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/jackc/pgx/v5"
	"github.com/khuriwalgroup/kgf-nexus/internal/platform"
)

var ErrPlatformConflict = errors.New("platform policy revision changed; reload before saving")

func GatewayPlatformGet(ctx context.Context, q GWQuerier, ns string) (platform.Config, error) {
	c := platform.Default()
	var raw []byte
	err := q.QueryRow(ctx, `SELECT revision,config FROM platform_config WHERE namespace=$1`, ns).Scan(&c.Revision, &raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	rev := c.Revision
	err = json.Unmarshal(raw, &c)
	c.Revision = rev
	// A document saved with "pools": null (before Normalize existed) would
	// otherwise overwrite Default()'s empty list with nil.
	c.Normalize()
	return c, err
}
func GatewayPlatformPut(ctx context.Context, db GWDB, ns string, c platform.Config, audit AuditEntry) (platform.Config, error) {
	if err := c.Validate(); err != nil {
		return c, err
	}
	tx, err := db.Begin(ctx)
	if err != nil {
		return c, err
	}
	defer tx.Rollback(ctx)
	old := c.Revision
	c.Revision++
	c.Normalize()
	raw, err := json.Marshal(c)
	if err != nil {
		return c, err
	}
	tag, err := tx.Exec(ctx, `INSERT INTO platform_config(namespace,revision,config) SELECT $1,$2,$3 WHERE $4::bigint=0 ON CONFLICT(namespace) DO NOTHING`, ns, c.Revision, raw, old)
	if err != nil {
		return c, err
	}
	if old > 0 {
		tag, err = tx.Exec(ctx, `UPDATE platform_config SET revision=$2,config=$3,updated_at=now() WHERE namespace=$1 AND revision=$4`, ns, c.Revision, raw, old)
	}
	if err != nil {
		return c, err
	}
	if tag.RowsAffected() != 1 {
		return c, ErrPlatformConflict
	}
	audit.Action = "platform.config.update"
	audit.TargetKind = "platform_config"
	audit.TargetID = ns
	audit.Namespace = ns
	audit.Outcome = AuditSuccess
	audit.Detail = map[string]any{"revision": c.Revision}
	if err = GatewayAppendAudit(ctx, tx, audit); err != nil {
		return c, err
	}
	return c, tx.Commit(ctx)
}

// One bounded aggregate per queue; raw task payloads never leave the store.
func GatewayPlatformDemand(ctx context.Context, q GWQuerier, ns string, p platform.Pool, profile platform.PodProfile) (platform.Demand, error) {
	var d platform.Demand
	err := q.QueryRow(ctx, `SELECT count(*) FILTER(WHERE a.state='PENDING'),count(*) FILTER(WHERE a.state IN ('DISPATCHED','RUNNING')),
 COALESCE(sum((a.resource_request->>'cpu_millis')::bigint),0),COALESCE(ceil(sum((a.resource_request->>'memory_bytes')::numeric)/1048576),0)::bigint,COALESCE(sum((a.resource_request->>'gpu_count')::int),0),
 count(*) FILTER(WHERE COALESCE((a.resource_request->>'cpu_millis')::bigint,0)>$3 OR COALESCE((a.resource_request->>'memory_bytes')::bigint,0)>$4 OR COALESCE((a.resource_request->>'gpu_count')::int,0)>$5)
 FROM task_attempts a JOIN workflow_runs r ON r.id=a.run_id WHERE r.namespace=$1 AND a.queue=$2 AND a.state IN ('PENDING','DISPATCHED','RUNNING')`, ns, p.Queue, profile.CPU, profile.Memory*1048576, profile.GPU).Scan(&d.Pending, &d.Running, &d.CPU, &d.Memory, &d.GPU, &d.Unschedulable)
	if err != nil {
		return d, err
	}
	err = q.QueryRow(ctx, `SELECT count(*) FROM workers WHERE namespace=$1 AND $2=ANY(queues) AND state IN ('ACTIVE','DRAINING') AND last_heartbeat_at>now()-interval '30 seconds'`, ns, p.Queue).Scan(&d.CurrentWorkers)
	// A single snapshot cannot prove an idle duration: scale-in remains held.
	return d, err
}
