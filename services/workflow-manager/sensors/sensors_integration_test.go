//go:build integration

// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package sensors

// Sensor tests against a real Postgres (NEXUS_TEST_DATABASE_URL) and a real
// S3-compatible store (NEXUS_TEST_S3_*; Silo in CI and locally).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/khuriwalgroup/kgf-nexus/internal/store"
	"github.com/khuriwalgroup/kgf-nexus/internal/testdb"
	"github.com/khuriwalgroup/kgf-nexus/internal/tests3"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

type env struct {
	t      *testing.T
	ctx    context.Context
	pool   *pgxpool.Pool
	ns     string
	specID string
	mc     *minio.Client
	bucket string
	s3     S3Config
}

func setup(t *testing.T, needS3 bool) *env {
	t.Helper()
	ctx := context.Background()
	dsn := testdb.DSN(t)
	pool, err := pgxpool.New(ctx, dsn)
	if err == nil {
		err = pool.Ping(ctx)
	}
	if err != nil {
		testdb.SkipUnreachable(t, err)
	}
	t.Cleanup(pool.Close)
	e := &env{t: t, ctx: ctx, pool: pool, ns: "sn-" + uuid.NewString()[:8]}
	testdb.RegisterTenant(t, pool, e.ns)
	e.specID = e.spec("downstream")
	if needS3 {
		c := tests3.Get(t)
		e.s3 = S3Config{Endpoint: c.Endpoint, AccessKeyID: c.AccessKeyID, SecretAccessKey: c.SecretAccessKey, UseTLS: c.UseTLS}
		mc, err := minio.New(c.HostPort(), &minio.Options{Creds: credentials.NewStaticV4(c.AccessKeyID, c.SecretAccessKey, ""), Secure: c.UseTLS})
		if err != nil {
			t.Fatal(err)
		}
		e.mc = mc
		e.bucket = "sensor-" + uuid.NewString()[:8]
		if err := mc.MakeBucket(ctx, e.bucket, minio.MakeBucketOptions{}); err != nil {
			tests3.SkipUnreachable(t, err)
		}
	}
	return e
}

func (e *env) spec(name string) string {
	var id string
	if err := e.pool.QueryRow(e.ctx, `INSERT INTO workflow_specs (namespace, name, version, spec_hash, spec_json, created_by)
VALUES ($1, $2, 1, $3, '{}', 'test') RETURNING id::text`, e.ns, name, uuid.NewString()).Scan(&id); err != nil {
		e.t.Fatal(err)
	}
	return id
}

func (e *env) put(key string) {
	if _, err := e.mc.PutObject(e.ctx, e.bucket, key, bytes.NewReader([]byte("x")), 1, minio.PutObjectOptions{}); err != nil {
		e.t.Fatal(err)
	}
}

// dbRuns is a RunCreator with the run layer's idempotency: it inserts into
// workflow_runs under uq_runs_idem and returns the existing run on conflict.
// failAfterCreate makes the next N calls create the run and then report an
// error, as a crash between create and mark would look.
type dbRuns struct {
	e               *env
	calls           atomic.Int64
	failAfterCreate atomic.Int64
}

func (d *dbRuns) Create(ctx context.Context, req CreateRunRequest) (string, bool, error) {
	d.calls.Add(1)
	params, _ := json.Marshal(req.Params)
	detail, _ := json.Marshal(req.TriggerDetail)
	var id string
	created := true
	err := d.e.pool.QueryRow(ctx, `
INSERT INTO workflow_runs (spec_id, namespace, trigger_kind, idempotency_key, params, trigger_detail)
SELECT id, namespace, 'sensor', $3, $4::jsonb, $5::jsonb FROM workflow_specs WHERE namespace = $1 AND name = $2
ON CONFLICT (namespace, idempotency_key) WHERE idempotency_key IS NOT NULL DO NOTHING
RETURNING id::text`, req.Namespace, req.SpecName, req.IdempotencyKey, string(params), string(detail)).Scan(&id)
	if err != nil {
		created = false
		if err := d.e.pool.QueryRow(ctx, `SELECT id::text FROM workflow_runs WHERE namespace = $1 AND idempotency_key = $2`,
			req.Namespace, req.IdempotencyKey).Scan(&id); err != nil {
			return "", false, fmt.Errorf("%w: %v", ErrPermanent, err)
		}
	}
	if d.failAfterCreate.Load() > 0 {
		d.failAfterCreate.Add(-1)
		return "", false, errors.New("simulated crash after the run was created")
	}
	return id, created, nil
}

func (e *env) service(r *dbRuns, page int) *Service {
	reg := NewRegistry()
	reg.Register("s3", func(context.Context) (ObjectLister, error) { return NewS3Lister(e.s3) })
	cfg := DefaultConfig()
	cfg.PageSize = page
	return New(e.pool, r, reg, cfg, quiet())
}

func (e *env) sensor(name, kind string, cfg, mapping map[string]any, backfill bool) store.Sensor {
	s, err := store.SensorCreate(e.ctx, e.pool, store.Sensor{
		Namespace: e.ns, Name: name, Kind: kind, SpecName: "downstream", Config: cfg, ParamsMapping: mapping,
		PollIntervalSeconds: 5, Backfill: backfill, Priority: 100, Enabled: true, CreatedBy: "test",
	})
	if err != nil {
		e.t.Fatal(err)
	}
	return s
}

func (e *env) due(id string) {
	if _, err := e.pool.Exec(e.ctx, `UPDATE sensor_state SET next_poll_at = now() WHERE sensor_id = $1`, id); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) pass(svc *Service, id string) {
	e.t.Helper()
	e.due(id)
	if _, err := svc.Pass(e.ctx); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) runs() map[string]map[string]any {
	rows, err := e.pool.Query(e.ctx, `SELECT idempotency_key, params FROM workflow_runs WHERE namespace = $1 AND trigger_kind = 'sensor'`, e.ns)
	if err != nil {
		e.t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]map[string]any{}
	for rows.Next() {
		var k string
		var p []byte
		if err := rows.Scan(&k, &p); err != nil {
			e.t.Fatal(err)
		}
		m := map[string]any{}
		_ = json.Unmarshal(p, &m)
		out[k] = m
	}
	return out
}

func TestObjectSensorBaselineThenTriggersOnlyNewMatchingObjects(t *testing.T) {
	e := setup(t, true)
	for _, k := range []string{"in/a.csv", "in/b.csv", "in/c.csv"} {
		e.put(k)
	}
	r := &dbRuns{e: e}
	svc := e.service(r, 1000)
	s := e.sensor("arrivals", store.SensorObjectStore,
		map[string]any{"uri": "s3://" + e.bucket + "/in/", "pattern": "*.csv"},
		map[string]any{"path": "$.object.uri", "key": "{{ $.object.relative_key }}"}, false)
	e.pass(svc, s.ID)
	if n := len(e.runs()); n != 0 {
		t.Fatalf("baseline triggered %d runs", n)
	}
	e.put("in/d.csv")
	e.put("in/e.json") // does not match
	e.pass(svc, s.ID)
	runs := e.runs()
	if len(runs) != 1 {
		t.Fatalf("got %d runs, want 1 for in/d.csv", len(runs))
	}
	for _, p := range runs {
		if p["path"] != "s3://"+e.bucket+"/in/d.csv" || p["key"] != "d.csv" {
			t.Fatalf("params %v", p)
		}
	}
	e.pass(svc, s.ID)
	if len(e.runs()) != 1 {
		t.Fatal("a second pass re-triggered")
	}
	got, _ := store.SensorGet(e.ctx, e.pool, e.ns, "arrivals")
	if got.State == nil || got.State.Status != "ok" || got.State.TriggeredTotal != 1 || !got.State.BaselineDone {
		t.Fatalf("state %+v", got.State)
	}
}

func TestObjectSensorBackfillTriggersExisting(t *testing.T) {
	e := setup(t, true)
	for i := 0; i < 5; i++ {
		e.put(fmt.Sprintf("d/%02d.parquet", i))
	}
	r := &dbRuns{e: e}
	svc := e.service(r, 2) // tiny pages: the durable cursor carries the sweep
	s := e.sensor("bf", store.SensorObjectStore, map[string]any{"uri": "s3://" + e.bucket + "/d/"}, nil, true)
	for i := 0; i < 5; i++ {
		e.pass(svc, s.ID)
	}
	if n := len(e.runs()); n != 5 {
		t.Fatalf("backfill created %d runs, want 5", n)
	}
}

func TestCrashBetweenCreateAndMarkConvergesOnOneRun(t *testing.T) {
	e := setup(t, true)
	r := &dbRuns{e: e}
	svc := e.service(r, 1000)
	s := e.sensor("crash", store.SensorObjectStore, map[string]any{"uri": "s3://" + e.bucket + "/"}, nil, true)
	e.put("x.bin")
	r.failAfterCreate.Store(1)
	e.pass(svc, s.ID)
	obs, _ := store.SensorPending(e.ctx, e.pool, s.ID, 10)
	if len(obs) != 1 {
		t.Fatalf("after the simulated crash want 1 pending observation, got %d", len(obs))
	}
	e.pass(svc, s.ID)
	if n := len(e.runs()); n != 1 {
		t.Fatalf("%d runs after recovery, want exactly 1", n)
	}
	if r.calls.Load() != 2 {
		t.Fatalf("create called %d times, want 2 (original + retry under the same key)", r.calls.Load())
	}
}

// TestConcurrentReplicasTriggerEachObjectExactlyOnce is the multi-replica
// proof: six sensor services (distinct lease owners) poll the same sensor
// while objects keep arriving, with pages small enough that sweeps span
// several passes and replicas. Every object must produce exactly one run.
func TestConcurrentReplicasTriggerEachObjectExactlyOnce(t *testing.T) {
	e := setup(t, true)
	r := &dbRuns{e: e}
	s := e.sensor("race", store.SensorObjectStore, map[string]any{"uri": "s3://" + e.bucket + "/p/"}, nil, true)
	const objects = 60
	stop := make(chan struct{})
	var wg sync.WaitGroup
	var passErrs atomic.Int64
	for i := 0; i < 6; i++ {
		svc := e.service(r, 7)
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_, _ = e.pool.Exec(e.ctx, `UPDATE sensor_state SET next_poll_at = now() WHERE sensor_id = $1`, s.ID)
				if _, err := svc.Pass(e.ctx); err != nil {
					passErrs.Add(1)
				}
			}
		}()
	}
	for i := 0; i < objects; i++ {
		e.put(fmt.Sprintf("p/obj-%03d", i))
	}
	deadline := time.Now().Add(60 * time.Second)
	for len(e.runs()) < objects && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	time.Sleep(500 * time.Millisecond) // let in-flight passes finish
	close(stop)
	wg.Wait()
	runs := e.runs()
	if len(runs) != objects {
		t.Fatalf("%d runs for %d objects", len(runs), objects)
	}
	var triggered, total int
	if err := e.pool.QueryRow(e.ctx, `SELECT count(*) FILTER (WHERE outcome = 'triggered'), count(*) FROM sensor_observations WHERE sensor_id = $1`, s.ID).Scan(&triggered, &total); err != nil {
		t.Fatal(err)
	}
	if triggered != objects || total != objects {
		t.Fatalf("observations triggered=%d total=%d, want %d each", triggered, total, objects)
	}
	var dupRuns int
	if err := e.pool.QueryRow(e.ctx, `SELECT count(*) FROM (SELECT trigger_detail->>'item_key' k, count(*) FROM workflow_runs
 WHERE namespace = $1 AND trigger_kind = 'sensor' GROUP BY 1 HAVING count(*) > 1) x`, e.ns).Scan(&dupRuns); err != nil {
		t.Fatal(err)
	}
	if dupRuns != 0 {
		t.Fatalf("%d objects produced more than one run", dupRuns)
	}
	t.Logf("create calls=%d pass errors=%d", r.calls.Load(), passErrs.Load())
}

func TestLeaseFencing(t *testing.T) {
	e := setup(t, false)
	r := &dbRuns{e: e}
	svc := e.service(r, 10)
	_ = e.spec("upstream")
	s := e.sensor("fence", store.SensorRunSucceeded, map[string]any{"upstream_workflow": "upstream"}, nil, false)
	if err := store.SensorEnsureStates(e.ctx, e.pool); err != nil {
		t.Fatal(err)
	}
	claimed, err := store.SensorClaimDue(e.ctx, e.pool, "replica-a", 1000, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range claimed {
		found = found || c.ID == s.ID
	}
	if !found {
		t.Fatal("sensor not claimed")
	}
	// A second replica cannot claim it while leased...
	again, _ := store.SensorClaimDue(e.ctx, e.pool, "replica-b", 1000, time.Minute)
	for _, c := range again {
		if c.ID == s.ID {
			t.Fatal("a leased sensor was claimed twice")
		}
	}
	// ...and cannot write its progress.
	if err := store.SensorSaveProgress(e.ctx, e.pool, s.ID, "replica-b", store.SensorProgress{NextPollAt: time.Now()}); !errors.Is(err, store.ErrSensorLeaseLost) {
		t.Fatalf("stale owner wrote progress: %v", err)
	}
	if err := store.SensorSaveProgress(e.ctx, e.pool, s.ID, "replica-a", store.SensorProgress{NextPollAt: time.Now()}); err != nil {
		t.Fatalf("owner could not write: %v", err)
	}
	_ = svc
}

func TestRunSucceededSensorMapsUpstreamParams(t *testing.T) {
	e := setup(t, false)
	up := e.spec("upstream")
	r := &dbRuns{e: e}
	svc := e.service(r, 100)
	// A run that succeeded BEFORE the sensor existed must not trigger (no backfill).
	e.upstreamRun(up, "2026-01-01")
	s := e.sensor("after-upstream", store.SensorRunSucceeded, map[string]any{"upstream_workflow": "upstream"},
		map[string]any{"date": "$.run.params.date", "upstream_run": "$.run.id"}, false)
	e.pass(svc, s.ID)
	id := e.upstreamRun(up, "2026-01-02")
	e.pass(svc, s.ID)
	e.pass(svc, s.ID) // lookback re-reads it; the observation key dedupes
	runs := e.runs()
	if len(runs) != 1 {
		t.Fatalf("%d downstream runs, want 1", len(runs))
	}
	for _, p := range runs {
		if p["date"] != "2026-01-02" || p["upstream_run"] != id {
			t.Fatalf("params %v", p)
		}
	}
}

func (e *env) upstreamRun(specID, date string) string {
	var id string
	if err := e.pool.QueryRow(e.ctx, `INSERT INTO workflow_runs (spec_id, namespace, state, trigger_kind, params, ended_at)
VALUES ($1, $2, 'SUCCEEDED', 'manual', jsonb_build_object('date', $3::text), clock_timestamp()) RETURNING id::text`, specID, e.ns, date).Scan(&id); err != nil {
		e.t.Fatal(err)
	}
	return id
}
