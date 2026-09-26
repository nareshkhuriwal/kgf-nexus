//go:build integration

// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package notifier

// Notifier tests against a real Postgres (migrations through 0034) and local
// HTTP fakes for every receiver. No external calls are made.

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/khuriwalgroup/kgf-nexus/internal/notify"
	"github.com/khuriwalgroup/kgf-nexus/internal/secretbox"
	"github.com/khuriwalgroup/kgf-nexus/internal/store"
	"github.com/khuriwalgroup/kgf-nexus/internal/testdb"
	eventservice "github.com/khuriwalgroup/kgf-nexus/services/event-service"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func pg(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := testdb.DSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err == nil {
		err = pool.Ping(ctx)
	}
	if err != nil {
		testdb.SkipUnreachable(t, err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// receiver is a local HTTP fake that records every request.
type receiver struct {
	mu     sync.Mutex
	bodies []map[string]any
	hdrs   []http.Header
	status atomic.Int32
	srv    *httptest.Server
}

func newReceiver(t *testing.T) *receiver {
	r := &receiver{}
	r.status.Store(200)
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		b, _ := io.ReadAll(req.Body)
		m := map[string]any{}
		_ = json.Unmarshal(b, &m)
		r.mu.Lock()
		r.bodies = append(r.bodies, m)
		r.hdrs = append(r.hdrs, req.Header.Clone())
		r.mu.Unlock()
		w.WriteHeader(int(r.status.Load()))
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *receiver) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.bodies)
}

type fixture struct {
	t    *testing.T
	ctx  context.Context
	pool *pgxpool.Pool
	ns   string
	box  *secretbox.Box
	pd   *receiver
}

func newFixture(t *testing.T) *fixture {
	pool := pg(t)
	ns := "nt-" + uuid.NewString()[:8]
	testdb.RegisterTenant(t, pool, ns)
	box, _ := secretbox.New([]byte("0123456789abcdef0123456789abcdef"))
	return &fixture{t: t, ctx: context.Background(), pool: pool, ns: ns, box: box, pd: newReceiver(t)}
}

func (f *fixture) notifier() *Notifier { return f.notifierWithBackoff(time.Millisecond) }

func (f *fixture) notifierWithBackoff(base time.Duration) *Notifier {
	s := &notify.Sender{HTTP: notify.NewHTTPClient(true, 5*time.Second), PagerDutyURL: f.pd.srv.URL + "/v2/enqueue"}
	cfg := DefaultConfig()
	cfg.BaseBackoff = base
	cfg.ConsoleURL = "https://nexus.example"
	return New(f.pool, f.box, s, cfg, quiet(), nil)
}

// spec inserts a workflow spec with labels and optional task assets.
func (f *fixture) spec(name string, labels map[string]string, tasks []map[string]any) string {
	doc, _ := json.Marshal(map[string]any{"name": name, "namespace": f.ns, "labels": labels, "tasks": tasks})
	var id string
	if err := f.pool.QueryRow(f.ctx, `
INSERT INTO workflow_specs (namespace, name, version, spec_hash, spec_json, created_by)
VALUES ($1, $2, 1, $3, $4::jsonb, 'test') RETURNING id::text`, f.ns, name, uuid.NewString(), string(doc)).Scan(&id); err != nil {
		f.t.Fatal(err)
	}
	return id
}

func (f *fixture) run(specID string) string {
	var id string
	if err := f.pool.QueryRow(f.ctx, `INSERT INTO workflow_runs (spec_id, namespace, state, trigger_kind)
VALUES ($1, $2, 'PENDING', 'test') RETURNING id::text`, specID, f.ns).Scan(&id); err != nil {
		f.t.Fatal(err)
	}
	return id
}

func (f *fixture) transition(runID string, from []string, to string) {
	if _, err := store.WFMTransitionRun(f.ctx, f.pool, runID, from, to); err != nil {
		f.t.Fatalf("transition %s -> %s: %v", runID, to, err)
	}
}

func (f *fixture) channel(name, kind string, cfg map[string]any, secret string, rate int32) string {
	id := uuid.NewString()
	sec := store.ChannelSecretUpdate{}
	if secret != "" {
		ct, err := f.box.Seal([]byte(secret), secretbox.Context("notification_channels", f.ns, id))
		if err != nil {
			f.t.Fatal(err)
		}
		sec.Ciphertext = ct
	}
	if rate == 0 {
		rate = 100
	}
	if _, err := store.NotifyCreateChannel(f.ctx, f.pool, store.NotificationChannel{
		ID: id, Namespace: f.ns, Name: name, Kind: kind, Config: cfg, RateLimitPerMinute: rate, Enabled: true, CreatedBy: "test",
	}, sec); err != nil {
		f.t.Fatal(err)
	}
	return id
}

func (f *fixture) rule(name string, events, workflows []string, labels map[string]string, sla *int32, channels ...string) {
	tx, err := f.pool.Begin(f.ctx)
	if err != nil {
		f.t.Fatal(err)
	}
	defer tx.Rollback(f.ctx)
	if _, err := store.NotifyUpsertRule(f.ctx, tx, store.NotificationRule{
		Namespace: f.ns, Name: name, Events: events, Workflows: workflows, Labels: labels, SLASeconds: sla,
		Enabled: true, ChannelIDs: channels, CreatedBy: "test",
	}, true, 0); err != nil {
		f.t.Fatal(err)
	}
	if err := tx.Commit(f.ctx); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) drain(n *Notifier) {
	f.t.Helper()
	for i := 0; i < 50; i++ {
		c, err := n.EvaluateBatch(f.ctx)
		if err != nil {
			f.t.Fatal(err)
		}
		if c == 0 {
			break
		}
	}
	for i := 0; i < 50; i++ {
		c, err := n.DispatchOnce(f.ctx)
		if err != nil {
			f.t.Fatal(err)
		}
		if c == 0 {
			break
		}
	}
}

func (f *fixture) deliveries(state string) []store.NotificationDelivery {
	ds, err := store.NotifyListDeliveries(f.ctx, f.pool, f.ns, store.DeliveryFilter{State: state, Limit: 500})
	if err != nil {
		f.t.Fatal(err)
	}
	return ds
}

func TestFailureNotifiesMatchingRulesOnceWithSignedWebhook(t *testing.T) {
	f := newFixture(t)
	hook := newReceiver(t)
	webhook := f.channel("hook", notify.KindWebhook, map[string]any{"url": hook.srv.URL}, "signing-secret", 0)
	f.rule("etl-failures", []string{notify.EventRunFailed}, []string{"etl"}, map[string]string{"team": "data"}, nil, webhook)
	// A second rule on the same channel matches the same event: still ONE delivery.
	f.rule("everything", []string{notify.EventRunFailed, notify.EventRunSucceeded}, nil, nil, nil, webhook)

	etl := f.spec("etl", map[string]string{"team": "data", "tier": "1"}, nil)
	other := f.spec("other", map[string]string{"team": "web"}, nil)
	r1 := f.run(etl)
	f.transition(r1, []string{"PENDING"}, "RUNNING")
	f.transition(r1, []string{"RUNNING"}, "FAILED")
	r2 := f.run(other)
	f.transition(r2, []string{"PENDING"}, "RUNNING")
	f.transition(r2, []string{"RUNNING"}, "SUCCEEDED")

	n := f.notifier()
	f.drain(n)

	if got := hook.count(); got != 2 {
		t.Fatalf("receiver got %d requests, want 2 (etl failure once, other success once)", got)
	}
	// Deliveries are sent concurrently, so find the failure by type.
	hook.mu.Lock()
	var first map[string]any
	var h http.Header
	for i, b := range hook.bodies {
		if b["type"] == notify.EventRunFailed {
			first, h = b, hook.hdrs[i]
		}
	}
	hook.mu.Unlock()
	if first["type"] != notify.EventRunFailed || first["run_id"] != r1 || first["link"] != "https://nexus.example/runs/"+r1 {
		t.Fatalf("first body %v", first)
	}
	if h.Get("X-Nexus-Signature") == "" || h.Get("Idempotency-Key") == "" {
		t.Fatalf("headers %v", h)
	}

	// Re-evaluating the same facts (as after a crash before the mark) adds nothing.
	if _, err := f.pool.Exec(f.ctx, `UPDATE outbox SET notified_at = NULL WHERE routing_key = ANY($1)`, []string{r1, r2}); err != nil {
		t.Fatal(err)
	}
	f.drain(n)
	if got := hook.count(); got != 2 {
		t.Fatalf("re-evaluation re-sent: %d requests", got)
	}
	if d := f.deliveries("delivered"); len(d) != 2 {
		t.Fatalf("%d delivered rows, want 2", len(d))
	}
}

func TestRuleCreatedAfterTheFactDoesNotMatchIt(t *testing.T) {
	f := newFixture(t)
	hook := newReceiver(t)
	spec := f.spec("late", nil, nil)
	r := f.run(spec)
	f.transition(r, []string{"PENDING"}, "RUNNING")
	f.transition(r, []string{"RUNNING"}, "FAILED")
	time.Sleep(10 * time.Millisecond)
	ch := f.channel("late-hook", notify.KindWebhook, map[string]any{"url": hook.srv.URL}, "k", 0)
	f.rule("late", []string{notify.EventRunFailed}, nil, nil, nil, ch)
	f.drain(f.notifier())
	if hook.count() != 0 {
		t.Fatal("a rule paged about a fact that predates it")
	}
}

func TestRetryBackoffAndDeadLetter(t *testing.T) {
	f := newFixture(t)
	hook := newReceiver(t)
	ch := f.channel("flaky", notify.KindWebhook, map[string]any{"url": hook.srv.URL}, "k", 0)
	f.rule("r", []string{notify.EventRunFailed}, nil, nil, nil, ch)
	r := f.run(f.spec("wf", nil, nil))
	f.transition(r, []string{"PENDING"}, "RUNNING")
	f.transition(r, []string{"RUNNING"}, "FAILED")
	n := f.notifierWithBackoff(time.Hour)

	hook.status.Store(503)
	f.drain(n)
	p := f.deliveries("pending")
	if len(p) != 1 || p[0].Attempts != 1 || p[0].LastStatus == nil || *p[0].LastStatus != 503 {
		t.Fatalf("after a 503: %+v", p)
	}
	if p[0].NextAttemptAt.Before(time.Now().Add(20 * time.Minute)) {
		t.Fatalf("backoff after one failure is %v, want at least half an hour", time.Until(p[0].NextAttemptAt))
	}
	// Make it due again; a 400 is permanent.
	if _, err := f.pool.Exec(f.ctx, `UPDATE notification_deliveries SET next_attempt_at = now() WHERE id = $1`, p[0].ID); err != nil {
		t.Fatal(err)
	}
	hook.status.Store(400)
	f.drain(n)
	dead := f.deliveries("dead")
	if len(dead) != 1 || dead[0].Attempts != 2 || dead[0].DeadAt == nil {
		t.Fatalf("after a 400: %+v", dead)
	}

	// Redrive: a NEW row that keeps the original dedup key.
	orig := dead[0]
	if _, ins, err := store.NotifyEnqueue(f.ctx, f.pool, store.DeliveryInsert{
		Namespace: f.ns, ChannelID: orig.ChannelID, RuleID: orig.RuleID, EventKey: orig.EventKey + ":redrive:1",
		DedupKey: orig.DedupKey, EventType: orig.EventType, Action: orig.Action, RunID: orig.RunID,
		Payload: orig.Payload, CreatedBy: "operator", RedriveOf: &orig.ID,
	}); err != nil || !ins {
		t.Fatalf("redrive: %v %v", ins, err)
	}
	hook.status.Store(200)
	f.drain(n)
	del := f.deliveries("delivered")
	if len(del) != 1 || del[0].DedupKey != orig.DedupKey || del[0].RedriveOf == nil {
		t.Fatalf("redrive: %+v", del)
	}
	hook.mu.Lock()
	last := hook.hdrs[len(hook.hdrs)-1].Get("Idempotency-Key")
	hook.mu.Unlock()
	if last != orig.DedupKey {
		t.Fatalf("redrive sent Idempotency-Key %q, want %q", last, orig.DedupKey)
	}
}

func TestPerChannelRateLimitDefersWithoutSpendingAttempts(t *testing.T) {
	f := newFixture(t)
	hook := newReceiver(t)
	ch := f.channel("limited", notify.KindWebhook, map[string]any{"url": hook.srv.URL}, "k", 2)
	f.rule("r", []string{notify.EventRunFailed}, nil, nil, nil, ch)
	spec := f.spec("wf", nil, nil)
	for i := 0; i < 5; i++ {
		r := f.run(spec)
		f.transition(r, []string{"PENDING"}, "RUNNING")
		f.transition(r, []string{"RUNNING"}, "FAILED")
	}
	f.drain(f.notifier())
	if got := hook.count(); got != 2 {
		// The window is per wall-clock minute; a test straddling a minute
		// boundary could legitimately send up to 4.
		if got > 4 || got < 2 {
			t.Fatalf("sent %d with a limit of 2/minute", got)
		}
	}
	for _, d := range f.deliveries("pending") {
		if d.Attempts != 0 || d.NextAttemptAt.Before(time.Now()) {
			t.Fatalf("deferred delivery spent an attempt or is not deferred: %+v", d)
		}
	}
}

func TestPagerDutyTriggerThenResolveOnRecovery(t *testing.T) {
	f := newFixture(t)
	pd := f.channel("oncall", notify.KindPagerDuty, map[string]any{"severity": "critical"}, "routing-key-123456", 0)
	f.rule("page", []string{notify.EventRunFailed, notify.EventRunTimedOut}, []string{"etl"}, nil, nil, pd)
	spec := f.spec("etl", nil, nil)
	n := f.notifier()

	// Two failures group into ONE incident (same dedup key).
	for i := 0; i < 2; i++ {
		r := f.run(spec)
		f.transition(r, []string{"PENDING"}, "RUNNING")
		f.transition(r, []string{"RUNNING"}, "FAILED")
	}
	f.drain(n)
	// A success resolves it.
	ok := f.run(spec)
	f.transition(ok, []string{"PENDING"}, "RUNNING")
	f.transition(ok, []string{"RUNNING"}, "SUCCEEDED")
	f.drain(n)
	// A second success has nothing open: nothing sent.
	ok2 := f.run(spec)
	f.transition(ok2, []string{"PENDING"}, "RUNNING")
	f.transition(ok2, []string{"RUNNING"}, "SUCCEEDED")
	f.drain(n)

	f.pd.mu.Lock()
	defer f.pd.mu.Unlock()
	if len(f.pd.bodies) != 3 {
		t.Fatalf("PagerDuty got %d events, want trigger, trigger, resolve", len(f.pd.bodies))
	}
	a, b, c := f.pd.bodies[0], f.pd.bodies[1], f.pd.bodies[2]
	if a["event_action"] != "trigger" || b["event_action"] != "trigger" || c["event_action"] != "resolve" {
		t.Fatalf("actions %v %v %v", a["event_action"], b["event_action"], c["event_action"])
	}
	if a["dedup_key"] != b["dedup_key"] || b["dedup_key"] != c["dedup_key"] {
		t.Fatalf("dedup keys differ: %v %v %v", a["dedup_key"], b["dedup_key"], c["dedup_key"])
	}
}

func TestAttemptLostAndSLAExceeded(t *testing.T) {
	f := newFixture(t)
	hook := newReceiver(t)
	ch := f.channel("h", notify.KindWebhook, map[string]any{"url": hook.srv.URL}, "k", 0)
	sla := int32(60)
	f.rule("lost", []string{notify.EventAttemptLost, notify.EventRunSLAExceeded}, nil, nil, &sla, ch)
	r := f.run(f.spec("wf", nil, nil))
	f.transition(r, []string{"PENDING"}, "RUNNING")
	node := "extract"
	att := uuid.NewString()
	if _, err := eventservice.NewStore(f.pool).Append(f.ctx, eventservice.Event{
		RunID: r, NodeID: &node, AttemptID: &att, Kind: "AttemptLost", Actor: "scheduler",
		Payload: map[string]any{"reason": "lease_expired"},
	}); err != nil {
		t.Fatal(err)
	}
	// Make the run overdue.
	if _, err := f.pool.Exec(f.ctx, `UPDATE workflow_runs SET created_at = now() - interval '2 hours' WHERE id = $1`, r); err != nil {
		t.Fatal(err)
	}
	n := f.notifier()
	f.drain(n)
	if _, err := n.SweepSLA(f.ctx); err != nil {
		t.Fatal(err)
	}
	if again, err := n.SweepSLA(f.ctx); err != nil || again != 0 {
		t.Fatalf("second SLA sweep enqueued %d (%v)", again, err)
	}
	f.drain(n)
	types := map[string]int{}
	hook.mu.Lock()
	for _, b := range hook.bodies {
		types[b["type"].(string)]++
	}
	hook.mu.Unlock()
	if types[notify.EventAttemptLost] != 1 || types[notify.EventRunSLAExceeded] != 1 {
		t.Fatalf("got %v", types)
	}
}

func TestLineageEdgesAndMaterialization(t *testing.T) {
	f := newFixture(t)
	tasks := []map[string]any{
		{"id": "extract", "assets": map[string]any{"consumes": []any{map[string]any{"uri": "s3://raw/orders", "name": "raw orders"}}, "produces": []any{map[string]any{"uri": "pg://dw/orders_clean"}}}},
		{"id": "report", "assets": map[string]any{"consumes": []any{map[string]any{"uri": "pg://dw/orders_clean"}}}},
	}
	r := f.run(f.spec("orders", nil, tasks))
	f.transition(r, []string{"PENDING"}, "RUNNING")
	node := "extract"
	if _, err := eventservice.NewStore(f.pool).Append(f.ctx, eventservice.Event{RunID: r, NodeID: &node, Kind: "AttemptSucceeded", Actor: "scheduler", Payload: map[string]any{}}); err != nil {
		t.Fatal(err)
	}
	f.drain(f.notifier())
	var edges, mats int
	_ = f.pool.QueryRow(f.ctx, `SELECT count(*) FROM asset_lineage_edges WHERE run_id = $1`, r).Scan(&edges)
	_ = f.pool.QueryRow(f.ctx, `SELECT count(*) FROM asset_materializations WHERE run_id = $1`, r).Scan(&mats)
	if edges != 3 || mats != 1 {
		t.Fatalf("edges=%d mats=%d, want 3 and 1", edges, mats)
	}
	var name string
	var last *time.Time
	if err := f.pool.QueryRow(f.ctx, `SELECT name, last_materialized_at FROM assets WHERE namespace = $1 AND uri = 's3://raw/orders'`, f.ns).Scan(&name, &last); err != nil || name != "raw orders" || last != nil {
		t.Fatalf("raw asset: %q %v %v", name, last, err)
	}
	if err := f.pool.QueryRow(f.ctx, `SELECT last_materialized_at FROM assets WHERE namespace = $1 AND uri = 'pg://dw/orders_clean'`, f.ns).Scan(&last); err != nil || last == nil {
		t.Fatalf("clean asset not materialized: %v", err)
	}
}

// TestConcurrentNotifiersDeliverEachEventOnce runs four notifier replicas
// against one database: each fact is enqueued once and each delivery is sent
// once, with no leader.
func TestConcurrentNotifiersDeliverEachEventOnce(t *testing.T) {
	f := newFixture(t)
	hook := newReceiver(t)
	ch := f.channel("c", notify.KindWebhook, map[string]any{"url": hook.srv.URL}, "k", 6000)
	f.rule("r", []string{notify.EventRunFailed}, nil, nil, nil, ch)
	spec := f.spec("wf", nil, nil)
	const runs = 40
	for i := 0; i < runs; i++ {
		r := f.run(spec)
		f.transition(r, []string{"PENDING"}, "RUNNING")
		f.transition(r, []string{"RUNNING"}, "FAILED")
	}
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			n := f.notifier()
			for j := 0; j < 30; j++ {
				if _, err := n.EvaluateBatch(f.ctx); err != nil {
					t.Error(err)
				}
				if _, err := n.DispatchOnce(f.ctx); err != nil {
					t.Error(err)
				}
			}
		}()
	}
	wg.Wait()
	f.drain(f.notifier())
	if got := hook.count(); got != runs {
		t.Fatalf("receiver got %d, want exactly %d", got, runs)
	}
	seen := map[string]bool{}
	hook.mu.Lock()
	for _, b := range hook.bodies {
		id := b["run_id"].(string)
		if seen[id] {
			t.Errorf("run %s notified twice", id)
		}
		seen[id] = true
	}
	hook.mu.Unlock()
}

func TestSealedSecretBoundToChannel(t *testing.T) {
	f := newFixture(t)
	hook := newReceiver(t)
	a := f.channel("a", notify.KindSlackWebhook, nil, hook.srv.URL, 0)
	b := f.channel("b", notify.KindSlackWebhook, nil, hook.srv.URL, 0)
	// Copy a's ciphertext onto b: b's deliveries must fail, not use a's secret.
	if _, err := f.pool.Exec(f.ctx, `UPDATE notification_channels SET secret_ciphertext = (SELECT secret_ciphertext FROM notification_channels WHERE id = $1) WHERE id = $2`, a, b); err != nil {
		t.Fatal(err)
	}
	f.rule("r", []string{notify.EventRunFailed}, nil, nil, nil, b)
	r := f.run(f.spec("wf", nil, nil))
	f.transition(r, []string{"PENDING"}, "RUNNING")
	f.transition(r, []string{"RUNNING"}, "FAILED")
	f.drain(f.notifier())
	if hook.count() != 0 || len(f.deliveries("dead")) != 1 {
		t.Fatalf("a ciphertext moved between rows was accepted (sent=%d)", hook.count())
	}
}
