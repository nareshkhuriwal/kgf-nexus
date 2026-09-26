//go:build integration

// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/khuriwalgroup/kgf-nexus/internal/auditchain"
)

// exportAndVerify runs a full (unfiltered) export through the real writer and
// the independent verifier, exactly as `nexusctl audit verify` would.
func exportAndVerify(t *testing.T, db GWDB, ns string) auditchain.Result {
	t.Helper()
	var buf bytes.Buffer
	w := auditchain.NewNDJSONWriter(&buf)
	var head *auditchain.Checkpoint
	err := GatewayExportAudit(context.Background(), db, ns, AuditFilter{}, func(m AuditExportMeta) error {
		head = m.Head
		h := auditchain.Header{Namespace: ns, Complete: true, Anchor: m.Anchor, GeneratedAt: "now"}
		if m.ChainStartedAt != nil {
			h.ChainStartedAt = m.ChainStartedAt.UTC().Format(auditchain.TimeLayout)
		}
		return w.WriteHeader(h)
	}, func(r AuditRow) error { return w.Write(r.Record()) })
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if err := w.Close(head); err != nil {
		t.Fatal(err)
	}
	res, err := auditchain.Verify(&buf)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	return res
}

func appendN(t *testing.T, db GWQuerier, ns string, n int, action string) {
	t.Helper()
	for i := 0; i < n; i++ {
		if err := GatewayAppendAudit(context.Background(), db, AuditEntry{Namespace: ns, ActorKind: "user",
			ActorLabel: "tester", Action: action, TargetID: fmt.Sprint(i),
			Detail: map[string]any{"i": i, "big": 1e21, "nested": map[string]any{"b": 1.50, "a": "<&>"}}}); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
}

// superuserExec runs SQL with triggers suppressed, which is what an attacker
// with database superuser rights can do -- the case the chain exists for.
func superuserExec(t *testing.T, admin *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	ctx := context.Background()
	tx, err := admin.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SET LOCAL session_replication_role = replica`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestAuditChainIsUnforkedUnderConcurrentReplicas(t *testing.T) {
	t.Parallel()
	admin := testPool(t)
	gw := rolePool(t, "nexus_api_gateway")
	ns := uniqueNS(t, admin)
	var wg sync.WaitGroup
	errs := make(chan error, 200)
	for g := 0; g < 25; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 8; i++ {
				if err := GatewayAppendAudit(context.Background(), gw, AuditEntry{Namespace: ns, ActorKind: "api_key",
					Action: "run.retry", TargetID: fmt.Sprintf("%d-%d", g, i)}); err != nil {
					errs <- err
				}
			}
		}(g)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent append: %v", err)
	}
	var n, maxSeq, distinct int64
	if err := admin.QueryRow(context.Background(), `SELECT count(*), max(chain_seq), count(DISTINCT chain_seq)
		FROM audit_log WHERE namespace = $1`, ns).Scan(&n, &maxSeq, &distinct); err != nil {
		t.Fatal(err)
	}
	if n != 200 || maxSeq != 200 || distinct != 200 {
		t.Fatalf("rows=%d max_seq=%d distinct=%d; want 200 rows at seqs 1..200", n, maxSeq, distinct)
	}
	res := exportAndVerify(t, gw, ns)
	if !res.OK(false) || res.Chained != 200 || res.LastSeq != 200 || res.Anchor == nil || res.Anchor.Kind != "genesis" {
		t.Fatalf("verification of a clean concurrent chain: %+v", res)
	}
}

func TestAuditTamperingIsDetected(t *testing.T) {
	t.Parallel()
	admin := testPool(t)
	cases := map[string]struct {
		tamper func(ns string)
		kind   string
	}{
		"edited outcome": {func(ns string) {
			superuserExec(t, admin, `UPDATE audit_log SET outcome = 'denied' WHERE namespace = $1 AND chain_seq = 4`, ns)
		}, "altered"},
		"edited detail": {func(ns string) {
			superuserExec(t, admin, `UPDATE audit_log SET detail = '{"i": 99}' WHERE namespace = $1 AND chain_seq = 2`, ns)
		}, "altered"},
		"deleted row": {func(ns string) {
			superuserExec(t, admin, `DELETE FROM audit_log WHERE namespace = $1 AND chain_seq = 5`, ns)
		}, "gap"},
		"deleted tail": {func(ns string) {
			superuserExec(t, admin, `DELETE FROM audit_log WHERE namespace = $1 AND chain_seq = 8`, ns)
		}, "truncated"},
		"moved row": {func(ns string) {
			superuserExec(t, admin, `UPDATE audit_log SET chain_seq = 100 WHERE namespace = $1 AND chain_seq = 3`, ns)
		}, "gap"},
		"unchained insert": {func(ns string) {
			// What an older gateway replica (or any INSERT-capable role) writes:
			// a plain row with no chain position.
			if _, err := admin.Exec(context.Background(), `INSERT INTO audit_log (namespace, actor_kind, action) VALUES ($1, 'user', 'token.create')`, ns); err != nil {
				t.Fatal(err)
			}
		}, "unchained"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			ns := uniqueNS(t, admin)
			appendN(t, admin, ns, 8, "user.create")
			if res := exportAndVerify(t, admin, ns); !res.OK(false) {
				t.Fatalf("clean chain failed: %v", res.Problems)
			}
			c.tamper(ns)
			res := exportAndVerify(t, admin, ns)
			found := false
			for _, p := range res.Problems {
				if p.Kind == c.kind {
					found = true
				}
			}
			if res.OK(false) || !found {
				t.Fatalf("tampering not detected as %q: %+v", c.kind, res.Problems)
			}
		})
	}
}

func TestAuditLogIsAppendOnlyForEveryRole(t *testing.T) {
	t.Parallel()
	admin := testPool(t)
	gw := rolePool(t, "nexus_api_gateway")
	ns := uniqueNS(t, admin)
	appendN(t, gw, ns, 2, "user.create")
	ctx := context.Background()
	for who, db := range map[string]*pgxpool.Pool{"superuser (trigger)": admin, "gateway (grant)": gw} {
		if _, err := db.Exec(ctx, `UPDATE audit_log SET outcome = 'x' WHERE namespace = $1`, ns); err == nil {
			t.Errorf("%s could UPDATE audit_log", who)
		}
		if _, err := db.Exec(ctx, `DELETE FROM audit_log WHERE namespace = $1`, ns); err == nil {
			t.Errorf("%s could DELETE from audit_log", who)
		}
	}
	if _, err := gw.Exec(ctx, `DELETE FROM audit_chain_checkpoints WHERE namespace = $1`, ns); err == nil {
		t.Error("the gateway could delete chain checkpoints")
	}
	if _, err := gw.Exec(ctx, `SELECT set_config('nexus.audit_purge', 'on', false); DELETE FROM audit_log WHERE namespace = '`+ns+`'`); err == nil {
		t.Error("the gateway bypassed audit_purge with the session flag")
	}
}

func TestAuditPaginationIsStableWhileRowsArrive(t *testing.T) {
	t.Parallel()
	admin := testPool(t)
	ns := uniqueNS(t, admin)
	appendN(t, admin, ns, 30, "run.retry")
	ctx := context.Background()
	seen := map[int64]bool{}
	cursor := ""
	pages := 0
	for {
		rows, next, err := GatewayListAudit(ctx, admin, ns, AuditFilter{Cursor: cursor, Limit: 7})
		if err != nil {
			t.Fatal(err)
		}
		pages++
		for _, r := range rows {
			if seen[r.ID] {
				t.Fatalf("row %d served twice", r.ID)
			}
			seen[r.ID] = true
		}
		// Rows arriving mid-pagination are newer than every cursor: they must
		// not shift or repeat what the next pages return.
		appendN(t, admin, ns, 3, "run.retry")
		if next == "" {
			break
		}
		cursor = next
	}
	if len(seen) != 30 || pages != 5 {
		t.Fatalf("saw %d rows over %d pages; want the original 30 over 5", len(seen), pages)
	}
	if _, _, err := GatewayListAudit(ctx, admin, ns, AuditFilter{Cursor: "not-a-cursor"}); !errors.Is(err, ErrBadCursor) {
		t.Errorf("forged cursor: %v", err)
	}
	since := time.Now().Add(-time.Hour)
	rows, _, err := GatewayListAudit(ctx, admin, ns, AuditFilter{Since: &since, Action: "run.retry", Limit: 1000})
	if err != nil || len(rows) != 30+3*5 {
		t.Errorf("since filter: %d rows err=%v", len(rows), err)
	}
}

// TestAuditRetentionLegalHoldAndCheckpoints is not parallel: it moves the
// package's audit clock to write back-dated rows.
func TestAuditRetentionLegalHoldAndCheckpoints(t *testing.T) {
	admin := testPool(t)
	gw := rolePool(t, "nexus_api_gateway")
	ns := uniqueNS(t, admin)
	ctx := context.Background()

	auditNow = func() time.Time { return time.Now().Add(-10 * 24 * time.Hour) }
	appendN(t, gw, ns, 5, "user.create") // seqs 1..5, ten days old
	auditNow = time.Now
	appendN(t, gw, ns, 3, "user.create") // seqs 6..8, now

	// An enabled sink that has only acknowledged seq 3.
	var sinkID string
	if err := admin.QueryRow(ctx, `INSERT INTO audit_sinks (namespace, name, kind, url, secret_ciphertext, secret_nonce, key_id, cursor_seq)
		VALUES ($1, 'siem', 'webhook', 'https://siem.example', '\x00', '\x00', 'k', 3) RETURNING id::text`, ns).Scan(&sinkID); err != nil {
		t.Fatal(err)
	}
	three := 3
	hold := true
	entry := func(a string) AuditEntry {
		return AuditEntry{Namespace: ns, ActorKind: "user", ActorLabel: "admin", Action: a}
	}
	if _, err := GatewayPutAuditPolicy(ctx, gw, ns, AuditPolicyChange{SetRetention: true, RetentionDays: &three, Revision: 0, Actor: "admin"},
		entry("audit.policy.update")); err != nil {
		t.Fatal(err)
	}
	if _, err := GatewayPutAuditPolicy(ctx, gw, ns, AuditPolicyChange{SetRetention: true, Revision: 0, Actor: "x"},
		entry("audit.policy.update")); !errors.Is(err, ErrRevisionConflict) {
		t.Errorf("stale revision: %v", err)
	}
	if _, err := GatewayPutAuditPolicy(ctx, gw, ns, AuditPolicyChange{LegalHold: &hold, Reason: "matter 42", Revision: 1, Actor: "admin"},
		entry("audit.legal_hold.set")); err != nil {
		t.Fatal(err)
	}
	if _, err := GatewayPurgeAudit(ctx, gw, ns); !errors.Is(err, ErrLegalHold) {
		t.Fatalf("purge under legal hold: %v", err)
	}
	release := false
	if _, err := GatewayPutAuditPolicy(ctx, gw, ns, AuditPolicyChange{LegalHold: &release, Revision: 2, Actor: "admin"},
		entry("audit.legal_hold.release")); err != nil {
		t.Fatal(err)
	}

	n, err := GatewayPurgeAudit(ctx, gw, ns)
	if err != nil || n != 3 {
		t.Fatalf("purge past the sink cursor: removed %d, err %v; want seqs 1..3 only", n, err)
	}
	res := exportAndVerify(t, gw, ns)
	if !res.OK(false) || res.Anchor == nil || res.Anchor.Kind != "purge" || res.Anchor.Seq != 3 || res.FirstSeq != 4 {
		t.Fatalf("after the first purge: %+v", res)
	}
	// The sink catches up; the remaining old rows go, the new ones stay.
	if _, err := admin.Exec(ctx, `UPDATE audit_sinks SET cursor_seq = 11 WHERE id::text = $1`, sinkID); err != nil {
		t.Fatal(err)
	}
	if n, err := GatewayPurgeAudit(ctx, gw, ns); err != nil || n != 2 {
		t.Fatalf("second purge removed %d, err %v; want seqs 4..5", n, err)
	}
	res = exportAndVerify(t, gw, ns)
	if !res.OK(false) || res.Anchor.Seq != 5 || res.FirstSeq != 6 || res.Chained != 6 {
		t.Fatalf("after the second purge: %+v", res)
	}
	var checkpoints int
	_ = admin.QueryRow(ctx, `SELECT count(*) FROM audit_chain_checkpoints WHERE namespace = $1 AND kind = 'purge'`, ns).Scan(&checkpoints)
	if checkpoints != 2 {
		t.Errorf("%d purge checkpoints, want 2", checkpoints)
	}
	var hasPolicyRows int
	_ = admin.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE namespace = $1 AND action LIKE 'audit.%'`, ns).Scan(&hasPolicyRows)
	if hasPolicyRows != 3 {
		t.Errorf("policy/hold changes audited %d times, want 3 (the conflicting one must not be)", hasPolicyRows)
	}
}

func TestAuditExportFiltersAndDetailNormalisation(t *testing.T) {
	t.Parallel()
	admin := testPool(t)
	ns := uniqueNS(t, admin)
	appendN(t, admin, ns, 3, "user.create")
	appendN(t, admin, ns, 2, "token.create")
	var got []string
	err := GatewayExportAudit(context.Background(), admin, ns, AuditFilter{Action: "token.create"},
		func(AuditExportMeta) error { return nil },
		func(r AuditRow) error {
			got = append(got, r.Action)
			// PostgreSQL normalised 1e21 before hashing; the stored form must
			// be what the hash covers.
			if !strings.Contains(string(r.Detail), `"big": 1000000000000000000000`) {
				t.Errorf("detail = %s", r.Detail)
			}
			return nil
		})
	if err != nil || len(got) != 2 {
		t.Fatalf("filtered export: %v %v", got, err)
	}
}
