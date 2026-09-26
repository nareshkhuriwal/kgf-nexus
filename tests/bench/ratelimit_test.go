//go:build bench

// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package bench

import (
	"context"
	"fmt"
	"math"
	"os"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/khuriwalgroup/kgf-nexus/internal/store"
)

// The shared rate limiter's cost (ADR-0032 §3, OPERATIONS §29.6).
//
// Three questions, each a table:
//
//  1. Throughput and latency of AcquirePermits when every request is granted
//     (a limit far above the offered load), for one hot key and for load
//     spread over 16 keys, at 1-16 scheduler replicas with 8 callers each.
//     This is the permit service's own ceiling.
//  2. Correctness under saturation: a 100 req/s limit hammered by the same
//     callers must never grant more than burst + rate x elapsed.
//  3. The dispatch ceiling with and without concurrent permit traffic: the
//     permit service adds nothing to the claim transaction, so claim latency
//     should move only by shared database load (ADR-0018).
//
// Knobs: NEXUS_BENCH_PERMIT_REPLICAS (1,4,16), NEXUS_BENCH_PERMIT_CALLERS (8),
// NEXUS_BENCH_PERMIT_SECONDS (5), NEXUS_BENCH_OUT.

type permitWorld struct {
	ns       string
	attempts []uuid.UUID
	tokens   []string
}

func seedPermitWorld(ctx context.Context, t *testing.T, pool *pgxpool.Pool, callers int, limits string) *permitWorld {
	t.Helper()
	id := uuid.NewString()[:8]
	w := &permitWorld{ns: "permit-" + id}
	must := func(err error, what string) {
		if err != nil {
			t.Fatalf("seed %s: %v", what, err)
		}
	}
	_, err := pool.Exec(ctx, `INSERT INTO tenants(namespace,name) VALUES ($1,$1)`, w.ns)
	must(err, "tenant")
	_, err = pool.Exec(ctx, `INSERT INTO platform_config (namespace, revision, config)
VALUES ($1, 1, jsonb_build_object('quotas', jsonb_build_object('rate_limits', $2::jsonb)))`, w.ns, limits)
	must(err, "limits")
	var specID, runID, workerID uuid.UUID
	must(pool.QueryRow(ctx, `INSERT INTO workflow_specs (namespace,name,version,spec_hash,spec_json,created_by)
VALUES ($1,'permits',1,$2,'{}','bench') RETURNING id`, w.ns, "h-"+id).Scan(&specID), "spec")
	must(pool.QueryRow(ctx, `INSERT INTO workflow_runs (spec_id,namespace,state,trigger_kind)
VALUES ($1,$2,'RUNNING','bench') RETURNING id`, specID, w.ns).Scan(&runID), "run")
	must(pool.QueryRow(ctx, `
INSERT INTO workers (namespace,hostname,queues,capacity,committed,state,agent_version,session_token_hash,last_heartbeat_at)
VALUES ($1,'permit-bench',ARRAY['permits'],'{}','{}','ACTIVE','bench','h',now()+interval '1 hour') RETURNING id`,
		w.ns).Scan(&workerID), "worker")
	for i := 0; i < callers; i++ {
		node := fmt.Sprintf("n%d", i)
		_, err = pool.Exec(ctx, `INSERT INTO task_nodes (run_id,node_id,task_ref,state,max_attempts,spec_json)
VALUES ($1,$2,'bench.llm','RUNNING',3,'{}')`, runID, node)
		must(err, "node")
		var aid uuid.UUID
		must(pool.QueryRow(ctx, `
INSERT INTO task_attempts (run_id,node_id,attempt_number,state,queue,priority,resource_request,idempotency_key,
                           scheduled_at,dispatched_at,started_at,worker_id)
VALUES ($1,$2,1,'RUNNING','permits',100,'{}',$3,now(),now(),now(),$4) RETURNING id`,
			runID, node, w.ns+"-"+node, workerID).Scan(&aid), "attempt")
		tok := "tok-" + uuid.NewString()
		_, err = pool.Exec(ctx, `INSERT INTO leases (attempt_id, worker_id, token, expires_at, epoch)
VALUES ($1,$2,$3,now()+interval '1 hour',1)`, aid, workerID, tok)
		must(err, "lease")
		w.attempts = append(w.attempts, aid)
		w.tokens = append(w.tokens, tok)
	}
	return w
}

func (w *permitWorld) cleanup(ctx context.Context, pool *pgxpool.Pool) {
	_, _ = pool.Exec(ctx, `DELETE FROM workflow_runs WHERE namespace=$1`, w.ns)
	_, _ = pool.Exec(ctx, `DELETE FROM workflow_specs WHERE namespace=$1`, w.ns)
	_, _ = pool.Exec(ctx, `DELETE FROM workers WHERE namespace=$1`, w.ns)
	_, _ = pool.Exec(ctx, `DELETE FROM rate_limit_buckets WHERE tenant_id = (SELECT id FROM tenants WHERE namespace=$1)`, w.ns)
	_, _ = pool.Exec(ctx, `DELETE FROM platform_config WHERE namespace=$1`, w.ns)
}

type permitCell struct {
	granted, denied, errors int64
	elapsed                 time.Duration
	p50, p99                time.Duration
}

// drivePermits runs replicas x callers goroutines against keysFor(caller) until
// the deadline. Denied callers sleep the suggested retry_after, as the SDK does.
func drivePermits(ctx context.Context, t *testing.T, dsn string, w *permitWorld, replicas, callers int,
	d time.Duration, keysFor func(i int) []string, stop *atomic.Bool) permitCell {
	t.Helper()
	var (
		granted, denied, errs atomic.Int64
		mu                    sync.Mutex
		lat                   []time.Duration
		wg                    sync.WaitGroup
	)
	start := time.Now()
	for r := 0; r < replicas; r++ {
		pc := store.DefaultPoolConfig(dsn)
		pc.MaxConns, pc.MinConns = int32(callers), 1
		p, err := store.NewPool(ctx, pc)
		if err != nil {
			t.Fatalf("pool: %v", err)
		}
		defer p.Close()
		st := store.NewSchedulerStore(p, store.DefaultClaimConfig("permits"), fmt.Sprintf("permit-%d", r))
		for c := 0; c < callers; c++ {
			i := (r*callers + c) % len(w.attempts)
			wg.Add(1)
			go func() {
				defer wg.Done()
				var local []time.Duration
				for time.Since(start) < d && (stop == nil || !stop.Load()) {
					t0 := time.Now()
					out, err := st.AcquirePermits(ctx, store.PermitRequest{
						AttemptID: w.attempts[i], Token: w.tokens[i], Keys: keysFor(i), Requests: 1, Tokens: 100,
					})
					local = append(local, time.Since(t0))
					switch {
					case err != nil:
						errs.Add(1)
					case out.StaleLease:
						errs.Add(1)
					case out.Granted:
						granted.Add(1)
					default:
						denied.Add(1)
						time.Sleep(out.RetryAfter)
					}
				}
				mu.Lock()
				lat = append(lat, local...)
				mu.Unlock()
			}()
		}
	}
	wg.Wait()
	sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
	return permitCell{granted: granted.Load(), denied: denied.Load(), errors: errs.Load(),
		elapsed: time.Since(start), p50: pct(lat, 0.5), p99: pct(lat, 0.99)}
}

func TestRateLimiterCeiling(t *testing.T) {
	dsn := os.Getenv("NEXUS_BENCH_DSN")
	if dsn == "" {
		t.Fatal("NEXUS_BENCH_DSN is unset. Point it at a DISPOSABLE migrated database (make bench-pg).")
	}
	ctx := context.Background()
	admin, err := store.NewPool(ctx, store.DefaultPoolConfig(dsn))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer admin.Close()
	var pgVersion string
	_ = admin.QueryRow(ctx, `SHOW server_version`).Scan(&pgVersion)
	callers := envInts("NEXUS_BENCH_PERMIT_CALLERS", "8")[0]
	secs := time.Duration(envInts("NEXUS_BENCH_PERMIT_SECONDS", "5")[0]) * time.Second

	var b strings.Builder
	fmt.Fprintf(&b, "Postgres %s · client GOMAXPROCS=%d · %d callers per replica · %s per cell · %s\n\n",
		pgVersion, runtime.GOMAXPROCS(0), callers, secs, time.Now().UTC().Format(time.RFC3339))

	// ── 1. Throughput, all granted ──────────────────────────────────────────
	fmt.Fprintf(&b, "**Permit throughput (every request granted)**\n\n| keys | replicas | permits/s | p50 | p99 | errors |\n|---|---:|---:|---:|---:|---:|\n")
	unlimitedish := `{"hot": {"requests_per_second": 1000000, "tokens_per_minute": 1000000000000}` +
		func() string {
			var s strings.Builder
			for k := 0; k < 16; k++ {
				fmt.Fprintf(&s, `, "spread/k%d": {"requests_per_second": 1000000, "tokens_per_minute": 1000000000000}`, k)
			}
			return s.String()
		}() + "}"
	for _, spread := range []bool{false, true} {
		for _, replicas := range envInts("NEXUS_BENCH_PERMIT_REPLICAS", "1,4,16") {
			w := seedPermitWorld(ctx, t, admin, replicas*callers, unlimitedish)
			keysFor := func(int) []string { return []string{"hot"} }
			label := "1 hot"
			if spread {
				keysFor = func(i int) []string { return []string{fmt.Sprintf("spread/k%d", i%16)} }
				label = "16 spread"
			}
			c := drivePermits(ctx, t, dsn, w, replicas, callers, secs, keysFor, nil)
			w.cleanup(ctx, admin)
			line := fmt.Sprintf("| %s | %d | %.0f | %s | %s | %d |\n", label, replicas,
				float64(c.granted)/c.elapsed.Seconds(), c.p50.Round(10*time.Microsecond),
				c.p99.Round(10*time.Microsecond), c.errors)
			b.WriteString(line)
			t.Log(strings.TrimSpace(line))
			if c.errors > 0 {
				t.Errorf("%d permit errors", c.errors)
			}
		}
	}

	// ── 2. Correctness under saturation ─────────────────────────────────────
	fmt.Fprintf(&b, "\n**Saturated limit (100 req/s, burst 100): granted vs. the ceiling burst + rate x elapsed**\n\n| replicas | granted | denied | ceiling | granted/s |\n|---|---:|---:|---:|---:|\n")
	for _, replicas := range envInts("NEXUS_BENCH_PERMIT_REPLICAS", "1,4,16") {
		w := seedPermitWorld(ctx, t, admin, replicas*callers, `{"hot": {"requests_per_second": 100}}`)
		c := drivePermits(ctx, t, dsn, w, replicas, callers, secs, func(int) []string { return []string{"hot"} }, nil)
		w.cleanup(ctx, admin)
		ceiling := 100 + 100*c.elapsed.Seconds()
		line := fmt.Sprintf("| %d | %d | %d | %.0f | %.1f |\n", replicas, c.granted, c.denied, ceiling,
			float64(c.granted)/c.elapsed.Seconds())
		b.WriteString(line)
		t.Log(strings.TrimSpace(line))
		if float64(c.granted) > math.Ceil(ceiling) {
			t.Errorf("granted %d > ceiling %.0f: the limit was exceeded", c.granted, ceiling)
		}
	}

	// ── 3. Dispatch with and without permit traffic ─────────────────────────
	fmt.Fprintf(&b, "\n**Pull claims, backlog 10,000, 4 replicas: alone vs. with 4x8 permit callers on one hot key**\n\n| permit load | claims/s | p50 | p99 | permits/s alongside |\n|---|---:|---:|---:|---:|\n")
	for _, withLoad := range []bool{false, true} {
		wq := seed(ctx, t, admin, 10000, 64)
		var (
			stop   atomic.Bool
			pc     permitCell
			pw     *permitWorld
			doneCh = make(chan struct{})
		)
		if withLoad {
			pw = seedPermitWorld(ctx, t, admin, 4*callers, unlimitedish)
			go func() {
				defer close(doneCh)
				pc = drivePermits(ctx, t, dsn, pw, 4, callers, 10*time.Minute, func(int) []string { return []string{"hot"} }, &stop)
			}()
		} else {
			close(doneCh)
		}
		c := run(ctx, t, dsn, "pull", wq, 4, 2000, 90*time.Second)
		stop.Store(true)
		<-doneCh
		wq.cleanup(ctx, admin)
		alongside := "-"
		label := "none"
		if withLoad {
			pw.cleanup(ctx, admin)
			alongside = fmt.Sprintf("%.0f", float64(pc.granted)/pc.elapsed.Seconds())
			label = "4x8 callers"
		}
		line := fmt.Sprintf("| %s | %.0f | %s | %s | %s |\n", label, float64(c.claims)/c.elapsed.Seconds(),
			c.p50.Round(10*time.Microsecond), c.p99.Round(10*time.Microsecond), alongside)
		b.WriteString(line)
		t.Log(strings.TrimSpace(line))
	}

	fmt.Println(b.String())
	if out := os.Getenv("NEXUS_BENCH_OUT"); out != "" {
		if err := os.WriteFile(out, []byte(b.String()), 0o644); err != nil {
			t.Fatalf("write %s: %v", out, err)
		}
	}
}
