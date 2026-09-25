// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package relay

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/khuriwalgroup/kgf-nexus/internal/bus"
	"github.com/khuriwalgroup/kgf-nexus/internal/store"
)

func ptr[T any](v T) *T { return &v }

func testRelay(t *testing.T) *Relay {
	t.Helper()
	r, err := New(nil, nil, DefaultConfig(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// TestBackoffIsBoundedAndNeverZero: an outage must neither hot-loop the relay
// (zero wait) nor park it past MaxBackoff after the broker is back.
func TestBackoffIsBoundedAndNeverZero(t *testing.T) {
	min, max := 250*time.Millisecond, 10*time.Second
	var b time.Duration
	for i := 0; i < 50; i++ {
		b = nextBackoff(b, min, max)
		if b < min/2 || b > max {
			t.Fatalf("step %d: backoff %v outside [%v, %v]", i, b, min/2, max)
		}
	}
	// Grows: after many failures it must be well above the minimum.
	if b < max/2 {
		t.Errorf("backoff did not grow toward the cap: %v", b)
	}
}

func TestRunEventMessage(t *testing.T) {
	r := testRelay(t)
	run := "5d6c2b1a-0000-4000-8000-000000000001"
	occurred := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	row := outboxRow{
		id: 42, topic: store.OutboxTopicRunEvent, createdAt: occurred,
		evID: ptr(int64(9)), evRunID: ptr(run), evSeq: ptr(int32(3)), evNodeID: ptr("n1"),
		evAttemptID: ptr("a-1"), evKind: ptr("AttemptDispatched"), evActor: ptr("scheduler"),
		evOccurred: &occurred, evNamespace: ptr("acme"), evPayload: []byte(`{"worker_id":"w"}`),
	}
	msg, known, gone := r.buildMessage(row)
	if !known || gone || msg == nil {
		t.Fatalf("known=%v gone=%v msg=%v", known, gone, msg)
	}
	if msg.Subject != bus.RunEventSubject("acme", run) {
		t.Errorf("subject = %q", msg.Subject)
	}
	var body map[string]any
	if err := json.Unmarshal(msg.Data, &body); err != nil {
		t.Fatal(err)
	}
	// seq is what lets a consumer order and de-duplicate; without it the stream
	// cannot be used for anything but "something happened".
	if body["seq"].(float64) != 3 || body["run_id"] != run || body["kind"] != "AttemptDispatched" {
		t.Errorf("body = %v", body)
	}
	if p, ok := body["payload"].(map[string]any); !ok || p["worker_id"] != "w" {
		t.Errorf("payload not carried: %v", body["payload"])
	}
	if msg.Header.Get("Nexus-Outbox-Topic") != store.OutboxTopicRunEvent {
		t.Error("topic header missing")
	}
}

// TestOversizedEventPayloadIsTruncatedNotDropped: a large payload must not make
// the whole row unpublishable (it would retry until it expired); the consumer is
// told to fetch the authoritative event instead.
func TestOversizedEventPayloadIsTruncatedNotDropped(t *testing.T) {
	r := testRelay(t)
	r.cfg.MaxEventPayloadBytes = 16
	row := outboxRow{
		id: 1, topic: store.OutboxTopicRunEvent,
		evID: ptr(int64(1)), evRunID: ptr("r"), evNamespace: ptr("ns"),
		evPayload: []byte(`{"stderr":"` + strings.Repeat("x", 100) + `"}`),
	}
	msg, known, _ := r.buildMessage(row)
	if !known {
		t.Fatal("not published")
	}
	var body map[string]any
	_ = json.Unmarshal(msg.Data, &body)
	if body["payload_truncated"] != true || body["payload"] != nil {
		t.Errorf("oversized payload not truncated: %v", body)
	}
}

// TestDeletedRunEventIsGoneNotRetried: the run was deleted (cascade removed the
// event) before the relay reached the row. Retrying would never succeed.
func TestDeletedRunEventIsGoneNotRetried(t *testing.T) {
	r := testRelay(t)
	_, known, gone := r.buildMessage(outboxRow{id: 1, topic: store.OutboxTopicRunEvent})
	if !known || !gone {
		t.Errorf("known=%v gone=%v, want a known topic with nothing left to publish", known, gone)
	}
}

func TestTaskReadyAndRunStateMessages(t *testing.T) {
	r := testRelay(t)
	msg, known, _ := r.buildMessage(outboxRow{
		id: 5, topic: store.OutboxTopicTaskReady, namespace: ptr("acme"), routingKey: ptr("gpu"),
		payload: []byte(`{"attempt_id":"a","queue":"gpu"}`),
	})
	if !known || msg.Subject != bus.DispatchSubject("acme", "gpu") {
		t.Fatalf("task_ready subject = %v known=%v", msg, known)
	}
	msg, known, _ = r.buildMessage(outboxRow{
		id: 6, topic: store.OutboxTopicRunState, namespace: ptr("acme"), routingKey: ptr("run-1"),
		payload: []byte(`{"state":"RUNNING","state_version":4}`),
	})
	if !known || msg.Subject != bus.RunStateSubject("acme", "run-1") {
		t.Fatalf("run_state subject = %v known=%v", msg, known)
	}
	var body map[string]any
	_ = json.Unmarshal(msg.Data, &body)
	if body["state_version"].(float64) != 4 || body["type"] != "run.state" || body["outbox_id"].(float64) != 6 {
		t.Errorf("run_state body = %v", body)
	}
}

// TestUsageRecordedMessage: the metering export hook (ADR-0021) goes to the
// namespace's usage subject, outside nexus.run.>, with the ledger payload intact.
func TestUsageRecordedMessage(t *testing.T) {
	r := testRelay(t)
	msg, known, gone := r.buildMessage(outboxRow{
		id: 9, topic: store.OutboxTopicUsageRecorded, namespace: ptr("acme"), routingKey: ptr("run-1"),
		payload: []byte(`{"ledger_id":17,"attempt_id":"a","cpu_core_seconds":2.5}`),
	})
	if !known || gone || msg.Subject != bus.UsageSubject("acme") {
		t.Fatalf("usage_recorded: msg=%v known=%v gone=%v", msg, known, gone)
	}
	var body map[string]any
	_ = json.Unmarshal(msg.Data, &body)
	if body["type"] != "nexus.usage.recorded" || body["ledger_id"].(float64) != 17 ||
		body["cpu_core_seconds"].(float64) != 2.5 || body["outbox_id"].(float64) != 9 {
		t.Errorf("usage_recorded body = %v", body)
	}
}

// TestUnknownTopicIsDeferredNotDropped: during a rolling deploy an OLD relay may
// meet a topic a NEW writer introduced. It must leave the row for a newer
// replica, not discard it and not report it as published.
func TestUnknownTopicIsDeferredNotDropped(t *testing.T) {
	r := testRelay(t)
	msg, known, gone := r.buildMessage(outboxRow{id: 1, topic: "from_the_future"})
	if known || gone || msg != nil {
		t.Errorf("unknown topic: msg=%v known=%v gone=%v, want deferred", msg, known, gone)
	}
}

func TestConfigFromEnvIgnoresGarbage(t *testing.T) {
	t.Setenv("NEXUS_OUTBOX_BATCH_SIZE", "not-a-number")
	t.Setenv("NEXUS_OUTBOX_RETENTION", "2h")
	c := ConfigFromEnv(testRelay(t).log)
	if c.BatchSize != DefaultConfig().BatchSize {
		t.Errorf("garbage batch size was applied: %d", c.BatchSize)
	}
	if c.Retention != 2*time.Hour {
		t.Errorf("valid retention not applied: %v", c.Retention)
	}
}
