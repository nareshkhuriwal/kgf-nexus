//go:build integration

// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package relay

// Relay tests against a REAL Postgres (migrations through 0016) and a REAL NATS
// JetStream server. Run them through tests/nats/run-integration.sh, which starts
// both in throwaway containers (nexus-nats-agent-*, ports 55511/55512) and fails
// if anything here skipped.
//
// Every test asserts on the STREAM, not on the relay's own counters: the claim
// under test is "this message is in JetStream exactly once", and the relay
// reporting success is not evidence of that (F89). Each test also carries a
// control showing the path it asserts a negative about really works.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
	"github.com/khuriwalgroup/kgf-nexus/internal/bus"
	"github.com/khuriwalgroup/kgf-nexus/internal/domain"
	"github.com/khuriwalgroup/kgf-nexus/internal/metrics"
	"github.com/khuriwalgroup/kgf-nexus/internal/store"
	eventservice "github.com/khuriwalgroup/kgf-nexus/services/event-service"
)

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func testPG(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("NEXUS_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("skipping integration test: NEXUS_TEST_DATABASE_URL is not set (use tests/nats/run-integration.sh)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err == nil {
		err = pool.Ping(ctx)
	}
	if err != nil {
		t.Skipf("skipping integration test: test database unreachable: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func testNATS(t *testing.T) *nats.Conn {
	t.Helper()
	url := os.Getenv("NEXUS_TEST_NATS_URL")
	if url == "" {
		t.Skip("skipping integration test: NEXUS_TEST_NATS_URL is not set (use tests/nats/run-integration.sh)")
	}
	nc, err := bus.Connect(bus.Config{URL: url, Name: "relay-it-" + t.Name()}, quietLog())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(nc.Close)
	waitConnected(t, nc, 10*time.Second)
	return nc
}

func waitConnected(t *testing.T, nc *nats.Conn, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for nc.Status() != nats.CONNECTED {
		if time.Now().After(deadline) {
			t.Skipf("skipping integration test: NATS not reachable (status %v)", nc.Status())
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// world is one tenant with one RUNNING run, isolated by a unique namespace so its
// subjects are its own even though the relay publishes every row it can see.
type world struct {
	t     *testing.T
	ctx   context.Context
	pool  *pgxpool.Pool
	ns    string
	queue string
	runID string
}

func newWorld(t *testing.T, pool *pgxpool.Pool) *world {
	t.Helper()
	ctx := context.Background()
	id := uuid.NewString()[:8]
	w := &world{t: t, ctx: ctx, pool: pool, ns: "relay-" + id, queue: "q-" + id}
	if _, err := pool.Exec(ctx, `INSERT INTO tenants (namespace, name) VALUES ($1,$1)`, w.ns); err != nil {
		t.Fatalf("tenant: %v", err)
	}
	var specID string
	if err := pool.QueryRow(ctx, `
INSERT INTO workflow_specs (namespace, name, version, spec_hash, spec_json, created_by)
VALUES ($1,'wf',1,$2,'{}','test') RETURNING id::text`, w.ns, "h-"+id).Scan(&specID); err != nil {
		t.Fatalf("spec: %v", err)
	}
	if err := pool.QueryRow(ctx, `
INSERT INTO workflow_runs (spec_id, namespace, state, trigger_kind)
VALUES ($1,$2,'RUNNING','test') RETURNING id::text`, specID, w.ns).Scan(&w.runID); err != nil {
		t.Fatalf("run: %v", err)
	}
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = pool.Exec(c, `DELETE FROM outbox WHERE namespace = $1 OR routing_key = $2
		   OR ref_id IN (SELECT id FROM run_events WHERE run_id = $2::uuid)`, w.ns, w.runID)
		_, _ = pool.Exec(c, `DELETE FROM timers WHERE subject_id IN (SELECT id FROM task_attempts WHERE run_id = $1::uuid)`, w.runID)
		_, _ = pool.Exec(c, `DELETE FROM workflow_runs WHERE id = $1::uuid`, w.runID)
		_, _ = pool.Exec(c, `DELETE FROM workflow_specs WHERE id = $1::uuid`, specID)
		_, _ = pool.Exec(c, `DELETE FROM workers WHERE namespace = $1`, w.ns)
		_, _ = pool.Exec(c, `DELETE FROM tenants WHERE namespace = $1`, w.ns)
	})
	return w
}

// appendEvents writes n run events through the event-service's own Append, the
// production write path, and returns their outbox row ids.
func (w *world) appendEvents(n int) []int64 {
	w.t.Helper()
	es := eventservice.NewStore(w.pool)
	for i := 0; i < n; i++ {
		if _, err := es.Append(w.ctx, eventservice.Event{
			RunID: w.runID, Kind: "TestEvent", Actor: "relay-it", Payload: map[string]any{"i": i},
		}); err != nil {
			w.t.Fatalf("append event %d: %v", i, err)
		}
	}
	return w.outboxIDs()
}

func (w *world) outboxIDs() []int64 {
	w.t.Helper()
	rows, err := w.pool.Query(w.ctx, `
SELECT o.id FROM outbox o JOIN run_events e ON e.id = o.ref_id
 WHERE o.topic = 'run_event' AND e.run_id = $1::uuid ORDER BY o.id`, w.runID)
	if err != nil {
		w.t.Fatal(err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		_ = rows.Scan(&id)
		ids = append(ids, id)
	}
	return ids
}

func (w *world) unpublished() int {
	w.t.Helper()
	var n int
	if err := w.pool.QueryRow(w.ctx, `
SELECT count(*)::int FROM outbox o JOIN run_events e ON e.id = o.ref_id
 WHERE e.run_id = $1::uuid AND o.published_at IS NULL`, w.runID).Scan(&n); err != nil {
		w.t.Fatal(err)
	}
	return n
}

func fastConfig() Config {
	c := DefaultConfig()
	c.BatchSize = 64
	c.PollInterval = 20 * time.Millisecond
	c.PublishTimeout = 3 * time.Second
	c.MinBackoff = 50 * time.Millisecond
	c.MaxBackoff = 500 * time.Millisecond
	c.HintInterval = 0
	c.BacklogInterval = 100 * time.Millisecond
	return c
}

func newRelay(t *testing.T, pool *pgxpool.Pool, nc *nats.Conn) (*Relay, *Metrics) {
	t.Helper()
	m := NewMetrics(metrics.NewRegistry())
	r, err := New(pool, nc, fastConfig(), quietLog(), m)
	if err != nil {
		t.Fatal(err)
	}
	return r, m
}

// drain runs batches until this world's rows are all published.
func drain(t *testing.T, r *Relay, w *world) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for w.unpublished() > 0 {
		if time.Now().After(deadline) {
			t.Fatalf("%d rows still unpublished", w.unpublished())
		}
		if _, err := r.PublishBatch(w.ctx); err != nil {
			t.Logf("batch: %v", err)
			time.Sleep(50 * time.Millisecond)
		}
	}
}

// streamBodies reads every message on subject from the stream, in stream order.
func streamBodies(t *testing.T, nc *nats.Conn, stream, subject string) []map[string]any {
	t.Helper()
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s, err := js.Stream(ctx, stream)
	if err != nil {
		t.Fatalf("stream %s: %v", stream, err)
	}
	info, err := s.Info(ctx, jetstream.WithSubjectFilter(subject))
	if err != nil {
		t.Fatalf("stream info: %v", err)
	}
	var want uint64
	for _, n := range info.State.Subjects {
		want += n
	}
	if want == 0 {
		return nil
	}
	cons, err := s.OrderedConsumer(ctx, jetstream.OrderedConsumerConfig{FilterSubjects: []string{subject}})
	if err != nil {
		t.Fatalf("consumer: %v", err)
	}
	var out []map[string]any
	for uint64(len(out)) < want {
		batch, err := cons.Fetch(int(want)-len(out), jetstream.FetchMaxWait(2*time.Second))
		if err != nil {
			t.Fatalf("fetch: %v", err)
		}
		got := 0
		for m := range batch.Messages() {
			var b map[string]any
			if err := json.Unmarshal(m.Data(), &b); err != nil {
				t.Fatalf("bad message body %q: %v", m.Data(), err)
			}
			out = append(out, b)
			got++
		}
		if got == 0 {
			t.Fatalf("stream reports %d messages on %s but only %d could be read", want, subject, len(out))
		}
	}
	return out
}

func runEventSubject(w *world) string { return bus.RunEventSubject(w.ns, w.runID) }

// assertExactlyOnce checks the stream holds each of the run's events exactly once:
// a count, and the seq set, so a duplicate cannot hide behind a missing message.
func assertExactlyOnce(t *testing.T, nc *nats.Conn, w *world, n int) {
	t.Helper()
	bodies := streamBodies(t, nc, bus.StreamRuns, runEventSubject(w))
	if len(bodies) != n {
		t.Fatalf("stream holds %d messages for the run, want exactly %d", len(bodies), n)
	}
	seen := map[int]bool{}
	for _, b := range bodies {
		seq := int(b["seq"].(float64))
		if seen[seq] {
			t.Errorf("seq %d is in the stream twice", seq)
		}
		seen[seq] = true
	}
	for s := 1; s <= n; s++ {
		if !seen[s] {
			t.Errorf("seq %d never reached the stream", s)
		}
	}
}

// ── outbox atomicity ──────────────────────────────────────────────────────────

// TestNATSRolledBackFactIsNeverPublished: a run transition that rolls back must
// produce no message. The committed transition in the same test is the control:
// it proves the relay does publish this topic for this run, so the absence of the
// rolled-back one means something.
func TestNATSRolledBackFactIsNeverPublished(t *testing.T) {
	pool, nc := testPG(t), testNATS(t)
	w := newWorld(t, pool)
	r, _ := newRelay(t, pool, nc)

	tx, err := pool.Begin(w.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.WFMTransitionRun(w.ctx, tx, w.runID, []string{"RUNNING"}, "CANCELLING"); err != nil {
		t.Fatalf("transition in tx: %v", err)
	}
	_ = tx.Rollback(w.ctx)

	if _, err := store.WFMTransitionRun(w.ctx, pool, w.runID, []string{"RUNNING"}, "PAUSING"); err != nil {
		t.Fatalf("committed transition: %v", err)
	}
	for i := 0; i < 5; i++ {
		if _, err := r.PublishBatch(w.ctx); err != nil {
			t.Fatalf("batch: %v", err)
		}
	}
	bodies := streamBodies(t, nc, bus.StreamRuns, bus.RunStateSubject(w.ns, w.runID))
	if len(bodies) != 1 {
		t.Fatalf("stream holds %d state messages, want exactly 1 (the committed one): %v", len(bodies), bodies)
	}
	if bodies[0]["state"] != "PAUSING" {
		t.Errorf("published state = %v; the rolled-back CANCELLING must never appear", bodies[0]["state"])
	}
}

// ── de-duplication ────────────────────────────────────────────────────────────

// TestNATSDuplicatePublishIsDeduplicated: re-publishing an already-published row
// (as any retry does) is acknowledged as a duplicate and does not add a message.
func TestNATSDuplicatePublishIsDeduplicated(t *testing.T) {
	pool, nc := testPG(t), testNATS(t)
	w := newWorld(t, pool)
	r, m := newRelay(t, pool, nc)

	ids := w.appendEvents(1)
	drain(t, r, w)
	assertExactlyOnce(t, nc, w, 1) // control: the first publish landed

	if _, err := pool.Exec(w.ctx, `UPDATE outbox SET published_at = NULL WHERE id = $1`, ids[0]); err != nil {
		t.Fatal(err)
	}
	before := m.Duplicates.Value()
	drain(t, r, w)
	if got := m.Duplicates.Value() - before; got != 1 {
		t.Errorf("duplicate acks = %v, want 1 (the re-publish must carry the same Nats-Msg-Id)", got)
	}
	assertExactlyOnce(t, nc, w, 1)
}

// TestNATSRelayCrashBetweenPublishAndMark: the broker has the messages but the
// relay dies before marking the rows. The rows must still be unpublished (the
// transaction never committed), the next relay must send them again, and the
// stream must still hold each exactly once.
func TestNATSRelayCrashBetweenPublishAndMark(t *testing.T) {
	pool, nc := testPG(t), testNATS(t)
	w := newWorld(t, pool)
	mine := map[int64]bool{}
	for _, id := range w.appendEvents(5) {
		mine[id] = true
	}

	crashing, _ := newRelay(t, pool, nc)
	crashed := 0
	crashing.beforeMark = func(ids []int64) error {
		for _, id := range ids {
			if mine[id] {
				crashed++
			}
		}
		return errors.New("simulated crash after ack, before mark")
	}
	for i := 0; i < 5 && crashed < 5; i++ {
		_, _ = crashing.PublishBatch(w.ctx)
	}
	if crashed != 5 {
		t.Fatalf("control failed: the crashing relay acked %d of 5 rows", crashed)
	}
	if n := w.unpublished(); n != 5 {
		t.Fatalf("%d rows unpublished after the crash, want 5 (a mark survived an uncommitted tx)", n)
	}
	// The broker really has them already -- this is the dangerous state.
	assertExactlyOnce(t, nc, w, 5)

	recovering, m := newRelay(t, pool, nc)
	drain(t, recovering, w)
	if d := m.Duplicates.Value(); d < 5 {
		t.Errorf("the recovering relay saw %v duplicate acks, want >= 5", d)
	}
	assertExactlyOnce(t, nc, w, 5)
}

// ── broker outage ─────────────────────────────────────────────────────────────

func docker(t *testing.T, args ...string) {
	t.Helper()
	out, err := exec.Command("docker", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker %v: %v\n%s", args, err, out)
	}
}

// TestNATSBrokerOutageBacklogDrainsWithoutLoss stops the real broker, writes
// facts while it is down, and starts it again. Everything written during the
// outage must reach the stream exactly once, with no manual intervention, and the
// write path must never have noticed the broker was gone.
func TestNATSBrokerOutageBacklogDrainsWithoutLoss(t *testing.T) {
	container := os.Getenv("NEXUS_TEST_NATS_CONTAINER")
	if container == "" {
		t.Skip("skipping integration test: NEXUS_TEST_NATS_CONTAINER is not set, so the broker cannot be stopped")
	}
	pool, nc := testPG(t), testNATS(t)
	w := newWorld(t, pool)
	r, m := newRelay(t, pool, nc)

	w.appendEvents(1)
	drain(t, r, w)
	assertExactlyOnce(t, nc, w, 1)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()

	docker(t, "stop", "-t", "1", container)
	restarted := false
	defer func() {
		if !restarted {
			docker(t, "start", container)
		}
	}()
	deadline := time.Now().Add(10 * time.Second)
	for nc.Status() == nats.CONNECTED {
		if time.Now().After(deadline) {
			t.Fatal("client still reports CONNECTED 10s after the broker stopped")
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Writes during the outage: the fact path must not care.
	w.appendEvents(40)
	time.Sleep(1500 * time.Millisecond) // several relay backoff cycles
	if n := w.unpublished(); n != 40 {
		t.Fatalf("%d rows unpublished during the outage, want 40", n)
	}
	if _, err := r.PublishBatch(w.ctx); !errors.Is(err, ErrBrokerUnavailable) {
		t.Errorf("PublishBatch during the outage returned %v, want ErrBrokerUnavailable", err)
	}
	if err := r.SampleBacklog(w.ctx); err != nil {
		t.Fatal(err)
	}
	if m.Backlog.Value() < 40 {
		t.Errorf("backlog gauge = %v during the outage, want >= 40", m.Backlog.Value())
	}

	docker(t, "start", container)
	restarted = true
	deadline = time.Now().Add(60 * time.Second)
	for w.unpublished() > 0 {
		if time.Now().After(deadline) {
			t.Fatalf("backlog did not drain within 60s of the broker returning: %d left", w.unpublished())
		}
		time.Sleep(100 * time.Millisecond)
	}
	assertExactlyOnce(t, nc, w, 41)
	if m.Failures.With(metrics.Labels{"kind": "broker"}).Value() == 0 && m.Published.With(metrics.Labels{"topic": "run_event"}).Value() < 40 {
		t.Error("metrics recorded neither the outage nor the drain")
	}
}

// ── two relay replicas ───────────────────────────────────────────────────────

// TestNATSTwoRelaysNeverPublishTheSameRow runs two relays against one outbox.
//
// The first half is deterministic rather than a hoped-for race (F86): relay A
// claims a batch and is held AFTER publishing, with its row locks open, while
// relay B runs. B must neither block on A's rows nor take any of them. The second
// half drains the rest with both relays concurrently and checks, from the relays'
// own per-batch acked ids, that no row was published by both -- de-duplication in
// the stream would hide a double publish, so the stream count alone is not enough.
func TestNATSTwoRelaysNeverPublishTheSameRow(t *testing.T) {
	pool, nc := testPG(t), testNATS(t)
	w := newWorld(t, pool)
	mine := map[int64]bool{}
	for _, id := range w.appendEvents(200) {
		mine[id] = true
	}

	var mu sync.Mutex
	publishedBy := map[int64][]string{}
	record := func(name string, ids []int64) {
		mu.Lock()
		defer mu.Unlock()
		for _, id := range ids {
			if mine[id] {
				publishedBy[id] = append(publishedBy[id], name)
			}
		}
	}

	a, _ := newRelay(t, pool, nc)
	b, _ := newRelay(t, pool, nc)
	a.cfg.BatchSize, b.cfg.BatchSize = 50, 50

	held := make(chan []int64, 1)
	release := make(chan struct{})
	var once sync.Once
	a.beforeMark = func(ids []int64) error {
		record("A", ids)
		once.Do(func() { held <- ids; <-release })
		return nil
	}
	b.beforeMark = func(ids []int64) error { record("B", ids); return nil }

	// Released on every exit path: if B wrongly blocks and the test fails, A must
	// still let go of its row locks, or the cleanup DELETE waits on them forever
	// and a failing test turns into a hanging one.
	var releaseOnce sync.Once
	letGo := func() { releaseOnce.Do(func() { close(release) }) }
	defer letGo()

	aDone := make(chan error, 1)
	go func() { _, err := a.PublishBatch(w.ctx); aDone <- err }()
	aIDs := <-held

	bCtx, bCancel := context.WithTimeout(w.ctx, 10*time.Second)
	res, err := b.PublishBatch(bCtx)
	bCancel()
	if err != nil {
		t.Fatalf("relay B failed while A held its batch (it must skip A's rows, not wait): %v", err)
	}
	if res.Published == 0 {
		t.Fatal("relay B published nothing while A held 50 of 200 rows")
	}
	letGo()
	if err := <-aDone; err != nil {
		t.Fatalf("relay A: %v", err)
	}
	if len(aIDs) == 0 {
		t.Fatal("relay A held an empty batch; the interleaving under test did not happen")
	}

	// Both relays now drain the rest concurrently.
	var wg sync.WaitGroup
	for _, r := range []*Relay{a, b} {
		wg.Add(1)
		go func(r *Relay) {
			defer wg.Done()
			deadline := time.Now().Add(30 * time.Second)
			for w.unpublished() > 0 && time.Now().Before(deadline) {
				_, _ = r.PublishBatch(w.ctx)
			}
		}(r)
	}
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	for id := range mine {
		switch who := publishedBy[id]; len(who) {
		case 1:
		case 0:
			t.Errorf("outbox row %d was never published", id)
		default:
			t.Errorf("outbox row %d was published by %v", id, who)
		}
	}
	var byA, byB int
	for _, who := range publishedBy {
		if who[0] == "A" {
			byA++
		} else {
			byB++
		}
	}
	t.Logf("relay A published %d rows, relay B %d", byA, byB)
	assertExactlyOnce(t, nc, w, 200)
}

// ── end to end: dispatch hint ────────────────────────────────────────────────

// TestNATSAttemptCreationReachesAWorkerSubscription follows a hint the whole way:
// the DAG engine creates an attempt, the relay publishes the outbox row, and a
// plain core subscription on the subject an agent uses receives it.
func TestNATSAttemptCreationReachesAWorkerSubscription(t *testing.T) {
	pool, nc := testPG(t), testNATS(t)
	w := newWorld(t, pool)
	r, _ := newRelay(t, pool, nc)

	got := make(chan *nats.Msg, 4)
	sub, err := nc.ChanSubscribe(bus.DispatchSubject(w.ns, w.queue), got)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sub.Unsubscribe() }()
	_ = nc.Flush()

	if _, err := pool.Exec(w.ctx, `INSERT INTO task_nodes (run_id, node_id, task_ref, state, spec_json)
		VALUES ($1::uuid,'n1','t','PENDING','{}')`, w.runID); err != nil {
		t.Fatal(err)
	}
	res, err := store.NewDAGEngineStore(pool).ApplyNodeStateAndCreateAttempts(w.ctx, w.runID,
		[]store.NodeTransition{{NodeID: "n1", From: domain.NodePending, To: domain.NodeReady}},
		[]store.NewAttempt{{RunID: w.runID, NodeID: "n1", Queue: w.queue,
			Resources: &nexusv1.ResourceVector{CpuMillis: 1}, IdempotencyKey: "k-" + w.runID}})
	if err != nil || len(res.AttemptsCreated) != 1 {
		t.Fatalf("create attempt: %v %+v", err, res)
	}
	for i := 0; i < 5; i++ {
		if _, err := r.PublishBatch(w.ctx); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case m := <-got:
		var body map[string]any
		_ = json.Unmarshal(m.Data, &body)
		if body["attempt_id"] != res.AttemptsCreated[0].AttemptID || body["type"] != "task.ready" {
			t.Errorf("hint body = %s", m.Data)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no dispatch hint reached the worker subject")
	}

	// The reconciler re-announces the same queue while the attempt stays PENDING.
	n, err := r.PublishReadyHints(w.ctx)
	if err != nil || n == 0 {
		t.Fatalf("reconcile hints: n=%d err=%v", n, err)
	}
	select {
	case m := <-got:
		if !json.Valid(m.Data) {
			t.Error("reconcile hint is not JSON")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the reconcile hint for a queue with PENDING work never arrived")
	}
}

// TestNATSPruneBoundsTheTable: published rows past retention and unpublished rows
// past max age are deleted; fresh unpublished rows are not.
func TestNATSPruneBoundsTheTable(t *testing.T) {
	pool := testPG(t)
	w := newWorld(t, pool)
	ids := w.appendEvents(3)
	// ids[0]: published long ago. ids[1]: unpublished and ancient. ids[2]: fresh.
	if _, err := pool.Exec(w.ctx, `UPDATE outbox SET published_at = now() - interval '2 hours' WHERE id = $1`, ids[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(w.ctx, `UPDATE outbox SET created_at = now() - interval '2 days' WHERE id = $1`, ids[1]); err != nil {
		t.Fatal(err)
	}
	r, m := newRelay(t, pool, nil) // the bus-disabled relay must still prune
	if _, _, err := r.Prune(w.ctx); err != nil {
		t.Fatal(err)
	}
	left := w.outboxIDs()
	if len(left) != 1 || left[0] != ids[2] {
		t.Errorf("rows left after prune = %v, want only the fresh row %d", left, ids[2])
	}
	if m.Pruned.With(metrics.Labels{"reason": "expired"}).Value() < 1 ||
		m.Pruned.With(metrics.Labels{"reason": "published"}).Value() < 1 {
		t.Error("prune metrics not recorded")
	}
}
