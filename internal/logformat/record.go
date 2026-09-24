// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

// Package logformat is the on-object format of durable task logs (ADR-0020),
// shared by the agent that writes chunks and the gateway that reads them.
//
// It lives outside both so neither side can drift: a chunk written by one
// release's agent must be readable by a later release's gateway, and the
// record struct below IS that compatibility surface. Fields may be added; none
// may be renamed or given a new meaning.
package logformat

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"
)

// Stream tags. A record carries exactly one.
const (
	StreamStdout = "stdout"
	StreamStderr = "stderr"
	// StreamCombined is used when the substrate interleaves stdout and stderr
	// before the agent sees them (Kubernetes pod logs).
	StreamCombined = "combined"
	// StreamNexus marks the platform's OWN lines: truncation and drop markers.
	// Kept distinct so a task cannot forge one by printing the same text.
	StreamNexus = "nexus"
)

// Meta kinds for StreamNexus records.
const (
	KindTruncated         = "truncated"
	KindTruncationSummary = "truncation_summary"
	KindDropped           = "dropped"
	// KindGap is synthesized by the READER, never written: a chunk the index
	// names could not be read, and these lines stand in for its range.
	KindGap = "gap"
)

// Record is one log line as stored in a chunk object, one JSON document per
// line (NDJSON). Field names are short because they repeat on every line of
// every log, and stable because objects outlive agent versions: the gateway of
// a later release must still read a chunk written today.
type Record struct {
	// N is the line number, contiguous from 1 per attempt. It is the cursor for
	// pagination and for SSE resume (Last-Event-ID), so it must never repeat or
	// skip within an attempt.
	N int64 `json:"n"`
	// TS is the agent's clock in unix milliseconds. Informational: worker
	// clocks are never authoritative (invariant 7), so nothing orders by it.
	TS int64  `json:"ts"`
	S  string `json:"s"`
	M  string `json:"m"`
	// P marks a PARTIAL line: split because it exceeded the line cap, or
	// flushed at the end with no trailing newline.
	P bool `json:"p,omitempty"`
	// K is the meta kind of a StreamNexus record.
	K string `json:"k,omitempty"`
}

// Encode renders the record as one NDJSON line including its newline.
//
// json.Marshal escapes control characters and invalid UTF-8 is replaced, so a
// task that prints binary garbage produces a valid (if ugly) document rather
// than a chunk the reader cannot parse.
func (r Record) Encode() []byte {
	if !utf8.ValidString(r.M) {
		r.M = strings.ToValidUTF8(r.M, "�")
	}
	b, err := json.Marshal(r)
	if err != nil {
		// Unreachable for this struct; kept honest rather than panicking.
		b = []byte(fmt.Sprintf(`{"n":%d,"ts":%d,"s":"nexus","m":"unencodable log line","k":"error"}`, r.N, r.TS))
	}
	return append(b, '\n')
}

// ContentType is set on every log object.
const ContentType = "application/x-ndjson"

// DecodeRecords parses a chunk (or a range of the compacted object) back into
// records. A malformed line is skipped rather than failing the whole read:
// one damaged line must not hide every other line of a log that is evidence.
func DecodeRecords(b []byte) ([]Record, int) {
	var out []Record
	bad := 0
	for len(b) > 0 {
		line := b
		if i := bytes.IndexByte(b, '\n'); i >= 0 {
			line, b = b[:i], b[i+1:]
		} else {
			b = nil
		}
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var r Record
		if err := json.Unmarshal(line, &r); err != nil || r.N < 1 {
			bad++
			continue
		}
		out = append(out, r)
	}
	return out, bad
}

// SplitUTF8 cuts b at most max bytes in, backing off to a rune boundary so a
// split long line never produces two halves of one multi-byte character.
func SplitUTF8(b []byte, max int) (head, rest []byte) {
	if len(b) <= max {
		return b, nil
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(b[cut]) {
		cut--
	}
	if cut == 0 {
		cut = max
	}
	return b[:cut], b[cut:]
}
