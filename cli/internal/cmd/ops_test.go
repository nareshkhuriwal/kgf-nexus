// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// route answers by "METHOD path"; unknown routes get 404 so a command that
// reaches the wrong URL fails its test instead of passing on the default {}.
type route struct {
	status int
	body   string
}

func (h *harness) routes(m map[string]route) {
	h.handler = func(w http.ResponseWriter, r *http.Request) bool {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Request-Id", "req-test")
		rt, ok := m[r.Method+" "+r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":"not_found","message":"no such route in test: `+r.Method+" "+r.URL.Path+`"}`)
			return true
		}
		if rt.status == 0 {
			rt.status = http.StatusOK
		}
		w.WriteHeader(rt.status)
		_, _ = io.WriteString(w, rt.body)
		return true
	}
}

// sent returns the requests with this method and path.
func (h *harness) sent(method, path string) []recorded {
	var out []recorded
	for _, r := range h.requests {
		if r.Method == method && r.Path == path {
			out = append(out, r)
		}
	}
	return out
}

func bodyOf(t *testing.T, r recorded) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(r.Body, &m); err != nil {
		t.Fatalf("request body is not JSON: %q", r.Body)
	}
	return m
}

// withTerminal makes stdin look interactive and feeds it answer.
func withTerminal(t *testing.T, h *harness, answer string) func(args ...string) (int, string, string) {
	t.Helper()
	prev := stdinIsTerminal
	stdinIsTerminal = func(io.Reader) bool { return true }
	t.Cleanup(func() { stdinIsTerminal = prev })
	return func(args ...string) (int, string, string) {
		var out, errb strings.Builder
		code := Execute(args, IO{Out: &out, Err: &errb, In: strings.NewReader(answer)},
			func(k string) string { return h.env[k] })
		return code, out.String(), errb.String()
	}
}

const ns = "/v1/namespaces/default"

// ── run pause / resume ───────────────────────────────────────────────────────

func TestRunPauseAndResume(t *testing.T) {
	h := newHarness(t)
	h.routes(map[string]route{
		"POST " + ns + "/runs/r-1/pause":  {body: `{"id":"r-1","state":"RUN_STATE_PAUSED"}`},
		"POST " + ns + "/runs/r-1/resume": {body: `{"id":"r-1","state":"RUN_STATE_RUNNING"}`},
	})
	code, out, errb := h.run("run", "pause", "r-1")
	if code != 0 || !strings.Contains(out, "Paused run r-1 (state: paused)") || !strings.Contains(out, "run resume r-1") {
		t.Fatalf("pause: code %d out %q err %q", code, out, errb)
	}
	if string(h.last().Body) != "{}" {
		t.Errorf("lifecycle POST should carry an empty object, got %q", h.last().Body)
	}
	code, out, _ = h.run("run", "resume", "r-1", "-o", "json")
	if code != 0 || !strings.Contains(out, `"state": "RUN_STATE_RUNNING"`) {
		t.Fatalf("resume json: code %d out %q", code, out)
	}
}

func TestRunPauseErrorsMapToExitCodes(t *testing.T) {
	h := newHarness(t)
	h.routes(map[string]route{
		"POST " + ns + "/runs/done/pause": {status: 409, body: `{"error":"run_state_conflict","message":"run is SUCCEEDED"}`},
	})
	code, _, errb := h.run("run", "pause", "done")
	if code != ExitConflict || !strings.Contains(errb, "run is SUCCEEDED") || strings.Contains(errb, "idempotency") {
		t.Errorf("409: code %d err %q", code, errb)
	}
	code, _, _ = h.run("run", "pause", "missing")
	if code != ExitNotFound {
		t.Errorf("404 should exit %d, got %d", ExitNotFound, code)
	}
	n := len(h.requests)
	code, _, errb = h.run("run", "resume")
	if code != ExitUsage || !strings.Contains(errb, "Usage:") || len(h.requests) != n {
		t.Errorf("missing operand: code %d err %q, requests %d->%d", code, errb, n, len(h.requests))
	}
}

// ── schedules ────────────────────────────────────────────────────────────────

const schedJSON = `{"name":"nightly","spec_name":"etl","cron":"0 2 * * *","timezone":"Europe/London",
  "params":{"source":"s3://a","keep":1},"priority":3,"overlap_policy":"skip","catchup_limit":2,
  "enabled":true,"spec_version":4,"next_fire_at":"2026-09-26T01:00:00Z","last_fire_at":null,
  "next_fires":["2026-09-26T02:00:00+01:00","2026-09-27T02:00:00+01:00"]}`

func TestScheduleListAndGet(t *testing.T) {
	h := newHarness(t)
	h.routes(map[string]route{
		"GET " + ns + "/schedules":         {body: `{"schedules":[` + schedJSON + `]}`},
		"GET " + ns + "/schedules/nightly": {body: schedJSON},
	})
	code, out, _ := h.run("schedule", "list", "--limit", "7")
	if code != 0 || !strings.Contains(out, "nightly") || !strings.Contains(out, "Europe/London") {
		t.Fatalf("list: %d %q", code, out)
	}
	if h.last().Query != "limit=7" {
		t.Errorf("list query = %q", h.last().Query)
	}
	code, out, _ = h.run("schedule", "get", "nightly", "--count", "2")
	for _, want := range []string{"Schedule:   nightly", "pinned to version 4", "0 2 * * * (Europe/London)",
		"2026-09-27T02:00:00+01:00", `"source":"s3://a"`} {
		if !strings.Contains(out, want) {
			t.Errorf("get output missing %q:\n%s", want, out)
		}
	}
	if code != 0 || h.last().Query != "count=2" {
		t.Errorf("get: code %d query %q", code, h.last().Query)
	}
}

func TestScheduleListEmptyIsExplained(t *testing.T) {
	h := newHarness(t)
	h.routes(map[string]route{"GET " + ns + "/schedules": {body: `{"schedules":[]}`}})
	_, out, _ := h.run("schedule", "list")
	if !strings.Contains(out, "No schedules") || !strings.Contains(out, "schedule create") {
		t.Errorf("empty list should say so and how to create one: %q", out)
	}
}

func TestScheduleCreateSendsTheGatewayBody(t *testing.T) {
	h := newHarness(t)
	h.routes(map[string]route{"POST " + ns + "/schedules": {status: 201, body: schedJSON}})
	code, out, errb := h.run("schedule", "create", "nightly", "--workflow", "etl", "--cron", "0 2 * * *",
		"--timezone", "Europe/London", "--param", "keep=1", "--param-string", "source=s3://a",
		"--spec-version", "4", "--overlap", "queue_one", "--catchup", "2", "--priority", "3")
	if code != 0 {
		t.Fatalf("create: %d %s", code, errb)
	}
	b := bodyOf(t, h.last())
	want := map[string]any{"name": "nightly", "spec_name": "etl", "cron": "0 2 * * *", "timezone": "Europe/London",
		"spec_version": 4.0, "overlap_policy": "queue_one", "catchup_limit": 2.0, "priority": 3.0, "enabled": true}
	for k, v := range want {
		if b[k] != v {
			t.Errorf("body[%s] = %v, want %v", k, b[k], v)
		}
	}
	params := b["params"].(map[string]any)
	if params["keep"] != 1.0 || params["source"] != "s3://a" {
		t.Errorf("params = %v", params)
	}
	if !strings.Contains(out, "Created schedule nightly") || !strings.Contains(out, "Upcoming:") {
		t.Errorf("output: %q", out)
	}

	_, _, _ = h.run("schedule", "create", "off", "--workflow", "etl", "--cron", "* * * * *", "--disabled")
	if bodyOf(t, h.last())["enabled"] != false {
		t.Errorf("--disabled must send enabled=false")
	}
}

func TestScheduleCreateRejectsBadInputLocally(t *testing.T) {
	h := newHarness(t)
	for _, args := range [][]string{
		{"schedule", "create", "x", "--cron", "* * * * *"},                              // no workflow
		{"schedule", "create", "x", "--workflow", "w"},                                  // no cron
		{"schedule", "create", "--workflow", "w", "--cron", "* * * * *"},                // no name
		{"schedule", "create", "x", "--workflow", "w", "--cron", "c", "--overlap", "x"}, // bad policy
		{"schedule", "create", "x", "--workflow", "w", "--cron", "c", "--catchup", "-1"},
		{"schedule", "create", "x", "--workflow", "w", "--cron", "c", "--param", "novalue"},
		{"schedule", "create", "x", "--workflow", "w", "--cron", "c", "--no-such-flag"},
	} {
		code, _, errb := h.run(args...)
		if code != ExitUsage {
			t.Errorf("%v: exit %d (want %d): %s", args, code, ExitUsage, errb)
		}
	}
	if len(h.requests) != 0 {
		t.Errorf("invalid input reached the server %d times", len(h.requests))
	}
}

func TestScheduleCreateServerErrors(t *testing.T) {
	h := newHarness(t)
	h.routes(map[string]route{"POST " + ns + "/schedules": {status: 409,
		body: `{"error":"schedule_exists","message":"a schedule with this name already exists in this namespace"}`}})
	code, _, errb := h.run("schedule", "create", "x", "--workflow", "w", "--cron", "* * * * *")
	if code != ExitConflict || !strings.Contains(errb, "already exists") || !strings.Contains(errb, "req-test") {
		t.Errorf("409: %d %q", code, errb)
	}
	h.routes(map[string]route{"POST " + ns + "/schedules": {status: 400,
		body: `{"error":"invalid_cron","message":"cron field 2: bad token \"x\""}`}})
	code, _, errb = h.run("schedule", "create", "x", "--workflow", "w", "--cron", "0 x * * *")
	if code != ExitError || !strings.Contains(errb, `bad token`) || strings.Contains(errb, "--dry-run") {
		t.Errorf("400: %d %q", code, errb)
	}
}

func TestScheduleUpdateMergesOntoTheCurrentSchedule(t *testing.T) {
	h := newHarness(t)
	h.routes(map[string]route{
		"GET " + ns + "/schedules/nightly": {body: schedJSON},
		"PUT " + ns + "/schedules/nightly": {body: schedJSON},
	})
	code, _, errb := h.run("schedule", "update", "nightly", "--cron", "0 3 * * *",
		"--param", "extra=true", "--unset-param", "keep", "--spec-version", "0")
	if code != 0 {
		t.Fatalf("update: %d %s", code, errb)
	}
	puts := h.sent("PUT", ns+"/schedules/nightly")
	if len(puts) != 1 {
		t.Fatalf("want one PUT, got %d", len(puts))
	}
	b := bodyOf(t, puts[0])
	if b["cron"] != "0 3 * * *" || b["spec_name"] != "etl" || b["timezone"] != "Europe/London" ||
		b["overlap_policy"] != "skip" || b["catchup_limit"] != 2.0 || b["priority"] != 3.0 {
		t.Errorf("unchanged fields were not kept: %v", b)
	}
	// Enabled is REQUIRED by the gateway on update and must be preserved.
	if b["enabled"] != true {
		t.Errorf("enabled not preserved: %v", b["enabled"])
	}
	if _, pinned := b["spec_version"]; pinned {
		t.Errorf("--spec-version 0 should unpin, body still has %v", b["spec_version"])
	}
	params := b["params"].(map[string]any)
	if _, has := params["keep"]; has || params["extra"] != true || params["source"] != "s3://a" {
		t.Errorf("params merge wrong: %v", params)
	}
}

func TestScheduleUpdateNeedsAChangeAndAKnownParam(t *testing.T) {
	h := newHarness(t)
	h.routes(map[string]route{"GET " + ns + "/schedules/nightly": {body: schedJSON}})
	if code, _, errb := h.run("schedule", "update", "nightly"); code != ExitUsage || !strings.Contains(errb, "nothing to update") {
		t.Errorf("no flags: %d %q", code, errb)
	}
	code, _, errb := h.run("schedule", "update", "nightly", "--unset-param", "ghost")
	if code != ExitUsage || !strings.Contains(errb, "ghost") {
		t.Errorf("unknown param: %d %q", code, errb)
	}
	if len(h.sent("PUT", ns+"/schedules/nightly")) != 0 {
		t.Error("a refused update must not PUT")
	}
	code, _, _ = h.run("schedule", "update", "missing", "--cron", "* * * * *")
	if code != ExitNotFound {
		t.Errorf("unknown schedule: exit %d", code)
	}
}

func TestScheduleDeleteConfirmation(t *testing.T) {
	h := newHarness(t)
	h.routes(map[string]route{"DELETE " + ns + "/schedules/nightly": {status: 204}})

	// No terminal and no --yes: refuse, send nothing.
	code, _, errb := h.run("schedule", "delete", "nightly")
	if code != ExitAborted || !strings.Contains(errb, "--yes") || len(h.requests) != 0 {
		t.Fatalf("non-interactive without --yes: %d %q, %d requests", code, errb, len(h.requests))
	}

	// Interactive, declined.
	run := withTerminal(t, h, "n\n")
	code, _, errb = run("schedule", "delete", "nightly")
	if code != ExitAborted || !strings.Contains(errb, "Continue? [y/N]") || len(h.requests) != 0 {
		t.Fatalf("declined: %d %q", code, errb)
	}
	// Interactive, confirmed.
	run = withTerminal(t, h, "yes\n")
	code, out, _ := run("schedule", "delete", "nightly")
	if code != 0 || !strings.Contains(out, "Deleted schedule nightly") {
		t.Fatalf("confirmed: %d %q", code, out)
	}
	// --yes, JSON.
	code, out, _ = h.run("schedule", "delete", "nightly", "--yes", "-o", "json")
	if code != 0 || !strings.Contains(out, `"deleted": true`) {
		t.Fatalf("--yes json: %d %q", code, out)
	}
	if n := len(h.sent("DELETE", ns+"/schedules/nightly")); n != 2 {
		t.Errorf("want 2 DELETEs, got %d", n)
	}
}

func TestScheduleEnableDisable(t *testing.T) {
	h := newHarness(t)
	h.routes(map[string]route{
		"POST " + ns + "/schedules/nightly/enable":  {body: schedJSON},
		"POST " + ns + "/schedules/nightly/disable": {body: `{"name":"nightly","enabled":false}`},
	})
	code, out, _ := h.run("schedule", "enable", "nightly")
	if code != 0 || !strings.Contains(out, "next fire at 2026-09-26T01:00:00Z") {
		t.Errorf("enable: %d %q", code, out)
	}
	code, out, _ = h.run("schedule", "disable", "nightly")
	if code != 0 || !strings.Contains(out, "Disabled schedule nightly") {
		t.Errorf("disable: %d %q", code, out)
	}
}

func TestSchedulePreview(t *testing.T) {
	h := newHarness(t)
	h.routes(map[string]route{"GET " + ns + "/schedules/preview": {body: `{"cron":"0 2 * * *","valid":true,
		"timezone":"Europe/London","next_fires":["2026-10-25T02:00:00+01:00","2026-10-26T02:00:00Z"]}`}})
	code, out, _ := h.run("schedule", "preview", "0 2 * * *", "--timezone", "Europe/London", "--count", "2")
	if code != 0 || !strings.Contains(out, "2026-10-26T02:00:00Z") {
		t.Fatalf("preview: %d %q", code, out)
	}
	q := h.last().Query
	for _, want := range []string{"cron=0+2+%2A+%2A+%2A", "timezone=Europe%2FLondon", "count=2"} {
		if !strings.Contains(q, want) {
			t.Errorf("query %q missing %q", q, want)
		}
	}

	h.routes(map[string]route{"GET " + ns + "/schedules/preview": {body: `{"cron":"61 * * * *","valid":false,
		"timezone":"UTC","error":"minute 61 out of range"}`}})
	code, _, errb := h.run("schedule", "preview", "61 * * * *")
	if code != ExitError || !strings.Contains(errb, "minute 61 out of range") {
		t.Errorf("invalid: %d %q", code, errb)
	}
	// JSON still prints the answer, and still exits non-zero.
	code, out, _ = h.run("schedule", "preview", "--cron", "61 * * * *", "-o", "json")
	if code != ExitError || !strings.Contains(out, `"valid": false`) {
		t.Errorf("invalid json: %d %q", code, out)
	}
	if code, _, _ := h.run("schedule", "preview"); code != ExitUsage {
		t.Errorf("no expression: exit %d", code)
	}
}

// ── backfills ────────────────────────────────────────────────────────────────

const bfJSON = `{"id":"bf-1","schedule_name":"nightly","state":"%s","from":"2026-09-01T00:00:00Z",
  "to":"2026-09-08T00:00:00Z","cursor_at":"2026-09-03T02:00:00Z","max_active":2,"max_runs":1000,"runs_created":2,
  "created_at":"2026-09-25T10:00:00Z","created_by":"alice"}`

func bf(state string) string { return strings.Replace(bfJSON, "%s", state, 1) }

func TestBackfillStart(t *testing.T) {
	h := newHarness(t)
	h.routes(map[string]route{"POST " + ns + "/schedules/nightly/backfills": {status: 201, body: bf("RUNNING")}})
	code, out, errb := h.run("backfill", "start", "nightly", "--from", "2026-09-01", "--to", "2026-09-08T00:00:00+02:00",
		"--max-active", "2")
	if code != 0 {
		t.Fatalf("start: %d %s", code, errb)
	}
	b := bodyOf(t, h.last())
	if b["from"] != "2026-09-01T00:00:00Z" || b["to"] != "2026-09-08T00:00:00+02:00" || b["max_active"] != 2.0 {
		t.Errorf("body: %v", b)
	}
	if _, has := b["max_runs"]; has {
		t.Errorf("max_runs should be omitted to take the server default: %v", b)
	}
	if !strings.Contains(out, "backfill get bf-1") {
		t.Errorf("output should point at backfill get: %q", out)
	}
}

func TestBackfillStartValidatesLocally(t *testing.T) {
	h := newHarness(t)
	for _, args := range [][]string{
		{"backfill", "start", "s", "--to", "2026-09-02"},
		{"backfill", "start", "s", "--from", "yesterday", "--to", "2026-09-02"},
		{"backfill", "start", "s", "--from", "2026-09-02", "--to", "2026-09-01"},
		{"backfill", "start", "s", "--from", "2026-09-01", "--to", "2026-09-02", "--max-active", "51"},
		{"backfill", "start", "--from", "2026-09-01", "--to", "2026-09-02"},
	} {
		if code, _, errb := h.run(args...); code != ExitUsage {
			t.Errorf("%v: exit %d: %s", args, code, errb)
		}
	}
	if len(h.requests) != 0 {
		t.Errorf("invalid backfills reached the server")
	}
}

func TestBackfillListAndGet(t *testing.T) {
	h := newHarness(t)
	h.routes(map[string]route{
		"GET " + ns + "/schedules/nightly/backfills": {body: `{"backfills":[` + bf("PAUSED") + `]}`},
		"GET " + ns + "/backfills/bf-1":              {body: bf("RUNNING")},
	})
	code, out, _ := h.run("backfill", "list", "nightly")
	if code != 0 || !strings.Contains(out, "bf-1") || !strings.Contains(out, "PAUSED") || !strings.Contains(out, "2/1000") {
		t.Errorf("list: %d %q", code, out)
	}
	code, out, _ = h.run("backfill", "get", "bf-1")
	if code != 0 || !strings.Contains(out, "[2026-09-01T00:00:00Z, 2026-09-08T00:00:00Z)") ||
		!strings.Contains(out, "2 of at most 1000, 2 at a time") {
		t.Errorf("get: %d %q", code, out)
	}
	if code, _, _ := h.run("backfill", "get", "nope"); code != ExitNotFound {
		t.Errorf("unknown backfill: exit %d", code)
	}
}

func TestBackfillActionsCheckTheTransition(t *testing.T) {
	cases := []struct {
		action, state string
		allowed       bool
	}{
		{"pause", "RUNNING", true},
		{"pause", "PAUSED", false},
		{"resume", "PAUSED", true},
		{"resume", "COMPLETED", false}, // an older server would revive it (F119)
		{"resume", "CANCELLED", false},
		{"cancel", "RUNNING", true},
		{"cancel", "PAUSED", true},
		{"cancel", "COMPLETED", false},
	}
	for _, c := range cases {
		h := newHarness(t)
		h.routes(map[string]route{
			"GET " + ns + "/backfills/bf-1":              {body: bf(c.state)},
			"POST " + ns + "/backfills/bf-1/" + c.action: {body: bf("DONE")},
		})
		args := []string{"backfill", c.action, "bf-1"}
		if c.action == "cancel" {
			args = append(args, "--yes")
		}
		code, _, errb := h.run(args...)
		posted := len(h.sent("POST", ns+"/backfills/bf-1/"+c.action)) > 0
		if c.allowed && (code != 0 || !posted) {
			t.Errorf("%s from %s: want success, got %d %q", c.action, c.state, code, errb)
		}
		if !c.allowed && (code != ExitConflict || posted || !strings.Contains(errb, c.state)) {
			t.Errorf("%s from %s: want refusal without a POST, got %d posted=%v %q", c.action, c.state, code, posted, errb)
		}
	}
}

func TestBackfillCancelNeedsConfirmation(t *testing.T) {
	h := newHarness(t)
	h.routes(map[string]route{"GET " + ns + "/backfills/bf-1": {body: bf("RUNNING")}})
	code, _, errb := h.run("backfill", "cancel", "bf-1")
	if code != ExitAborted || !strings.Contains(errb, "--yes") || len(h.sent("POST", ns+"/backfills/bf-1/cancel")) != 0 {
		t.Errorf("cancel without --yes: %d %q", code, errb)
	}
}

// ── workers ──────────────────────────────────────────────────────────────────

func TestWorkerDrain(t *testing.T) {
	h := newHarness(t)
	h.routes(map[string]route{"POST " + ns + "/workers/w-1/drain": {status: 202,
		body: `{"worker_id":"w-1","state":"DRAINING"}`}})
	if code, _, _ := h.run("worker", "drain", "w-1"); code != ExitAborted {
		t.Errorf("drain without --yes: exit %d", code)
	}
	code, out, _ := h.run("worker", "drain", "w-1", "--yes")
	if code != 0 || !strings.Contains(out, "DRAINING") || !strings.Contains(out, "heartbeat") {
		t.Errorf("drain: %d %q", code, out)
	}
	if code, _, _ := h.run("worker", "drain", "ghost", "--yes"); code != ExitNotFound {
		t.Errorf("unknown worker: exit %d", code)
	}
}

func TestWorkerQuarantineAndRelease(t *testing.T) {
	h := newHarness(t)
	h.routes(map[string]route{
		"POST " + ns + "/workers/w-1/quarantine": {body: `{"worker_id":"w-1","state":"QUARANTINED","duration_ms":21600000}`},
		"POST " + ns + "/workers/w-1/release":    {body: `{"worker_id":"w-1","state":"ACTIVE"}`},
		"POST " + ns + "/workers/w-2/quarantine": {status: 409, body: `{"error":"not_active","message":"only an ACTIVE worker can be quarantined"}`},
	})
	if code, _, errb := h.run("worker", "quarantine", "w-1", "--yes"); code != ExitUsage || !strings.Contains(errb, "--reason") {
		t.Errorf("no reason: %d %q", code, errb)
	}
	if code, _, _ := h.run("worker", "quarantine", "w-1", "--reason", "x", "--for", "800h", "--yes"); code != ExitUsage {
		t.Errorf("over 30 days: exit %d", code)
	}
	if len(h.requests) != 0 {
		t.Fatal("invalid quarantines reached the server")
	}
	code, out, errb := h.run("worker", "quarantine", "w-1", "--reason", "flaky disk", "--for", "6h", "--yes")
	if code != 0 || !strings.Contains(out, "QUARANTINED for 6h0m0s") {
		t.Fatalf("quarantine: %d %q %q", code, out, errb)
	}
	b := bodyOf(t, h.last())
	if b["reason"] != "flaky disk" || b["duration_ms"] != 21600000.0 {
		t.Errorf("body: %v", b)
	}
	code, _, errb = h.run("worker", "quarantine", "w-2", "--reason", "x", "--yes")
	if code != ExitConflict || !strings.Contains(errb, "only an ACTIVE worker") {
		t.Errorf("not active: %d %q", code, errb)
	}
	code, out, _ = h.run("worker", "release", "w-1", "--reason", "disk replaced")
	if code != 0 || !strings.Contains(out, "released (state: ACTIVE)") || bodyOf(t, h.last())["reason"] != "disk replaced" {
		t.Errorf("release: %d %q", code, out)
	}
}

// ── workflow delete ──────────────────────────────────────────────────────────

func TestWorkflowDelete(t *testing.T) {
	h := newHarness(t)
	h.routes(map[string]route{
		"DELETE " + ns + "/specs/etl": {body: `{"namespace":"default","name":"etl","versions_deleted":3,"deleted":true}`},
		"DELETE " + ns + "/specs/busy": {status: 409, body: `{"error":"workflow_referenced","message":"workflow is referenced",
			"referencing_runs":4,"referencing_schedules":["nightly","hourly"]}`},
	})
	if code, _, _ := h.run("workflow", "delete", "etl"); code != ExitAborted || len(h.requests) != 0 {
		t.Fatalf("delete without --yes must not send anything")
	}
	code, out, _ := h.run("workflow", "delete", "etl", "--yes")
	if code != 0 || !strings.Contains(out, "Deleted workflow etl (3 versions)") {
		t.Errorf("delete: %d %q", code, out)
	}
	code, _, errb := h.run("workflow", "delete", "busy", "--yes")
	for _, want := range []string{"referencing runs: 4", "referencing schedules: nightly, hourly", "then retry"} {
		if !strings.Contains(errb, want) {
			t.Errorf("refusal should say %q:\n%s", want, errb)
		}
	}
	if code != ExitConflict {
		t.Errorf("referenced: exit %d", code)
	}
}

// ── cache ────────────────────────────────────────────────────────────────────

func TestCacheInvalidate(t *testing.T) {
	h := newHarness(t)
	h.routes(map[string]route{"POST " + ns + "/cache/invalidations": {body: `{"invalidation_id":17,"invalidated_at":"2026-09-25T10:00:00Z"}`}})
	for _, args := range [][]string{
		{"cache", "invalidate", "--yes"},
		{"cache", "invalidate", "--task-version", "2", "--cache-key", "k", "--yes"},
		{"cache", "invalidate", "etl.transform", "--yes"},
	} {
		if code, _, _ := h.run(args...); code != ExitUsage {
			t.Errorf("%v: exit %d", args, code)
		}
	}
	if code, _, _ := h.run("cache", "invalidate", "--task-ref", "etl.transform"); code != ExitAborted {
		t.Errorf("without --yes: exit %d", code)
	}
	if len(h.requests) != 0 {
		t.Fatal("refused invalidations reached the server")
	}
	code, out, _ := h.run("cache", "invalidate", "--task-ref", "etl.transform", "--task-version", "2",
		"--reason", "upstream fix", "--yes")
	if code != 0 || !strings.Contains(out, "every cached result of etl.transform version 2") || !strings.Contains(out, "17") {
		t.Errorf("invalidate: %d %q", code, out)
	}
	b := bodyOf(t, h.last())
	if b["task_ref"] != "etl.transform" || b["task_version"] != 2.0 || b["reason"] != "upstream fix" {
		t.Errorf("body: %v", b)
	}
	if _, has := b["cache_key"]; has {
		t.Errorf("empty cache_key should be omitted: %v", b)
	}
}

// ── exit codes ───────────────────────────────────────────────────────────────

func TestExitCodesForAuthAndUnreachable(t *testing.T) {
	h := newHarness(t)
	h.replyJSON(401, `{"message":"invalid api key"}`)
	if code, _, errb := h.run("schedule", "list"); code != ExitAuth || !strings.Contains(errb, "NEXUS_API_KEY") {
		t.Errorf("401: %d %q", code, errb)
	}
	h.replyJSON(503, `{"message":"starting"}`)
	if code, _, _ := h.run("schedule", "list"); code != ExitUnavailable {
		t.Errorf("503: exit %d", code)
	}
	h.env["NEXUS_ENDPOINT"] = "http://127.0.0.1:1"
	if code, _, errb := h.run("schedule", "list"); code != ExitUnavailable || !strings.Contains(errb, "cannot reach") {
		t.Errorf("unreachable: %d %q", code, errb)
	}
}

func TestUnknownVerbIsAUsageError(t *testing.T) {
	h := newHarness(t)
	code, _, errb := h.run("schedule", "creat")
	if code != ExitUsage || !strings.Contains(errb, "preview") {
		t.Errorf("unknown verb: %d %q", code, errb)
	}
}

// ── validation problems ──────────────────────────────────────────────────────

func TestSubmitShowsTheValidatorsProblems(t *testing.T) {
	path := writeSpec(t, "api_version: v1\nname: w\ntasks:\n  - {id: a, task_ref: r}\n")
	h := newHarness(t)
	h.replyJSON(422, `{"error":"spec_invalid","message":"spec failed validation","problems":[
	  {"path":"/tasks/0/resources","message":"cpu required","stage":"schema"},
	  {"path":"/edges/0","message":"cycle a -> a","stage":"semantic"}]}`)
	code, _, errb := h.run("workflow", "submit", path)
	if code != ExitError {
		t.Errorf("exit %d", code)
	}
	// Each problem is placed at its line in the submitted file (F117), with
	// the JSON pointer kept in parentheses.
	for _, want := range []string{"cpu required (/tasks/0/resources)", "cycle a -> a (/edges/0)", "--dry-run"} {
		if !strings.Contains(errb, want) {
			t.Errorf("missing %q in:\n%s", want, errb)
		}
	}
}
