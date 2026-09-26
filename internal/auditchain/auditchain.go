// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

// Package auditchain defines the audit trail's hash chain (ADR-0025 §2): the
// canonical_v1 encoding of a row, its hash, the export record format, and an
// independent verifier used by `nexusctl audit verify`.
//
// The gateway computes row hashes with Hash; the verifier recomputes them from
// an export with the same function. Keeping both in one small package with no
// database dependency is what makes "an auditor can verify offline" true: the
// verifier needs nothing but the exported file.
package auditchain

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Version tags the canonical encoding. A future encoding gets a new tag; a
// verifier refuses tags it does not know rather than guessing.
const Version = "nexus-audit-v1"

// TimeLayout is the fixed, microsecond-precision UTC form used in the
// canonical encoding and in exports. PostgreSQL timestamptz has microsecond
// resolution, so this round-trips exactly.
const TimeLayout = "2006-01-02T15:04:05.000000Z"

// GenesisHash is the prev_hash of seq 1 in a namespace whose chain started
// fresh: sha256("nexus-audit-v1:genesis:" + namespace).
func GenesisHash(namespace string) string {
	h := sha256.Sum256([]byte(Version + ":genesis:" + namespace))
	return hex.EncodeToString(h[:])
}

// Row is the hashed content of one audit row. Every field is covered.
type Row struct {
	Namespace  string
	Seq        int64
	OccurredAt time.Time
	ActorID    *string // lower-case UUID text or nil
	ActorKind  string
	ActorLabel string
	Action     string
	TargetKind string
	TargetID   string
	Outcome    string
	// Detail is the row's detail JSON as stored (any JSON text; it is
	// canonicalised before hashing).
	Detail   json.RawMessage
	ClientIP string
	PrevHash string
}

// CanonicalJSON re-encodes JSON text deterministically: numbers verbatim (as
// PostgreSQL normalised them), object keys sorted, no insignificant
// whitespace, no HTML escaping.
func CanonicalJSON(raw []byte) ([]byte, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		raw = []byte("{}")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("canonicalise detail: %w", err)
	}
	if dec.More() {
		return nil, fmt.Errorf("canonicalise detail: trailing data")
	}
	return marshal(v)
}

func marshal(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(b.Bytes(), "\n"), nil
}

// Canonical returns canonical_v1(row): a compact JSON array
//
//	["nexus-audit-v1", namespace, seq, occurred_at, actor_id|null, actor_kind,
//	 actor_label, action, target_kind, target_id, outcome, <detail>, client_ip,
//	 prev_hash]
//
// with occurred_at in TimeLayout, detail as CanonicalJSON, strings encoded by
// encoding/json with HTML escaping off.
func Canonical(r Row) ([]byte, error) {
	detail, err := CanonicalJSON(r.Detail)
	if err != nil {
		return nil, err
	}
	var actor any
	if r.ActorID != nil {
		actor = strings.ToLower(*r.ActorID)
	}
	return marshal([]any{
		Version, r.Namespace, r.Seq, r.OccurredAt.UTC().Format(TimeLayout), actor,
		r.ActorKind, r.ActorLabel, r.Action, r.TargetKind, r.TargetID, r.Outcome,
		json.RawMessage(detail), r.ClientIP, r.PrevHash,
	})
}

// Hash returns hex(sha256(canonical_v1(row))).
func Hash(r Row) (string, error) {
	c, err := Canonical(r)
	if err != nil {
		return "", err
	}
	h := sha256.Sum256(c)
	return hex.EncodeToString(h[:]), nil
}

// ── Export format (NDJSON, "v": 1) ────────────────────────────────────────

// Header is the first line of an NDJSON export.
type Header struct {
	Type      string `json:"type"` // "header"
	V         int    `json:"v"`
	Encoding  string `json:"encoding"` // Version
	Namespace string `json:"namespace"`
	// Complete is true when the export is unfiltered: every retained chained
	// row is present, so gaps and a missing tail are tampering.
	Complete bool              `json:"complete"`
	Filters  map[string]string `json:"filters,omitempty"`
	// Anchor is where the retained chain starts: the latest purge
	// checkpoint, or the genesis. Nil when the namespace has no chain yet.
	Anchor *Checkpoint `json:"anchor,omitempty"`
	// ChainStartedAt is when this namespace's chain began (TimeLayout). A row
	// without a chain position that is older than this is pre-chain history
	// (written before migration 0033); a newer one is reported as unchained.
	ChainStartedAt string `json:"chain_started_at,omitempty"`
	GeneratedAt    string `json:"generated_at"`
}

// Checkpoint anchors the chain.
type Checkpoint struct {
	Kind string `json:"kind"` // "genesis" | "purge"
	Seq  int64  `json:"seq"`
	Hash string `json:"hash"`
}

// Record is one audit row in an export.
type Record struct {
	Type       string          `json:"type"` // "audit"
	ID         int64           `json:"id"`
	Namespace  string          `json:"namespace"`
	Seq        *int64          `json:"chain_seq"` // nil: written without a chain position
	OccurredAt string          `json:"occurred_at"`
	ActorID    *string         `json:"actor_id"`
	ActorKind  string          `json:"actor_kind"`
	ActorLabel string          `json:"actor_label"`
	Action     string          `json:"action"`
	TargetKind string          `json:"target_kind"`
	TargetID   string          `json:"target_id"`
	Outcome    string          `json:"outcome"`
	Detail     json.RawMessage `json:"detail"`
	ClientIP   string          `json:"client_ip"`
	PrevHash   *string         `json:"prev_hash"`
	RowHash    *string         `json:"row_hash"`
}

// Trailer is the last line of an NDJSON export. Its Head lets a verifier
// detect a truncated tail: the last exported row must be the head.
type Trailer struct {
	Type string      `json:"type"` // "trailer"
	Rows int64       `json:"rows"`
	Head *Checkpoint `json:"head,omitempty"`
}

// RowOf converts an export record to the hashed form.
func (rec Record) RowOf() (Row, error) {
	t, err := time.Parse(TimeLayout, rec.OccurredAt)
	if err != nil {
		if t, err = time.Parse(time.RFC3339Nano, rec.OccurredAt); err != nil {
			return Row{}, fmt.Errorf("occurred_at %q: %w", rec.OccurredAt, err)
		}
	}
	r := Row{
		Namespace: rec.Namespace, OccurredAt: t, ActorID: rec.ActorID, ActorKind: rec.ActorKind,
		ActorLabel: rec.ActorLabel, Action: rec.Action, TargetKind: rec.TargetKind, TargetID: rec.TargetID,
		Outcome: rec.Outcome, Detail: rec.Detail, ClientIP: rec.ClientIP,
	}
	if rec.Seq != nil {
		r.Seq = *rec.Seq
	}
	if rec.PrevHash != nil {
		r.PrevHash = *rec.PrevHash
	}
	return r, nil
}
