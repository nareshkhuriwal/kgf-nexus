//go:build bench

// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

// Package bench measures the Postgres-as-queue dispatch ceiling (ADR-0018).
//
// It drives the REAL claim code -- store.ClaimForWorker (the pull path every
// worker poll takes) and store.Claim (the push loop) -- against a real Postgres,
// with R scheduler "replicas" (goroutines, each with its OWN connection pool, so
// they contend exactly as separate processes do on the database) and a backlog
// of B PENDING attempts. It reports claims/sec and p50/p99 latency of one claim
// transaction for each (mode, backlog, replicas) cell.
//
// It needs a DISPOSABLE, migrated database: `make bench-pg` starts one. It is
// behind the `bench` build tag so `go test ./...` never runs it, and it refuses
// to run without NEXUS_BENCH_DSN rather than defaulting to a database someone
// may care about.
//
// These are laptop numbers. They locate the knee of the curve and show which
// resource saturates first; they are not a production capacity figure.
package bench

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/khuriwalgroup/kgf-nexus/internal/store"
)

func envInts(key, def string) []int {
	v := os.Getenv(key)
	if v == "" {
		v = def
	}
	var out []int
	for _, s := range strings.Split(v, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(s))
		if err == nil && n > 0 {
			out = append(out, n)
		}
	}
	return out
}

func envStr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

type world struct {
	ns      string
	queue   string
	workers []uuid.UUID
}

// seed builds one namespace with `backlog` PENDING attempts spread over many
// runs, and `nWorkers` ACTIVE workers with effectively unbounded capacity.
//
// Spread over runs because every dispatch bumps its run's event_seq (the
// CONTRACT seq rule): a single-run backlog would measure contention on one
// workflow_runs row, which is an artefact of the fixture, not of the queue.
// Unbounded capacity because this measures the QUEUE, not bin packing.
func seed(ctx context.Context, t *testing.T, pool *pgxpool.Pool, backlog, nWorkers int) *world {
	t.Helper()
	id := uuid.NewString()[:8]
	w := &world{ns: "bench-" + id, queue: "bench-" + id}
	must := func(err error, what string) {
		if err != nil {
			t.Fatalf("seed %s: %v", what, err)
		}
	}
	_, err := pool.Exec(ctx, `INSERT INTO tenants(namespace,name) VALUES ($1,$1)`, w.ns)
	must(err, "tenant")
	var specID uuid.UUID
	must(pool.QueryRow(ctx, `INSERT INTO workflow_specs (namespace,name,version,spec_hash,spec_json,created_by)
VALUES ($1,'bench',1,$2,'{}','bench') RETURNING id`, w.ns, "h-"+id).Scan(&specID), "spec")

	nRuns := backlog / 100
	if nRuns < 10 {
		nRuns = 10
	}
	_, err = pool.Exec(ctx, `INSERT INTO workflow_runs (spec_id,namespace,state,trigger_kind)
SELECT $1,$2,'RUNNING','bench' FROM generate_series(1,$3)`, specID, w.ns, nRuns)
	must(err, "runs")
	_, err = pool.Exec(ctx, `
WITH runs AS (SELECT array_agg(id ORDER BY id) AS ids FROM workflow_runs WHERE namespace=$1)
INSERT INTO task_nodes (run_id,node_id,task_ref,state,max_attempts,spec_json)
SELECT runs.ids[1 + i % $3], 'n'||i, 'bench.task','RUNNING',3,'{}'
  FROM runs, generate_series(0,$2-1) i`, w.ns, backlog, nRuns)
	must(err, "nodes")
	_, err = pool.Exec(ctx, `
WITH runs AS (SELECT array_agg(id ORDER BY id) AS ids FROM workflow_runs WHERE namespace=$1)
INSERT INTO task_attempts (run_id,node_id,attempt_number,state,queue,priority,resource_request,idempotency_key,scheduled_at)
SELECT runs.ids[1 + i % $4], 'n'||i, 1, 'PENDING', $2, 100 + (i % 7),
       '{"cpu_millis":1,"memory_bytes":1}', $1||'-'||i, now() - make_interval(secs => i % 600)
  FROM runs, generate_series(0,$3-1) i`, w.ns, w.queue, backlog, nRuns)
	must(err, "attempts")
	for i := 0; i < nWorkers; i++ {
		var wid uuid.UUID
		must(pool.QueryRow(ctx, `
INSERT INTO workers (namespace,hostname,queues,capacity,committed,state,agent_version,session_token_hash,last_heartbeat_at)
VALUES ($1,$2,ARRAY[$3],'{"cpu_millis":1000000000000,"memory_bytes":1000000000000000}','{}','ACTIVE','bench','h',now()+interval '1 hour')
RETURNING id`, w.ns, fmt.Sprintf("bench-%d", i), w.queue).Scan(&wid), "worker")
		w.workers = append(w.workers, wid)
	}
	// NEXUS_BENCH_QUOTA=true gives the namespace concurrent quotas far above
	// anything the cell reaches (ADR-0021), so every claim takes the quota
	// path -- lock, usage read, check -- without ever being refused. That is
	// the cost a quota-bound namespace pays; without it the cell measures the
	// unlimited namespace, which pays only the lookup.
	if envStr("NEXUS_BENCH_QUOTA", "false") == "true" {
		_, err = pool.Exec(ctx, `INSERT INTO platform_config (namespace, revision, config)
VALUES ($1, 1, '{"quotas":{"max_inflight_attempts":1000000,"max_cpu_millis":1000000000000}}')`, w.ns)
		must(err, "quotas")
	}
	_, err = pool.Exec(ctx, `ANALYZE task_attempts; ANALYZE task_nodes; ANALYZE workflow_runs; ANALYZE workers`)
	must(err, "analyze")
	return w
}

func (w *world) cleanup(ctx context.Context, pool *pgxpool.Pool) {
	_, _ = pool.Exec(ctx, `DELETE FROM timers WHERE subject_id IN (SELECT id FROM task_attempts WHERE queue=$1)`, w.queue)
	_, _ = pool.Exec(ctx, `DELETE FROM workflow_runs WHERE namespace=$1`, w.ns)
	_, _ = pool.Exec(ctx, `DELETE FROM workflow_specs WHERE namespace=$1`, w.ns)
	_, _ = pool.Exec(ctx, `DELETE FROM workers WHERE namespace=$1`, w.ns)
	_, _ = pool.Exec(ctx, `DELETE FROM platform_config WHERE namespace=$1`, w.ns)
	_, _ = pool.Exec(ctx, `DELETE FROM tenants WHERE namespace=$1`, w.ns)
}

type cell struct {
	mode              string
	backlog, replicas int
	claims, misses    int64
	errors            int64
	elapsed           time.Duration
	p50, p99, max     time.Duration
}

func pct(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	i := int(p * float64(len(sorted)-1))
	return sorted[i]
}

// run drives one cell: `replicas` goroutines claim until `target` dispatches
// have committed (or the time limit passes).
func run(ctx context.Context, t *testing.T, dsn, mode string, w *world, replicas, target int, limit time.Duration) cell {
	t.Helper()
	var (
		done, misses, errs atomic.Int64
		mu                 sync.Mutex
		lat                []time.Duration
		wg                 sync.WaitGroup
	)
	stores := make([]*store.SchedulerStore, replicas)
	for r := 0; r < replicas; r++ {
		pc := store.DefaultPoolConfig(dsn)
		pc.MaxConns, pc.MinConns = 2, 1
		p, err := store.NewPool(ctx, pc)
		if err != nil {
			t.Fatalf("pool: %v", err)
		}
		defer p.Close()
		cfg := store.DefaultClaimConfig(w.queue)
		// NEXUS_BENCH_FAIR_SHARE=false measures the pre-ADR-0017 order, so the
		// fair-share term's own cost can be separated from the queue's.
		cfg.FairShare = envStr("NEXUS_BENCH_FAIR_SHARE", "true") != "false"
		st := store.NewSchedulerStore(p, cfg, fmt.Sprintf("bench-%d", r))
		// NEXUS_BENCH_WINDOW=false measures the exhaustive pull scan that
		// ADR-0018 Stage 1 replaced, so before/after come from one run on one
		// machine. Asking for the window and not getting it is a failure, not
		// a silent fallback: a benchmark that quietly measured the old path
		// would report the fix as a no-op (F89).
		if envStr("NEXUS_BENCH_WINDOW", "true") != "false" {
			ws, err := st.EnableWindowedClaim(ctx)
			if err != nil || !ws.Enabled || !ws.Indexed {
				t.Fatalf("windowed claim requested but not active: %+v %v (is migration 0023 applied?)", ws, err)
			}
		}
		stores[r] = st
	}
	deadline := time.Now().Add(limit)
	start := time.Now()
	for r := 0; r < replicas; r++ {
		wg.Add(1)
		go func(r int) {
			defer wg.Done()
			st := stores[r]
			var local []time.Duration
			// Each replica serves a disjoint slice of the workers, the way
			// round_robin (F81) spreads polls across replicas.
			var mine []uuid.UUID
			for i, wid := range w.workers {
				if i%replicas == r {
					mine = append(mine, wid)
				}
			}
			if len(mine) == 0 {
				mine = w.workers
			}
			for i := 0; done.Load() < int64(target) && time.Now().Before(deadline); i++ {
				t0 := time.Now()
				var ok bool
				var err error
				switch mode {
				case "pull":
					var asg []store.WorkerAssignment
					asg, err = st.ClaimForWorker(ctx, time.Now().UTC(), store.WorkerClaimRequest{
						WorkerID: mine[i%len(mine)], MaxTasks: 1})
					ok = len(asg) == 1
				default:
					var rep store.ClaimReport
					rep, err = st.Claim(ctx, time.Now().UTC())
					ok = rep.Outcome == store.ClaimDispatched
				}
				d := time.Since(t0)
				switch {
				case err != nil:
					errs.Add(1)
				case ok:
					done.Add(1)
					local = append(local, d)
				default:
					misses.Add(1)
				}
			}
			mu.Lock()
			lat = append(lat, local...)
			mu.Unlock()
		}(r)
	}
	wg.Wait()
	elapsed := time.Since(start)
	sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
	c := cell{mode: mode, replicas: replicas,
		claims: done.Load(), misses: misses.Load(), errors: errs.Load(), elapsed: elapsed,
		p50: pct(lat, 0.50), p99: pct(lat, 0.99)}
	if len(lat) > 0 {
		c.max = lat[len(lat)-1]
	}
	return c
}

// TestDispatchCeiling is the benchmark. Knobs (all optional):
//
//	NEXUS_BENCH_MODES     pull,push
//	NEXUS_BENCH_BACKLOGS  1000,10000,100000
//	NEXUS_BENCH_REPLICAS  1,2,4,8,16
//	NEXUS_BENCH_WORKERS   64
//	NEXUS_BENCH_CLAIMS    3000   dispatches measured per cell (capped at backlog/2
//	                             so the backlog stays roughly the size named)
//	NEXUS_BENCH_WINDOW    true   false measures the pre-0023 exhaustive pull scan
//	NEXUS_BENCH_QUOTA     false  true gives the namespace (unreached) quotas (ADR-0021)
//	NEXUS_BENCH_OUT       path to also write the markdown table to
func TestDispatchCeiling(t *testing.T) {
	dsn := os.Getenv("NEXUS_BENCH_DSN")
	if dsn == "" {
		t.Fatal("NEXUS_BENCH_DSN is unset. Point it at a DISPOSABLE migrated database (make bench-pg); " +
			"the benchmark writes hundreds of thousands of rows.")
	}
	ctx := context.Background()
	admin, err := store.NewPool(ctx, store.DefaultPoolConfig(dsn))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer admin.Close()

	var pgVersion string
	_ = admin.QueryRow(ctx, `SHOW server_version`).Scan(&pgVersion)
	nWorkers := envInts("NEXUS_BENCH_WORKERS", "64")[0]
	perCell := envInts("NEXUS_BENCH_CLAIMS", "3000")[0]

	var b strings.Builder
	fmt.Fprintf(&b, "Postgres %s · client GOMAXPROCS=%d · %d workers · %s\n\n",
		pgVersion, runtime.GOMAXPROCS(0), nWorkers, time.Now().UTC().Format(time.RFC3339))
	fmt.Fprintf(&b, "| mode | backlog | replicas | dispatched | claims/s | p50 | p99 | max | empty | errors |\n")
	fmt.Fprintf(&b, "|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|\n")

	for _, mode := range strings.Split(envStr("NEXUS_BENCH_MODES", "pull,push"), ",") {
		for _, backlog := range envInts("NEXUS_BENCH_BACKLOGS", "1000,10000,100000") {
			for _, replicas := range envInts("NEXUS_BENCH_REPLICAS", "1,2,4,8,16") {
				target := perCell
				if target > backlog/2 {
					target = backlog / 2
				}
				w := seed(ctx, t, admin, backlog, nWorkers)
				c := run(ctx, t, dsn, mode, w, replicas, target, 90*time.Second)
				w.cleanup(ctx, admin)
				rate := float64(c.claims) / c.elapsed.Seconds()
				line := fmt.Sprintf("| %s | %d | %d | %d | %.0f | %s | %s | %s | %d | %d |\n",
					mode, backlog, replicas, c.claims, rate,
					c.p50.Round(10*time.Microsecond), c.p99.Round(10*time.Microsecond),
					c.max.Round(100*time.Microsecond), c.misses, c.errors)
				b.WriteString(line)
				t.Log(strings.TrimSpace(line))
			}
		}
	}
	fmt.Println(b.String())
	if out := os.Getenv("NEXUS_BENCH_OUT"); out != "" {
		if err := os.WriteFile(out, []byte(b.String()), 0o644); err != nil {
			t.Fatalf("write %s: %v", out, err)
		}
	}
}
