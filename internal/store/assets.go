// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package store

// Read projections of declared asset lineage (ADR-0028), for the gateway.

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// Asset is one catalogued asset.
type Asset struct {
	URI                   string
	Name                  string
	FirstSeenAt           time.Time
	LastDeclaredAt        time.Time
	LastMaterializedAt    *time.Time
	LastMaterializedRunID string
	Producers             int64
	Consumers             int64
}

const assetCols = `a.uri, a.name, a.first_seen_at, a.last_declared_at, a.last_materialized_at,
  COALESCE(a.last_materialized_run_id::text, ''),
  (SELECT count(DISTINCT (e.spec_name, e.node_id)) FROM asset_lineage_edges e
    WHERE e.namespace = a.namespace AND e.asset_uri = a.uri AND e.direction = 'produces'),
  (SELECT count(DISTINCT (e.spec_name, e.node_id)) FROM asset_lineage_edges e
    WHERE e.namespace = a.namespace AND e.asset_uri = a.uri AND e.direction = 'consumes')`

func scanAsset(row pgx.Row) (Asset, error) {
	var x Asset
	err := row.Scan(&x.URI, &x.Name, &x.FirstSeenAt, &x.LastDeclaredAt, &x.LastMaterializedAt,
		&x.LastMaterializedRunID, &x.Producers, &x.Consumers)
	return x, err
}

// AssetList lists a namespace's assets, optionally filtered by a substring of
// the URI or name.
func AssetList(ctx context.Context, q GWQuerier, ns, search string, limit int) ([]Asset, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	rows, err := q.Query(ctx, `SELECT `+assetCols+` FROM assets a
 WHERE a.namespace = $1 AND ($2 = '' OR a.uri ILIKE '%' || $2 || '%' OR a.name ILIKE '%' || $2 || '%')
 ORDER BY a.uri LIMIT $3`, ns, search, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Asset
	for rows.Next() {
		x, err := scanAsset(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// AssetGet reads one asset.
func AssetGet(ctx context.Context, q GWQuerier, ns, uri string) (Asset, error) {
	x, err := scanAsset(q.QueryRow(ctx, `SELECT `+assetCols+` FROM assets a WHERE a.namespace = $1 AND a.uri = $2`, ns, uri))
	if errors.Is(err, pgx.ErrNoRows) {
		return x, ErrNotifyNotFound
	}
	return x, err
}

// AssetTask is a (workflow, task) that declares an asset.
type AssetTask struct {
	Workflow     string
	NodeID       string
	LastRunID    string
	LastRecorded time.Time
	Runs         int64
}

// AssetTasks lists the tasks that produce (or consume) an asset.
func AssetTasks(ctx context.Context, q GWQuerier, ns, uri, direction string) ([]AssetTask, error) {
	rows, err := q.Query(ctx, `
SELECT spec_name, node_id,
       (array_agg(run_id::text ORDER BY recorded_at DESC))[1], max(recorded_at), count(*)
  FROM asset_lineage_edges
 WHERE namespace = $1 AND asset_uri = $2 AND direction = $3
 GROUP BY spec_name, node_id
 ORDER BY max(recorded_at) DESC
 LIMIT 200`, ns, uri, direction)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AssetTask
	for rows.Next() {
		var t AssetTask
		if err := rows.Scan(&t.Workflow, &t.NodeID, &t.LastRunID, &t.LastRecorded, &t.Runs); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// Materialization is one recorded build of an asset.
type Materialization struct {
	RunID          string
	NodeID         string
	Workflow       string
	MaterializedAt time.Time
	RunState       string
}

// AssetMaterializations lists an asset's most recent materializations.
func AssetMaterializations(ctx context.Context, q GWQuerier, ns, uri string, limit int) ([]Materialization, error) {
	if limit <= 0 || limit > 200 {
		limit = 20
	}
	rows, err := q.Query(ctx, `
SELECT m.run_id::text, m.node_id, m.spec_name, m.materialized_at, COALESCE(r.state::text, '')
  FROM asset_materializations m LEFT JOIN workflow_runs r ON r.id = m.run_id
 WHERE m.namespace = $1 AND m.asset_uri = $2
 ORDER BY m.materialized_at DESC LIMIT $3`, ns, uri, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Materialization
	for rows.Next() {
		var m Materialization
		if err := rows.Scan(&m.RunID, &m.NodeID, &m.Workflow, &m.MaterializedAt, &m.RunState); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// LineageEdge is one declared (workflow task, direction, asset) triple.
type LineageEdge struct {
	Workflow  string
	NodeID    string
	Direction string
	AssetURI  string
	AssetName string
}

// AssetLineageEdges returns the namespace's distinct declared edges, bounded.
// truncated reports that the bound was hit.
func AssetLineageEdges(ctx context.Context, q GWQuerier, ns string, limit int) ([]LineageEdge, bool, error) {
	if limit <= 0 || limit > 20000 {
		limit = 5000
	}
	rows, err := q.Query(ctx, `
SELECT DISTINCT spec_name, node_id, direction, asset_uri, asset_name
  FROM asset_lineage_edges WHERE namespace = $1
 ORDER BY spec_name, node_id, direction, asset_uri
 LIMIT $2`, ns, limit+1)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	var out []LineageEdge
	for rows.Next() {
		var e LineageEdge
		if err := rows.Scan(&e.Workflow, &e.NodeID, &e.Direction, &e.AssetURI, &e.AssetName); err != nil {
			return nil, false, err
		}
		out = append(out, e)
	}
	if len(out) > limit {
		return out[:limit], true, rows.Err()
	}
	return out, false, rows.Err()
}
