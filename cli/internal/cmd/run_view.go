// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"encoding/json"
	"time"

	"github.com/khuriwalgroup/kgf-nexus/cli/internal/output"
)

// Helpers that read the gateway's run, attempt and event documents for the
// human tables. Field names are the gateway's (api/openapi.yaml): a run's
// nodes carry state and attempts_used, its attempts carry id, worker and
// timing, and an event carries kind, occurred_at and payload.

// latestAttempts indexes attempts by node id, keeping the highest
// attempt_number for each.
func latestAttempts(attempts []map[string]any) map[string]map[string]any {
	out := map[string]map[string]any{}
	for _, a := range attempts {
		id := output.Str(a, "node_id")
		if prev, ok := out[id]; !ok || output.Int(a, "attempt_number") >= output.Int(prev, "attempt_number") {
			out[id] = a
		}
	}
	return out
}

// attemptError renders an error object ({class, message}) or string for a
// table cell.
func attemptError(m map[string]any) string {
	if e, ok := m["error"].(map[string]any); ok {
		text := truncateCell(output.Str(e, "message"), 48)
		if cls := output.Str(e, "class"); cls != "-" {
			text = shortEnum(cls, "ERROR_CLASS_") + ": " + text
		}
		return text
	}
	if s := output.Str(m, "error"); s != "-" {
		return truncateCell(s, 48)
	}
	return "-"
}

// spanBetween formats the time between two RFC 3339 fields, or "" when
// either is missing or unparsable.
func spanBetween(m map[string]any, from, to string) string {
	a, errA := time.Parse(time.RFC3339Nano, output.Str(m, from))
	b, errB := time.Parse(time.RFC3339Nano, output.Str(m, to))
	if errA != nil || errB != nil || b.Before(a) {
		return ""
	}
	return output.Duration(b.Sub(a))
}

// eventTime is an event's timestamp as the table shows it: the gateway's
// occurred_at, with the older "at" accepted.
func eventTime(ev map[string]any) string {
	raw := firstStr(ev, "occurred_at", "at")
	if t, err := time.Parse(time.RFC3339Nano, raw); err == nil {
		return t.UTC().Format("15:04:05.000")
	}
	return raw
}

// eventPayloadDetail summarizes an event's payload when no message field
// exists: compact JSON, truncated.
func eventPayloadDetail(ev map[string]any) string {
	p, ok := ev["payload"].(map[string]any)
	if !ok || len(p) == 0 {
		return "-"
	}
	if r, ok := p["redacted"].(bool); ok && r && len(p) == 1 {
		return "(redacted)"
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return "-"
	}
	return truncateCell(string(raw), 80)
}

// runDuration is a run's duration_ms when the gateway sends one, otherwise
// the span from started_at to ended_at, otherwise "-".
func runDuration(r map[string]any) string {
	if ms := output.Int(r, "duration_ms"); ms > 0 {
		return output.Millis(ms)
	}
	if d := spanBetween(r, "started_at", "ended_at"); d != "" {
		return d
	}
	return "-"
}
