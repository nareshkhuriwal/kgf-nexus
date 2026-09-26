// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package auditchain

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"time"
)

// Problem is one verification failure.
type Problem struct {
	Seq  int64  `json:"seq,omitempty"`
	ID   int64  `json:"id,omitempty"`
	Kind string `json:"kind"` // altered | gap | reordered | broken-link | truncated | unchained | foreign | malformed
	Msg  string `json:"message"`
}

func (p Problem) String() string {
	if p.Seq > 0 {
		return fmt.Sprintf("seq %d (id %d): %s: %s", p.Seq, p.ID, p.Kind, p.Msg)
	}
	return fmt.Sprintf("%s: %s", p.Kind, p.Msg)
}

// Result summarises a verification.
type Result struct {
	Namespace string      `json:"namespace"`
	Complete  bool        `json:"complete"`
	Rows      int64       `json:"rows"`
	Chained   int64       `json:"chained"`
	PreChain  int64       `json:"pre_chain"` // written before the chain existed (migration 0033)
	Unchained int64       `json:"unchained"` // written AFTER the chain started without a position
	FirstSeq  int64       `json:"first_seq,omitempty"`
	LastSeq   int64       `json:"last_seq,omitempty"`
	LastHash  string      `json:"last_hash,omitempty"`
	Anchor    *Checkpoint `json:"anchor,omitempty"`
	Problems  []Problem   `json:"problems,omitempty"`
}

// OK reports a clean verification. Unchained rows are problems unless
// allowUnchained (an operator who knows an older gateway replica wrote them
// during a rolling upgrade can accept them explicitly).
func (r Result) OK(allowUnchained bool) bool {
	for _, p := range r.Problems {
		if p.Kind == "unchained" && allowUnchained {
			continue
		}
		return false
	}
	return true
}

// FirstProblem returns the first problem, for the one-line failure message.
func (r Result) FirstProblem(allowUnchained bool) *Problem {
	for i, p := range r.Problems {
		if p.Kind == "unchained" && allowUnchained {
			continue
		}
		return &r.Problems[i]
	}
	return nil
}

// maxProblems bounds the report; the first problem is what matters.
const maxProblems = 1000

// Verify reads an NDJSON export (header, records, trailer) and checks every
// chained row's hash, the chain's continuity from its anchor, and -- for a
// complete export -- that nothing is missing at either end.
//
// It trusts nothing in the file except as input to recomputation: a row's
// row_hash is only accepted if recomputing canonical_v1 over the row's own
// fields yields it, and a link is only accepted if prev_hash equals the
// previous row's recomputed hash.
func Verify(r io.Reader) (Result, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	var res Result
	add := func(p Problem) {
		if len(res.Problems) < maxProblems {
			res.Problems = append(res.Problems, p)
		}
	}
	var hdr *Header
	var trl *Trailer
	var startedAt time.Time
	var prevSeq int64
	var prevHash string
	seen := false
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var probe struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(line, &probe); err != nil {
			return res, fmt.Errorf("line is not JSON: %w", err)
		}
		switch probe.Type {
		case "header":
			if hdr != nil {
				return res, fmt.Errorf("two headers")
			}
			hdr = &Header{}
			if err := json.Unmarshal(line, hdr); err != nil {
				return res, fmt.Errorf("header: %w", err)
			}
			if hdr.Encoding != Version || hdr.V != 1 {
				return res, fmt.Errorf("unsupported export encoding %q v%d", hdr.Encoding, hdr.V)
			}
			res.Namespace, res.Complete, res.Anchor = hdr.Namespace, hdr.Complete, hdr.Anchor
			if hdr.ChainStartedAt != "" {
				startedAt, _ = time.Parse(TimeLayout, hdr.ChainStartedAt)
			}
			if hdr.Anchor != nil {
				prevSeq, prevHash = hdr.Anchor.Seq, hdr.Anchor.Hash
			}
		case "audit":
			if hdr == nil {
				return res, fmt.Errorf("a record before the header")
			}
			if trl != nil {
				add(Problem{Kind: "malformed", Msg: "records after the trailer"})
			}
			var rec Record
			if err := json.Unmarshal(line, &rec); err != nil {
				return res, fmt.Errorf("record: %w", err)
			}
			res.Rows++
			if rec.Namespace != hdr.Namespace {
				add(Problem{ID: rec.ID, Kind: "foreign", Msg: fmt.Sprintf("row of namespace %q in an export of %q", rec.Namespace, hdr.Namespace)})
				continue
			}
			if rec.Seq == nil {
				t, _ := time.Parse(TimeLayout, rec.OccurredAt)
				if !startedAt.IsZero() && t.Before(startedAt) {
					res.PreChain++
				} else {
					res.Unchained++
					add(Problem{ID: rec.ID, Kind: "unchained", Msg: "row written after the chain started has no chain position " +
						"(an older gateway replica during a rolling upgrade, or a direct SQL insert)"})
				}
				continue
			}
			res.Chained++
			seq := *rec.Seq
			row, err := rec.RowOf()
			if err != nil {
				add(Problem{Seq: seq, ID: rec.ID, Kind: "malformed", Msg: err.Error()})
				continue
			}
			row.Seq = seq
			h, err := Hash(row)
			if err != nil {
				add(Problem{Seq: seq, ID: rec.ID, Kind: "malformed", Msg: err.Error()})
				continue
			}
			if rec.RowHash == nil || *rec.RowHash != h {
				add(Problem{Seq: seq, ID: rec.ID, Kind: "altered", Msg: "row content does not match its row_hash"})
			}
			if res.FirstSeq == 0 {
				res.FirstSeq = seq
			}
			switch {
			case seen && seq <= prevSeq:
				add(Problem{Seq: seq, ID: rec.ID, Kind: "reordered", Msg: fmt.Sprintf("follows seq %d", prevSeq)})
			case seen && seq != prevSeq+1:
				if hdr.Complete {
					add(Problem{Seq: seq, ID: rec.ID, Kind: "gap", Msg: fmt.Sprintf("seqs %d..%d are missing", prevSeq+1, seq-1)})
				}
			case !seen && hdr.Anchor != nil && seq != prevSeq+1:
				if hdr.Complete {
					add(Problem{Seq: seq, ID: rec.ID, Kind: "gap", Msg: fmt.Sprintf("chain starts at %d but its anchor is %s seq %d",
						seq, hdr.Anchor.Kind, prevSeq)})
				}
			default:
				// Consecutive with the previous row (or the anchor): the link must hold.
				if (seen || hdr.Anchor != nil) && row.PrevHash != prevHash {
					add(Problem{Seq: seq, ID: rec.ID, Kind: "broken-link", Msg: "prev_hash does not equal the previous row's hash"})
				}
			}
			seen = true
			prevSeq, prevHash = seq, h
			res.LastSeq, res.LastHash = seq, h
		case "trailer":
			trl = &Trailer{}
			if err := json.Unmarshal(line, trl); err != nil {
				return res, fmt.Errorf("trailer: %w", err)
			}
		default:
			return res, fmt.Errorf("unknown line type %q", probe.Type)
		}
	}
	if err := sc.Err(); err != nil {
		return res, err
	}
	if hdr == nil {
		return res, fmt.Errorf("no header: not a Nexus audit export")
	}
	if trl == nil {
		add(Problem{Kind: "truncated", Msg: "no trailer: the export ended early"})
		return res, nil
	}
	if trl.Rows != res.Rows {
		add(Problem{Kind: "truncated", Msg: fmt.Sprintf("trailer counts %d rows, the file has %d", trl.Rows, res.Rows)})
	}
	if hdr.Complete && trl.Head != nil {
		switch {
		case !seen && hdr.Anchor != nil && (trl.Head.Seq != hdr.Anchor.Seq || trl.Head.Hash != hdr.Anchor.Hash):
			add(Problem{Kind: "truncated", Msg: fmt.Sprintf("no chained rows, but the chain head is seq %d", trl.Head.Seq)})
		case seen && (trl.Head.Seq != res.LastSeq || trl.Head.Hash != res.LastHash):
			add(Problem{Seq: trl.Head.Seq, Kind: "truncated", Msg: fmt.Sprintf(
				"the chain head is seq %d but the export ends at seq %d (rows missing at the end, or the last row altered)",
				trl.Head.Seq, res.LastSeq)})
		}
	}
	return res, nil
}
