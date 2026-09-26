// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package auditchain

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// TestCanonicalV1IsPinned pins the encoding with a golden value: changing it
// silently would invalidate every chain already written. A deliberate change
// is a new Version.
func TestCanonicalV1IsPinned(t *testing.T) {
	actor := "0F3C0B7E-1111-4222-8333-944445555666"
	r := Row{
		Namespace: "team-a", Seq: 7, OccurredAt: time.Date(2026, 9, 25, 10, 11, 12, 345678000, time.UTC),
		ActorID: &actor, ActorKind: "user", ActorLabel: "Ana <ana@x>", Action: "token.create",
		TargetKind: "token", TargetID: "t-1", Outcome: "success",
		Detail:   json.RawMessage(`{"scopes": ["read"], "b": 1.50, "a": {"z": null, "&": "<>"}}`),
		ClientIP: "10.0.0.1", PrevHash: strings.Repeat("a", 64),
	}
	c, err := Canonical(r)
	if err != nil {
		t.Fatal(err)
	}
	want := `["nexus-audit-v1","team-a",7,"2026-09-25T10:11:12.345678Z","0f3c0b7e-1111-4222-8333-944445555666","user","Ana <ana@x>","token.create","token","t-1","success",{"a":{"&":"<>","z":null},"b":1.50,"scopes":["read"]},"10.0.0.1","` + strings.Repeat("a", 64) + `"]`
	if string(c) != want {
		t.Fatalf("canonical_v1 changed:\n got %s\nwant %s", c, want)
	}
	h, _ := Hash(r)
	// The canonical bytes are pinned above; the hash is their sha256.
	if len(h) != 64 {
		t.Fatal("hash is not hex sha256")
	}
	// Key order and whitespace in the stored detail do not matter.
	r2 := r
	r2.Detail = json.RawMessage(`{"b":1.50,"a":{"&":"<>","z":null},"scopes":["read"]}`)
	if h2, _ := Hash(r2); h2 != h {
		t.Error("detail key order changed the hash")
	}
	// Every field is covered.
	for name, mut := range map[string]func(*Row){
		"namespace": func(x *Row) { x.Namespace = "team-b" }, "seq": func(x *Row) { x.Seq = 8 },
		"time":  func(x *Row) { x.OccurredAt = x.OccurredAt.Add(time.Microsecond) },
		"actor": func(x *Row) { x.ActorID = nil }, "kind": func(x *Row) { x.ActorKind = "api_key" },
		"label": func(x *Row) { x.ActorLabel = "Ana" }, "action": func(x *Row) { x.Action = "token.revoke" },
		"tkind": func(x *Row) { x.TargetKind = "user" }, "tid": func(x *Row) { x.TargetID = "t-2" },
		"outcome": func(x *Row) { x.Outcome = "denied" }, "detail": func(x *Row) { x.Detail = json.RawMessage(`{}`) },
		"ip": func(x *Row) { x.ClientIP = "10.0.0.2" }, "prev": func(x *Row) { x.PrevHash = strings.Repeat("b", 64) },
	} {
		m := r
		mut(&m)
		if hm, _ := Hash(m); hm == h {
			t.Errorf("changing %s does not change the hash", name)
		}
	}
}

// buildExport produces a valid export of n rows anchored at genesis.
func buildExport(t *testing.T, n int, mutate func(i int, rec *Record)) []byte {
	t.Helper()
	ns := "team-a"
	prev := GenesisHash(ns)
	var recs []Record
	for i := 1; i <= n; i++ {
		seq := int64(i)
		row := Row{Namespace: ns, Seq: seq, OccurredAt: time.Date(2026, 9, 25, 0, 0, i, 0, time.UTC),
			ActorKind: "user", Action: "user.create", Outcome: "success", Detail: json.RawMessage(`{"n":` + string(rune('0'+i%10)) + `}`), PrevHash: prev}
		h, err := Hash(row)
		if err != nil {
			t.Fatal(err)
		}
		p, hh := prev, h
		recs = append(recs, Record{ID: int64(100 + i), Namespace: ns, Seq: &seq, OccurredAt: row.OccurredAt.Format(TimeLayout),
			ActorKind: "user", Action: "user.create", Outcome: "success", Detail: row.Detail, PrevHash: &p, RowHash: &hh})
		prev = h
	}
	var head *Checkpoint
	if n > 0 {
		head = &Checkpoint{Kind: "head", Seq: int64(n), Hash: prev}
	}
	for i := range recs {
		if mutate != nil {
			mutate(i, &recs[i])
		}
	}
	var b bytes.Buffer
	w := NewNDJSONWriter(&b)
	_ = w.WriteHeader(Header{Namespace: ns, Complete: true, Anchor: &Checkpoint{Kind: "genesis", Seq: 0, Hash: GenesisHash(ns)},
		ChainStartedAt: "2026-09-25T00:00:00.000000Z"})
	for _, r := range recs {
		if r.Type == "skip" {
			continue
		}
		_ = w.Write(r)
	}
	_ = w.Close(head)
	return b.Bytes()
}

func TestVerifyAcceptsAnIntactChain(t *testing.T) {
	res, err := Verify(bytes.NewReader(buildExport(t, 25, nil)))
	if err != nil || !res.OK(false) || res.Chained != 25 || res.LastSeq != 25 {
		t.Fatalf("res=%+v err=%v", res, err)
	}
}

func TestVerifyDetectsTampering(t *testing.T) {
	cases := map[string]struct {
		mut  func(i int, r *Record)
		kind string
		cut  func([]byte) []byte
	}{
		"altered detail": {mut: func(i int, r *Record) {
			if i == 9 {
				r.Detail = json.RawMessage(`{"n":"forged"}`)
			}
		}, kind: "altered"},
		"altered outcome": {mut: func(i int, r *Record) {
			if i == 3 {
				r.Outcome = "denied"
			}
		}, kind: "altered"},
		"deleted row": {mut: func(i int, r *Record) {
			if i == 12 {
				r.Type = "skip"
			}
		}, kind: "gap"},
		"rehashed forgery breaks the next link": {mut: func(i int, r *Record) {
			if i == 5 {
				r.Outcome = "denied"
				row, _ := r.RowOf()
				row.Seq = *r.Seq
				h, _ := Hash(row)
				r.RowHash = &h
			}
		}, kind: "broken-link"},
		"reordered": {mut: func(i int, r *Record) {
			if i == 7 {
				s := int64(3)
				r.Seq = &s
			}
		}, kind: "reordered"},
		"tail truncated": {cut: func(b []byte) []byte {
			lines := bytes.Split(bytes.TrimSpace(b), []byte("\n"))
			// drop the last row but keep the (original) trailer
			out := append(append([][]byte{}, lines[:len(lines)-2]...), lines[len(lines)-1])
			return append(bytes.Join(out, []byte("\n")), '\n')
		}, kind: "truncated"},
		"no trailer": {cut: func(b []byte) []byte {
			lines := bytes.Split(bytes.TrimSpace(b), []byte("\n"))
			return append(bytes.Join(lines[:len(lines)-1], []byte("\n")), '\n')
		}, kind: "truncated"},
		"unchained row": {mut: func(i int, r *Record) {
			if i == 20 {
				r.Seq = nil
			}
		}, kind: "unchained"},
	}
	for name, c := range cases {
		b := buildExport(t, 25, c.mut)
		if c.cut != nil {
			b = c.cut(b)
		}
		res, err := Verify(bytes.NewReader(b))
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		p := res.FirstProblem(false)
		if p == nil {
			t.Errorf("%s: not detected", name)
			continue
		}
		found := false
		for _, q := range res.Problems {
			if q.Kind == c.kind {
				found = true
			}
		}
		if !found {
			t.Errorf("%s: problems %v, want a %q", name, res.Problems, c.kind)
		}
	}
}

func TestPreChainRowsAreReportedNotFailed(t *testing.T) {
	var b bytes.Buffer
	w := NewNDJSONWriter(&b)
	_ = w.WriteHeader(Header{Namespace: "n", Complete: true, ChainStartedAt: "2026-09-25T00:00:00.000000Z"})
	_ = w.Write(Record{ID: 1, Namespace: "n", OccurredAt: "2026-01-01T00:00:00.000000Z", Detail: json.RawMessage(`{}`)})
	_ = w.Close(nil)
	res, err := Verify(&b)
	if err != nil || !res.OK(false) || res.PreChain != 1 {
		t.Fatalf("%+v %v", res, err)
	}
}

func TestCSVNeutralisesFormulas(t *testing.T) {
	var b bytes.Buffer
	w, _ := NewCSVWriter(&b)
	s := int64(1)
	_ = w.Write(Record{ID: 1, Namespace: "n", Seq: &s, ActorLabel: `=HYPERLINK("http://x","y")`, TargetID: "+1", Action: "a,b", Detail: json.RawMessage(`{"q":"\"x\""}`)})
	_ = w.Flush()
	rows, err := csv.NewReader(&b).ReadAll()
	if err != nil || len(rows) != 2 {
		t.Fatalf("csv: %v %d", err, len(rows))
	}
	if rows[1][6] != `'=HYPERLINK("http://x","y")` || rows[1][9] != "'+1" || rows[1][7] != "a,b" || rows[1][11] != `{"q":"\"x\""}` {
		t.Errorf("row = %q", rows[1])
	}
}
