// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/khuriwalgroup/kgf-nexus/internal/auditchain"
)

// chainExport builds a valid 6-row export.
func chainExport(t *testing.T) []byte {
	t.Helper()
	ns := "team-a"
	prev := auditchain.GenesisHash(ns)
	var b bytes.Buffer
	w := auditchain.NewNDJSONWriter(&b)
	_ = w.WriteHeader(auditchain.Header{Namespace: ns, Complete: true,
		Anchor: &auditchain.Checkpoint{Kind: "genesis", Hash: prev}})
	for i := int64(1); i <= 6; i++ {
		seq := i
		row := auditchain.Row{Namespace: ns, Seq: seq, OccurredAt: time.Date(2026, 9, 1, 0, 0, int(i), 0, time.UTC),
			ActorKind: "user", Action: "token.create", Outcome: "success", Detail: json.RawMessage(`{}`), PrevHash: prev}
		h, _ := auditchain.Hash(row)
		p, hh := prev, h
		_ = w.Write(auditchain.Record{ID: 10 + i, Namespace: ns, Seq: &seq, OccurredAt: row.OccurredAt.Format(auditchain.TimeLayout),
			ActorKind: "user", Action: "token.create", Outcome: "success", Detail: row.Detail, PrevHash: &p, RowHash: &hh})
		prev = h
	}
	_ = w.Close(&auditchain.Checkpoint{Kind: "head", Seq: 6, Hash: prev})
	return b.Bytes()
}

func TestAuditVerifyOffline(t *testing.T) {
	h := newHarness(t)
	dir := t.TempDir()
	good := filepath.Join(dir, "good.ndjson")
	_ = os.WriteFile(good, chainExport(t), 0o600)
	code, out, errOut := h.run("audit", "verify", "--file", good)
	if code != 0 || !strings.Contains(out, "head:       seq 6") {
		t.Fatalf("intact export: code %d\n%s\n%s", code, out, errOut)
	}
	if len(h.requests) != 0 {
		t.Error("offline verification contacted the API")
	}

	bad := filepath.Join(dir, "bad.ndjson")
	tampered := bytes.Replace(chainExport(t), []byte(`"chain_seq":3,"occurred_at":"2026-09-01T00:00:03.000000Z","actor_id":null,"actor_kind":"user","actor_label":"","action":"token.create"`),
		[]byte(`"chain_seq":3,"occurred_at":"2026-09-01T00:00:03.000000Z","actor_id":null,"actor_kind":"user","actor_label":"","action":"token.revoke"`), 1)
	if bytes.Equal(tampered, chainExport(t)) {
		t.Fatal("the test did not tamper with anything")
	}
	_ = os.WriteFile(bad, tampered, 0o600)
	code, _, errOut = h.run("audit", "verify", "--file", bad)
	if code != ExitVerifyFailed || !strings.Contains(errOut, "seq 3") {
		t.Fatalf("tampered export: code %d, stderr %s", code, errOut)
	}

	code, out, _ = h.run("audit", "verify", "--file", good, "-o", "json")
	var res auditchain.Result
	if code != 0 || json.Unmarshal([]byte(out), &res) != nil || res.LastSeq != 6 {
		t.Fatalf("json output: %d %s", code, out)
	}
}

func TestAuditVerifyAndExportThroughTheAPI(t *testing.T) {
	h := newHarness(t)
	h.env["NEXUS_NAMESPACE"] = "team-a"
	h.env["NEXUS_API_KEY"] = "k"
	body := chainExport(t)
	h.handler = func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/v1/namespaces/team-a/audit/export" {
			return false
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		_, _ = w.Write(body)
		return true
	}
	code, out, errOut := h.run("audit", "verify")
	if code != 0 || !strings.Contains(out, "complete:   true") {
		t.Fatalf("verify via API: %d %s %s", code, out, errOut)
	}
	if q := h.last().Query; !strings.Contains(q, "format=ndjson") {
		t.Errorf("query %q", q)
	}
	file := filepath.Join(t.TempDir(), "x.ndjson")
	code, _, _ = h.run("audit", "export", "--since", "2026-09-01T00:00:00Z", "--action", "token.create", "--file", file)
	got, _ := os.ReadFile(file)
	if code != 0 || !bytes.Equal(got, body) {
		t.Fatalf("export: %d, %d bytes", code, len(got))
	}
	if q := h.last().Query; !strings.Contains(q, "since=2026-09-01T00%3A00%3A00Z") || !strings.Contains(q, "action=token.create") {
		t.Errorf("export query %q", q)
	}
	if st, _ := os.Stat(file); st.Mode().Perm() != 0o600 {
		t.Errorf("export file mode %v; an audit trail is sensitive", st.Mode().Perm())
	}
}
