package store

import (
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
