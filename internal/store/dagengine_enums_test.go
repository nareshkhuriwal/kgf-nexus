// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"encoding/json"
	"testing"

	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
	"google.golang.org/protobuf/encoding/protojson"
)

// parse mirrors what LoadSpec does, so these tests exercise the real path.
func parse(t *testing.T, raw string) *nexusv1.WorkflowSpec {
	t.Helper()
	var spec nexusv1.WorkflowSpec
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).
		Unmarshal(normalizeSpecEnums([]byte(raw)), &spec); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return &spec
}

// TestRetryOnSurvivesAuthoringSpelling is the regression test for the silent
// retry-policy loss: protojson discards "application" without error, so before
// normalization every retry_on an author wrote arrived as an empty list and the
// attempt failed with "error_class_not_in_retry_on" naming the allowed class.
func TestRetryOnSurvivesAuthoringSpelling(t *testing.T) {
	const doc = `{"api_version":"nexus.dev/v1","namespace":"d","name":"n","tasks":[
	  {"id":"a","task_ref":"x.y","retry":{"max_attempts":2,
	   "strategy":"exponential","retry_on":["application","oom"],
	   "no_retry_on":["validation_error"]}}]}`

	got := parse(t, doc).GetTasks()[0].GetRetry()
	if want := []nexusv1.ErrorClass{
		nexusv1.ErrorClass_ERROR_CLASS_APPLICATION,
		nexusv1.ErrorClass_ERROR_CLASS_OOM,
	}; len(got.GetRetryOn()) != len(want) {
		t.Fatalf("retry_on = %v, want %v (authoring spellings were dropped)", got.GetRetryOn(), want)
	}
	if got.GetRetryOn()[0] != nexusv1.ErrorClass_ERROR_CLASS_APPLICATION {
		t.Errorf("retry_on[0] = %v, want ERROR_CLASS_APPLICATION", got.GetRetryOn()[0])
	}
	if got.GetStrategy() != nexusv1.BackoffStrategy_BACKOFF_STRATEGY_EXPONENTIAL {
		t.Errorf("strategy = %v, want EXPONENTIAL", got.GetStrategy())
	}
	if len(got.GetNoRetryOn()) != 1 ||
		got.GetNoRetryOn()[0] != nexusv1.ErrorClass_ERROR_CLASS_VALIDATION {
		t.Errorf("no_retry_on = %v, want [ERROR_CLASS_VALIDATION]", got.GetNoRetryOn())
	}
}

// TestProtoSpellingStillParses proves normalization is additive: a document that
// already uses proto enum names, as dagengine_writes emits, is unaffected.
func TestProtoSpellingStillParses(t *testing.T) {
	const doc = `{"api_version":"nexus.dev/v1","namespace":"d","name":"n","tasks":[
	  {"id":"a","task_ref":"x.y","idempotency":"IDEMPOTENCY_MODE_REQUIRED",
	   "trigger_rule":"TRIGGER_RULE_ALL_DONE",
	   "retry":{"max_attempts":2,"retry_on":["ERROR_CLASS_APPLICATION"]}}]}`

	task := parse(t, doc).GetTasks()[0]
	if got := task.GetRetry().GetRetryOn(); len(got) != 1 ||
		got[0] != nexusv1.ErrorClass_ERROR_CLASS_APPLICATION {
		t.Errorf("retry_on = %v, want [ERROR_CLASS_APPLICATION]", got)
	}
	if task.GetIdempotency() != nexusv1.IdempotencyMode_IDEMPOTENCY_MODE_REQUIRED {
		t.Errorf("idempotency = %v, want REQUIRED", task.GetIdempotency())
	}
	if task.GetTriggerRule() != nexusv1.TriggerRule_TRIGGER_RULE_ALL_DONE {
		t.Errorf("trigger_rule = %v, want ALL_DONE", task.GetTriggerRule())
	}
}

// TestAuthoringSpellingsForOtherEnums covers the remaining enum fields, since the
// same protojson behaviour applies to each of them.
func TestAuthoringSpellingsForOtherEnums(t *testing.T) {
	const doc = `{"api_version":"nexus.dev/v1","namespace":"d","name":"n","tasks":[
	  {"id":"a","task_ref":"x.y","idempotency":"required","trigger_rule":"all_done"}]}`

	task := parse(t, doc).GetTasks()[0]
	if task.GetIdempotency() != nexusv1.IdempotencyMode_IDEMPOTENCY_MODE_REQUIRED {
		t.Errorf("idempotency = %v, want REQUIRED", task.GetIdempotency())
	}
	if task.GetTriggerRule() != nexusv1.TriggerRule_TRIGGER_RULE_ALL_DONE {
		t.Errorf("trigger_rule = %v, want ALL_DONE", task.GetTriggerRule())
	}
}

// TestUnknownEnumValueIsLeftAlone keeps normalization from inventing meaning: an
// unrecognised value must reach protojson unchanged so the caller's error stands.
func TestUnknownEnumValueIsLeftAlone(t *testing.T) {
	const doc = `{"api_version":"nexus.dev/v1","namespace":"d","name":"n","tasks":[
	  {"id":"a","task_ref":"x.y","retry":{"retry_on":["not_a_real_class"]}}]}`

	if got := parse(t, doc).GetTasks()[0].GetRetry().GetRetryOn(); len(got) != 0 {
		t.Errorf("retry_on = %v, want empty for an unknown class", got)
	}
}

// The DAG engine re-reads the stored (short-spelling) spec with protojson, whose
// DiscardUnknown drops an unrecognised enum string to the zero value. Found by
// the simulated spot e2e: `capacity: spot_preferred` materialized as
// SPOT_ALLOWED, and `preempted` in no_retry_on vanished.
func TestPlacementCapacityAndPreemptedSurviveMaterialization(t *testing.T) {
	spec := parse(t, `{"api_version":"nexus.dev/v1","namespace":"d","name":"n","tasks":[
	 {"id":"a","task_ref":"x.y","placement":{"capacity":"on_demand_only","max_preemptions":2},
	  "retry":{"no_retry_on":["preempted"]}},
	 {"id":"b","task_ref":"x.y","placement":{"capacity":"spot_preferred"}}]}`)
	if got := spec.GetTasks()[0].GetPlacement().GetCapacity(); got != nexusv1.CapacityPreference_CAPACITY_PREFERENCE_ON_DEMAND_ONLY {
		t.Fatalf("on_demand_only materialized as %v", got)
	}
	if got := spec.GetTasks()[1].GetPlacement().GetCapacity(); got != nexusv1.CapacityPreference_CAPACITY_PREFERENCE_SPOT_PREFERRED {
		t.Fatalf("spot_preferred materialized as %v", got)
	}
	if nr := spec.GetTasks()[0].GetRetry().GetNoRetryOn(); len(nr) != 1 || nr[0] != nexusv1.ErrorClass_ERROR_CLASS_PREEMPTED {
		t.Fatalf("no_retry_on: [preempted] materialized as %v", nr)
	}
}

// TestTaskSpecViewParsesProtoNamesWithQuotedInts is the regression test for the
// second half of the silent retry loss.
//
// The DAG engine marshals node specs with protojson UseProtoNames: snake_case
// keys, but 64-bit integers rendered as QUOTED STRINGS. The flat decode declares
// those fields int64, so `"start_to_close_ms": "60000"` failed the whole struct
// and every such node arrived as an empty spec — no executor constraint, no
// timeouts, and no retry policy, which is what made every failure report
// "error_class_not_in_retry_on".
func TestTaskSpecViewParsesProtoNamesWithQuotedInts(t *testing.T) {
	// Verbatim shape of a real task_nodes.spec_json row.
	const row = `{
	  "id": "flaky", "queue": "default", "task_ref": "shell.flaky:v1",
	  "retry": {"max_ms":"8000","retry_on":["ERROR_CLASS_APPLICATION"],
	            "strategy":"BACKOFF_STRATEGY_EXPONENTIAL","initial_ms":"2000",
	            "multiplier":2,"max_attempts":2},
	  "runtime": {"executor":"subprocess","command":["sh","-c"],"args":["exit 3"]},
	  "timeouts": {"start_to_close_ms":"60000","schedule_to_start_ms":"300000"}
	}`

	var view TaskSpecView
	if err := json.Unmarshal([]byte(row), &view); err != nil {
		t.Fatalf("unmarshal node spec: %v", err)
	}

	if view.Executor != "subprocess" {
		t.Errorf("Executor = %q, want subprocess (placement would ignore executor compatibility)", view.Executor)
	}
	if view.StartToCloseMs != 60000 {
		t.Errorf("StartToCloseMs = %d, want 60000 (no timer would be set)", view.StartToCloseMs)
	}
	if view.ScheduleToStartMs != 300000 {
		t.Errorf("ScheduleToStartMs = %d, want 300000", view.ScheduleToStartMs)
	}
	if view.TaskRef != "shell.flaky:v1" {
		t.Errorf("TaskRef = %q, want shell.flaky:v1", view.TaskRef)
	}

	pol := view.RetryPolicy()
	if pol.GetMaxAttempts() != 2 {
		t.Errorf("MaxAttempts = %d, want 2", pol.GetMaxAttempts())
	}
	if pol.GetInitialMs() != 2000 || pol.GetMaxMs() != 8000 {
		t.Errorf("backoff = (initial %d, max %d), want (2000, 8000)",
			pol.GetInitialMs(), pol.GetMaxMs())
	}
	if pol.GetStrategy() != nexusv1.BackoffStrategy_BACKOFF_STRATEGY_EXPONENTIAL {
		t.Errorf("Strategy = %v, want EXPONENTIAL", pol.GetStrategy())
	}
	got := pol.GetRetryOn()
	if len(got) != 1 || got[0] != nexusv1.ErrorClass_ERROR_CLASS_APPLICATION {
		t.Fatalf("RetryOn = %v, want [ERROR_CLASS_APPLICATION]; "+
			"an empty list is what made every failure look non-retryable", got)
	}
}

// TestTaskSpecViewStillParsesBareNumbers keeps the fix additive: the gateway's
// dialect writes the same keys with UNquoted integers, and it must keep working.
func TestTaskSpecViewStillParsesBareNumbers(t *testing.T) {
	const row = `{
	  "task_ref":"a.b","executor":"docker",
	  "schedule_to_start_ms":1000,"start_to_close_ms":2000,
	  "retry":{"max_attempts":3,"initial_ms":500,"max_ms":9000,"retry_on":["application"]}
	}`

	var view TaskSpecView
	if err := json.Unmarshal([]byte(row), &view); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if view.Executor != "docker" || view.StartToCloseMs != 2000 || view.ScheduleToStartMs != 1000 {
		t.Errorf("view = %+v, want executor=docker s2s=1000 stc=2000", view)
	}
	pol := view.RetryPolicy()
	if pol.GetInitialMs() != 500 || pol.GetMaxMs() != 9000 || pol.GetMaxAttempts() != 3 {
		t.Errorf("policy = (%d, %d, %d), want (500, 9000, 3)",
			pol.GetInitialMs(), pol.GetMaxMs(), pol.GetMaxAttempts())
	}
	if got := pol.GetRetryOn(); len(got) != 1 ||
		got[0] != nexusv1.ErrorClass_ERROR_CLASS_APPLICATION {
		t.Errorf("RetryOn = %v, want [ERROR_CLASS_APPLICATION]", got)
	}
}
