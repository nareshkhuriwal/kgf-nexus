// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// The service-instance registry (migration 0041, ADR-0037).
//
// Unlike every other file here, the writes in this one are made by ALL six
// services: each control-plane process upserts the one row that describes it.
// The table is platform-scoped and carries no authoritative state, so it is
// the documented exception to one-owner-per-table (ADR-0037 §2): one writer
// per ROW, the process itself.

// InstanceExecer is the write surface the heartbeat needs: a pool, a
// connection or a transaction.
type InstanceExecer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// ServiceInstance is one process's registry row as its heartbeat writes it.
// last_seen_at is not a field: the database stamps it with its own clock, so
// staleness is judged against one clock whatever the hosts' clocks say.
type ServiceInstance struct {
	Component    string
	Instance     string
	PodNamespace string
	Node         string
	Cluster      string
	Version      string
	StartedAt    time.Time
	Ready        bool
	// Detail is the component's JSON object (ADR-0037 §3). Nil writes {}.
	Detail map[string]any
}

// MaxInstanceDetailBytes mirrors the table's CHECK on detail. A detail object
// over it is replaced by a marker rather than failing the beat: the row's
// liveness matters more than its decoration.
const MaxInstanceDetailBytes = 16384

// UpsertServiceInstance writes the process's row: inserted on the first beat
// of a new identity, updated on every later one (and on the first beat of a
// restarted StatefulSet pod, which keeps its name).
func UpsertServiceInstance(ctx context.Context, db InstanceExecer, in ServiceInstance) error {
	detail, err := encodeInstanceDetail(in.Detail)
	if err != nil {
		return err
	}
	_, err = db.Exec(ctx, `
		INSERT INTO service_instances
		       (component, instance, pod_namespace, node, cluster, version,
		        started_at, last_seen_at, ready, detail)
		VALUES ($1, $2, $3, $4, $5, $6, $7, now(), $8, $9::jsonb)
		ON CONFLICT (component, cluster, pod_namespace, instance) DO UPDATE
		   SET node         = EXCLUDED.node,
		       version      = EXCLUDED.version,
		       started_at   = EXCLUDED.started_at,
		       last_seen_at = EXCLUDED.last_seen_at,
		       ready        = EXCLUDED.ready,
		       detail       = EXCLUDED.detail`,
		in.Component, in.Instance, in.PodNamespace, in.Node, in.Cluster, in.Version,
		in.StartedAt.UTC(), in.Ready, detail)
	if err != nil {
		return fmt.Errorf("upsert service instance %s/%s: %w", in.Component, in.Instance, err)
	}
	return nil
}

func encodeInstanceDetail(d map[string]any) (string, error) {
	if d == nil {
		return "{}", nil
	}
	b, err := json.Marshal(d)
	if err != nil {
		return "", fmt.Errorf("encode instance detail: %w", err)
	}
	// The CHECK measures the jsonb's text form, which normalises whitespace
	// but not content; json.Marshal emits none, so the lengths agree closely
	// enough for a cap with 16 KiB of room.
	if len(b) > MaxInstanceDetailBytes-64 {
		return `{"detail_truncated": true}`, nil
	}
	return string(b), nil
}

// PruneServiceInstances deletes rows not seen for longer than retention: the
// pods of past deploys and processes that died without a graceful shutdown.
// Any replica of any component may run it; it is idempotent.
func PruneServiceInstances(ctx context.Context, db InstanceExecer, retention time.Duration) (int64, error) {
	tag, err := db.Exec(ctx,
		`DELETE FROM service_instances WHERE last_seen_at < now() - make_interval(secs => $1)`,
		retention.Seconds())
	if err != nil {
		return 0, fmt.Errorf("prune service instances: %w", err)
	}
	return tag.RowsAffected(), nil
}

// ServiceInstanceView is a registry row as the gateway reads it.
type ServiceInstanceView struct {
	Component    string
	Instance     string
	PodNamespace string
	Node         string
	Cluster      string
	Version      string
	StartedAt    time.Time
	LastSeenAt   time.Time
	Ready        bool
	// Stale is decided by the database: last_seen_at older than the
	// threshold at the listing's own now().
	Stale  bool
	Detail json.RawMessage
}

// GatewayListServiceInstances returns every registry row, ordered by
// component and then by identity (a stable order: the console does not
// reshuffle between polls), together with the database time the staleness was
// judged at. staleAfter is the threshold (ADR-0037: 30 s).
//
// One statement, so the observation time and every row's staleness come from
// the same now(); the LEFT JOIN returns that time even when the table is
// empty.
func GatewayListServiceInstances(ctx context.Context, q GWQuerier, staleAfter time.Duration) (time.Time, []ServiceInstanceView, error) {
	rows, err := q.Query(ctx, `
		SELECT t.at, si.component, si.instance, si.pod_namespace, si.node, si.cluster,
		       si.version, si.started_at, si.last_seen_at, si.ready, si.detail::text,
		       si.last_seen_at < t.at - make_interval(secs => $1)
		  FROM (SELECT now() AS at) t
		  LEFT JOIN service_instances si ON true
		 ORDER BY si.component, si.cluster, si.pod_namespace, si.instance`,
		staleAfter.Seconds())
	if err != nil {
		return time.Time{}, nil, fmt.Errorf("list service instances: %w", err)
	}
	defer rows.Close()

	var (
		at  time.Time
		out = []ServiceInstanceView{}
	)
	for rows.Next() {
		var (
			component, instance, podNS, node, cluster, version, detail *string
			started, seen                                              *time.Time
			ready, stale                                               *bool
		)
		if err := rows.Scan(&at, &component, &instance, &podNS, &node, &cluster,
			&version, &started, &seen, &ready, &detail, &stale); err != nil {
			return time.Time{}, nil, fmt.Errorf("scan service instance: %w", err)
		}
		if component == nil {
			continue // the empty table's single all-NULL row
		}
		out = append(out, ServiceInstanceView{
			Component: *component, Instance: *instance, PodNamespace: *podNS,
			Node: *node, Cluster: *cluster, Version: *version,
			StartedAt: started.UTC(), LastSeenAt: seen.UTC(),
			Ready: *ready, Stale: *stale, Detail: json.RawMessage(*detail),
		})
	}
	if err := rows.Err(); err != nil {
		return time.Time{}, nil, fmt.Errorf("list service instances: %w", err)
	}
	return at.UTC(), out, nil
}
