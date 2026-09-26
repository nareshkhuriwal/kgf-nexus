// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"slices"
	"strings"
	"testing"

	"github.com/khuriwalgroup/kgf-nexus/internal/notify"
)

func TestTriggerCreateSendsMappingAndPrintsTheSecretOnce(t *testing.T) {
	h := newHarness(t)
	h.routes(map[string]route{"POST " + ns + "/triggers": {status: 201,
		body: `{"name":"gh","id":"t-1","workflow":"etl","path":"/v1/hooks/default/t-1","secret":"s3cr3t"}`}})
	code, out, errb := h.run("trigger", "create", "gh", "--workflow", "etl",
		"--map", "repo=$.payload.repository.name", "--map-json", "n=3", "--idempotency-key-path", "$.payload.id")
	if code != 0 {
		t.Fatalf("create: %d %s", code, errb)
	}
	b := bodyOf(t, h.last())
	m := b["params_mapping"].(map[string]any)
	if b["name"] != "gh" || b["workflow"] != "etl" || m["repo"] != "$.payload.repository.name" || m["n"] != 3.0 ||
		b["idempotency_key_path"] != "$.payload.id" {
		t.Fatalf("body %v", b)
	}
	if !strings.Contains(out, "s3cr3t") || !strings.Contains(errb, "cannot be shown again") {
		t.Fatalf("out %q err %q", out, errb)
	}
	if code, _, _ := h.run("trigger", "create", "x"); code != ExitUsage {
		t.Fatalf("missing --workflow: exit %d", code)
	}
}

// TestTriggerUpdateSendsOnlyGivenFlags: the given flags, plus the revision
// read just before -- the gateway has no unconditional overwrite.
func TestTriggerUpdateSendsOnlyGivenFlags(t *testing.T) {
	h := newHarness(t)
	h.routes(map[string]route{
		"GET " + ns + "/triggers/gh": {body: `{"name":"gh","revision":4}`},
		"PUT " + ns + "/triggers/gh": {body: `{"name":"gh"}`},
	})
	if code, _, errb := h.run("trigger", "update", "gh", "--disable"); code != 0 {
		t.Fatalf("%d %s", code, errb)
	}
	b := bodyOf(t, h.last())
	if len(b) != 2 || b["enabled"] != false || b["revision"] != 4.0 {
		t.Fatalf("update body %v", b)
	}
}

func TestTriggerFireSignsTheBody(t *testing.T) {
	h := newHarness(t)
	h.env["HOOK_SECRET"] = "abc"
	h.routes(map[string]route{
		"GET " + ns + "/triggers/gh": {body: `{"name":"gh","path":"/v1/hooks/default/t-1"}`},
		"POST /v1/hooks/default/t-1": {status: 202, body: `{"run_id":"r-1","created":true}`},
	})
	code, out, errb := h.run("trigger", "fire", "gh", "--secret-env", "HOOK_SECRET", "--data", `{"id":"d-1"}`)
	if code != 0 {
		t.Fatalf("%d %s", code, errb)
	}
	r := h.sent("POST", "/v1/hooks/default/t-1")[0]
	if !notify.VerifySignature([]byte("abc"), r.Headers.Get("X-Nexus-Timestamp"), r.Body, r.Headers.Get("X-Nexus-Signature")) {
		t.Fatal("fire sent an unverifiable signature")
	}
	if r.Headers.Get("Authorization") != "" {
		t.Fatal("the hook must be called as an external sender, without the API key")
	}
	if !strings.Contains(out, "r-1") {
		t.Fatalf("out %q", out)
	}
	if code, _, _ := h.run("trigger", "fire", "gh", "--secret-env", "NOPE"); code != ExitUsage {
		t.Fatalf("empty secret env: exit %d", code)
	}
}

func TestSensorCreateObjectStore(t *testing.T) {
	h := newHarness(t)
	h.routes(map[string]route{"POST " + ns + "/sensors": {status: 201, body: `{"name":"land"}`}})
	code, _, errb := h.run("sensor", "create", "land", "--workflow", "etl", "--uri", "s3://raw/in/", "--pattern", "*.csv",
		"--map", "path=$.object.uri", "--poll", "1m", "--backfill")
	if code != 0 {
		t.Fatalf("%d %s", code, errb)
	}
	b := bodyOf(t, h.last())
	cfg := b["config"].(map[string]any)
	if b["kind"] != "object_store" || cfg["uri"] != "s3://raw/in/" || cfg["pattern"] != "*.csv" ||
		b["poll_interval_seconds"] != 60.0 || b["backfill"] != true {
		t.Fatalf("body %v", b)
	}
	if code, _, _ := h.run("sensor", "create", "x", "--workflow", "etl", "--uri", "s3://a/", "--upstream", "b"); code != ExitUsage {
		t.Fatalf("both sources: exit %d", code)
	}
}

func TestSensorEnableDisable(t *testing.T) {
	h := newHarness(t)
	h.routes(map[string]route{
		"GET " + ns + "/sensors/land": {body: `{"name":"land","revision":2}`},
		"PUT " + ns + "/sensors/land": {body: `{"name":"land","enabled":false}`},
	})
	if code, _, _ := h.run("sensor", "disable", "land"); code != 0 || bodyOf(t, h.last())["enabled"] != false {
		t.Fatal("disable")
	}
	if code, _, _ := h.run("sensor", "enable", "land"); code != 0 || bodyOf(t, h.last())["enabled"] != true ||
		bodyOf(t, h.last())["revision"] != 2.0 {
		t.Fatal("enable")
	}
}

func TestNotifyChannelCreateReadsTheSecretFromTheEnvironment(t *testing.T) {
	h := newHarness(t)
	h.env["SLACK_URL"] = "https://hooks.slack.com/services/x"
	h.routes(map[string]route{"POST " + ns + "/notification-channels": {status: 201, body: `{"name":"s","kind":"slack_webhook"}`}})
	if code, _, errb := h.run("notify", "channel-create", "s", "--kind", "slack_webhook", "--secret-env", "SLACK_URL", "--rate", "10"); code != 0 {
		t.Fatalf("%d %s", code, errb)
	}
	b := bodyOf(t, h.last())
	if b["secret"] != "https://hooks.slack.com/services/x" || b["rate_limit_per_minute"] != 10.0 {
		t.Fatalf("body %v", b)
	}
	if code, _, _ := h.run("notify", "channel-create", "e", "--kind", "email", "--to", "a@x.io", "--to", "b@x.io"); code != 0 {
		t.Fatal("email")
	}
	cfg := bodyOf(t, h.last())["config"].(map[string]any)
	if len(cfg["to"].([]any)) != 2 {
		t.Fatalf("to %v", cfg)
	}
}

func TestNotifyRuleCreateAndDeadLetters(t *testing.T) {
	h := newHarness(t)
	h.routes(map[string]route{
		"POST " + ns + "/notification-rules":                {status: 201, body: `{"name":"r"}`},
		"GET " + ns + "/notification-deliveries":            {body: `{"deliveries":[{"id":7,"event_type":"run.failed","channel":"s","state":"dead","attempts":8}]}`},
		"POST " + ns + "/notification-deliveries/7/redrive": {status: 202, body: `{"delivery_id":8,"redrive_of":7,"state":"pending"}`},
	})
	if code, _, errb := h.run("notify", "rule-create", "r", "--event", "run.failed", "--event", "run.sla_exceeded",
		"--channel", "s", "--workflow", "etl", "--label", "team=data", "--sla", "30m"); code != 0 {
		t.Fatalf("%d %s", code, errb)
	}
	b := bodyOf(t, h.last())
	if b["sla_seconds"] != 1800.0 || b["labels"].(map[string]any)["team"] != "data" || len(b["events"].([]any)) != 2 {
		t.Fatalf("body %v", b)
	}
	code, out, _ := h.run("notify", "deliveries", "--state", "dead")
	if code != 0 || !strings.Contains(out, "dead") || !strings.Contains(h.last().Query, "state=dead") {
		t.Fatalf("deliveries %d %q %q", code, out, h.last().Query)
	}
	if code, out, _ := h.run("notify", "redrive", "7"); code != 0 || !strings.Contains(out, "8") {
		t.Fatalf("redrive %d %q", code, out)
	}
}

func TestAssetCommands(t *testing.T) {
	h := newHarness(t)
	h.routes(map[string]route{
		"GET " + ns + "/assets":        {body: `{"assets":[{"uri":"s3://raw/orders","producers":1,"consumers":2}]}`},
		"GET " + ns + "/assets/detail": {body: `{"uri":"s3://raw/orders","producer_tasks":[]}`},
		"GET " + ns + "/assets/graph":  {body: `{"nodes":[],"edges":[{"from":"task:etl/x","to":"asset:s3://raw/orders"}]}`},
	})
	if code, out, _ := h.run("asset", "list"); code != 0 || !strings.Contains(out, "s3://raw/orders") {
		t.Fatalf("list %q", out)
	}
	if code, _, _ := h.run("asset", "get", "s3://raw/orders"); code != 0 || !strings.Contains(h.last().Query, "uri=s3%3A%2F%2Fraw%2Forders") {
		t.Fatalf("get query %q", h.last().Query)
	}
	if code, out, _ := h.run("asset", "graph"); code != 0 || !strings.Contains(out, "task:etl/x") {
		t.Fatalf("graph %q", out)
	}
}

func TestEventGroupsAreInTheCompletionTree(t *testing.T) {
	got := complete([]string{""})
	for _, g := range []string{"trigger", "sensor", "notify", "asset"} {
		if !slices.Contains(got, g) {
			t.Errorf("completion is missing %q", g)
		}
	}
	if got := complete([]string{"notify", "re"}); !slices.Contains(got, "redrive") {
		t.Errorf("notify re -> %v", got)
	}
}
