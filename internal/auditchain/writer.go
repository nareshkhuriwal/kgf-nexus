// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package auditchain

import (
	"bufio"
	"encoding/csv"
	"encoding/json"
	"io"
	"strconv"
	"strings"
)

// NDJSONWriter streams an export: WriteHeader once, Write per row, Close with
// the trailer. Each line is flushed through the buffered writer; the caller
// flushes the HTTP response as it sees fit.
type NDJSONWriter struct {
	w    *bufio.Writer
	enc  *json.Encoder
	rows int64
}

// NewNDJSONWriter wraps w.
func NewNDJSONWriter(w io.Writer) *NDJSONWriter {
	bw := bufio.NewWriterSize(w, 64<<10)
	enc := json.NewEncoder(bw)
	enc.SetEscapeHTML(false)
	return &NDJSONWriter{w: bw, enc: enc}
}

func (n *NDJSONWriter) WriteHeader(h Header) error {
	h.Type, h.V, h.Encoding = "header", 1, Version
	return n.enc.Encode(h)
}

func (n *NDJSONWriter) Write(r Record) error {
	r.Type = "audit"
	n.rows++
	return n.enc.Encode(r)
}

// Flush pushes buffered lines to the underlying writer.
func (n *NDJSONWriter) Flush() error { return n.w.Flush() }

// Close writes the trailer and flushes.
func (n *NDJSONWriter) Close(head *Checkpoint) error {
	if err := n.enc.Encode(Trailer{Type: "trailer", Rows: n.rows, Head: head}); err != nil {
		return err
	}
	return n.w.Flush()
}

// KGFWriter streams KGF-shaped audit events (ToKGF), one JSON object per
// line, with no header or trailer: the shape a KGF-side collector ingests.
type KGFWriter struct {
	w   *bufio.Writer
	enc *json.Encoder
}

// NewKGFWriter wraps w.
func NewKGFWriter(w io.Writer) *KGFWriter {
	bw := bufio.NewWriterSize(w, 64<<10)
	enc := json.NewEncoder(bw)
	enc.SetEscapeHTML(false)
	return &KGFWriter{w: bw, enc: enc}
}

func (k *KGFWriter) Write(r Record) error { return k.enc.Encode(ToKGF(r)) }

// Flush pushes buffered lines.
func (k *KGFWriter) Flush() error { return k.w.Flush() }

// CSVColumns is the CSV export's header row.
var CSVColumns = []string{"id", "namespace", "chain_seq", "occurred_at", "actor_id", "actor_kind", "actor_label",
	"action", "target_kind", "target_id", "outcome", "detail", "client_ip", "prev_hash", "row_hash",
	"request_id", "kgf_organization", "kgf_subscription", "kgf_resource_group", "kgf_principal", "kgf_on_behalf_of"}

// CSVWriter streams RFC 4180 CSV. CSV is for spreadsheets and SIEM imports,
// not verification: it has no header or trailer lines, and cells are
// neutralised against formula injection, which changes their bytes.
type CSVWriter struct {
	w  *csv.Writer
	bw *bufio.Writer
}

// NewCSVWriter wraps w and writes the column row.
func NewCSVWriter(w io.Writer) (*CSVWriter, error) {
	bw := bufio.NewWriterSize(w, 64<<10)
	c := &CSVWriter{w: csv.NewWriter(bw), bw: bw}
	return c, c.w.Write(CSVColumns)
}

// SafeCell neutralises a cell a spreadsheet would evaluate as a formula (it
// starts with = + - @, or a tab or carriage return) by prefixing a single
// quote -- the OWASP CSV-injection mitigation. Audit fields such as
// actor_label and target_id are attacker-influenced (a user's display name, a
// failed login's email), so this is not hypothetical.
func SafeCell(s string) string {
	if s == "" {
		return s
	}
	switch s[0] {
	case '=', '+', '-', '@', '\t', '\r':
		return "'" + s
	}
	return s
}

func (c *CSVWriter) Write(r Record) error {
	opt := func(p *string) string {
		if p == nil {
			return ""
		}
		return *p
	}
	seq := ""
	if r.Seq != nil {
		seq = strconv.FormatInt(*r.Seq, 10)
	}
	row := []string{strconv.FormatInt(r.ID, 10), r.Namespace, seq, r.OccurredAt, opt(r.ActorID), r.ActorKind,
		r.ActorLabel, r.Action, r.TargetKind, r.TargetID, r.Outcome, strings.TrimSpace(string(r.Detail)),
		r.ClientIP, opt(r.PrevHash), opt(r.RowHash), r.RequestID}
	k := KGF{}
	if r.KGF != nil {
		k = *r.KGF
	}
	row = append(row, k.Organization, k.Subscription, k.ResourceGroup, k.Principal, k.OnBehalfOf)
	for i := range row {
		row[i] = SafeCell(row[i])
	}
	return c.w.Write(row)
}

// Flush pushes buffered rows.
func (c *CSVWriter) Flush() error {
	c.w.Flush()
	if err := c.w.Error(); err != nil {
		return err
	}
	return c.bw.Flush()
}
