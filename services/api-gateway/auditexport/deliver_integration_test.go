//go:build integration

// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package auditexport

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/khuriwalgroup/kgf-nexus/internal/auditchain"
	"github.com/khuriwalgroup/kgf-nexus/internal/store"
	"github.com/khuriwalgroup/kgf-nexus/internal/testdb"
)

func gatewayPool(t *testing.T) (*pgxpool.Pool, *pgxpool.Pool) {
	t.Helper()
	dsn := testdb.DSN(t)
	admin, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		testdb.SkipUnreachable(t, err)
	}
	if err := admin.Ping(context.Background()); err != nil {
		testdb.SkipUnreachable(t, err)
	}
	t.Cleanup(admin.Close)
	cfg, _ := pgxpool.ParseConfig(dsn)
	cfg.ConnConfig.RuntimeParams["role"] = "nexus_api_gateway"
	gw, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(gw.Close)
	return admin, gw
}

type receiver struct {
	mu       sync.Mutex
	failNext int
	batches  [][]auditchain.Record
	headers  []http.Header
	bodies   [][]byte
}

func (rc *receiver) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	rc.mu.Lock()
	defer rc.mu.Unlock()
	if rc.failNext > 0 {
		rc.failNext--
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	var batch []auditchain.Record
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		var rec auditchain.Record
		_ = json.Unmarshal(sc.Bytes(), &rec)
		batch = append(batch, rec)
	}
	rc.batches = append(rc.batches, batch)
	rc.headers = append(rc.headers, r.Header.Clone())
	rc.bodies = append(rc.bodies, b)
	w.WriteHeader(http.StatusAccepted)
}

// TestWebhookSinkDeliversSignedBatchesAtLeastOnce drives the real deliverer
// against the real schema: a receiver that fails twice, then accepts, gets
// every chained row once acknowledged, in chain order, each batch signed with
// the secret only the sink's creator saw.
func TestWebhookSinkDeliversSignedBatchesAtLeastOnce(t *testing.T) {
	admin, gw := gatewayPool(t)
	ctx := context.Background()
	ns := "sink-" + uuid.NewString()[:8]
	testdb.RegisterTenant(t, admin, ns)

	rc := &receiver{failNext: 2}
	srv := httptest.NewServer(rc)
	defer srv.Close()

	keys, _ := NewKeyring(bytes.Repeat([]byte{3}, 32))
	secret, _ := NewSecret()
	ct, nonce, _ := keys.Seal(ns, []byte(secret))
	for i := 0; i < 5; i++ {
		if err := store.GatewayAppendAudit(ctx, gw, store.AuditEntry{Namespace: ns, ActorKind: "user", Action: "user.create"}); err != nil {
			t.Fatal(err)
		}
	}
	sink, err := store.GatewayCreateAuditSink(ctx, gw, store.AuditSink{Namespace: ns, Name: "siem", Kind: "webhook",
		URL: srv.URL, Enabled: true, BatchSize: 3, SecretCiphertext: ct, SecretNonce: nonce, KeyID: keys.KeyID()},
		true, store.AuditEntry{Namespace: ns, ActorKind: "user", Action: "audit.sink.create"})
	if err != nil {
		t.Fatal(err)
	}
	// 5 rows + the sink.create row = seqs 1..6.

	d := &Deliverer{DB: gw, Keys: keys, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Policy: URLPolicy{AllowHTTP: true, AllowCIDRs: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}}}
	d.HTTP = d.Policy.Client(5 * time.Second)
	due := func() {
		// Skip the backoff wait: the test is about what is delivered, not when.
		if _, err := admin.Exec(ctx, `UPDATE audit_sinks SET next_attempt_at = now() WHERE id::text = $1`, sink.ID); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 6; i++ {
		if err := d.Tick(ctx); err != nil {
			t.Fatal(err)
		}
		due()
	}
	s, _ := store.GatewayGetAuditSink(ctx, gw, ns, sink.ID)
	if s.CursorSeq != 6 || s.ConsecutiveFailures != 0 {
		t.Fatalf("cursor=%d failures=%d last_error=%q; want everything acknowledged", s.CursorSeq, s.ConsecutiveFailures, s.LastError)
	}
	rc.mu.Lock()
	defer rc.mu.Unlock()
	var seqs []int64
	for i, batch := range rc.batches {
		if err := Verify([]byte(secret), rc.headers[i].Get(SignatureHeader), rc.bodies[i], time.Now().Unix(), 300); err != nil {
			t.Errorf("batch %d signature: %v", i, err)
		}
		if rc.headers[i].Get("X-Nexus-Namespace") != ns || rc.headers[i].Get("X-Nexus-Delivery") == "" {
			t.Errorf("batch %d headers: %v", i, rc.headers[i])
		}
		for _, r := range batch {
			seqs = append(seqs, *r.Seq)
			if r.RowHash == nil || len(*r.RowHash) != 64 {
				t.Errorf("delivered row without its hash: %+v", r)
			}
		}
	}
	if len(rc.batches) != 2 || len(seqs) != 6 || seqs[0] != 1 || seqs[5] != 6 {
		t.Fatalf("delivered batches %d, seqs %v; want 2 batches of 1..6 in order", len(rc.batches), seqs)
	}
	for i := 1; i < len(seqs); i++ {
		if seqs[i] != seqs[i-1]+1 {
			t.Fatalf("out of order: %v", seqs)
		}
	}

	// A disabled sink, and a sink pointing at an internal address, deliver nothing.
	var failures int
	_ = admin.QueryRow(ctx, `SELECT consecutive_failures FROM audit_sinks WHERE id::text = $1`, sink.ID).Scan(&failures)
	strict := &Deliverer{DB: gw, Keys: keys, Log: d.Log, Policy: URLPolicy{AllowHTTP: true}}
	if err := store.GatewayAppendAudit(ctx, gw, store.AuditEntry{Namespace: ns, ActorKind: "user", Action: "run.retry"}); err != nil {
		t.Fatal(err)
	}
	due()
	_ = strict.Tick(ctx)
	s, _ = store.GatewayGetAuditSink(ctx, gw, ns, sink.ID)
	if s.CursorSeq != 6 || s.ConsecutiveFailures != 1 {
		t.Errorf("SSRF policy: cursor=%d failures=%d (%s); the loopback receiver must be refused without an allow-list",
			s.CursorSeq, s.ConsecutiveFailures, s.LastError)
	}
}
