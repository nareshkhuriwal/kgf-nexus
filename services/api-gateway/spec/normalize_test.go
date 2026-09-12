// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package spec

import (
	"encoding/json"
	"testing"

	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
)

// The point of F52: a spec the SDK emits and the same spec hand-written must be
// accepted AND must become the same stored document. Equal content addresses is
// the assertion that actually proves it, because the content address is the
// spec's identity (ADR-0004).
func TestProtoAndShortEnumSpellingsCompileIdentically(t *testing.T) {
	protoForm := []byte(`{
	  "api_version": "nexus.dev/v1", "namespace": "default", "name": "enum-spelling",
	  "tasks": [{
	    "id": "a", "task_ref": "x.y", "version": 1,
	    "idempotency": "IDEMPOTENCY_MODE_BEST_EFFORT",
	    "trigger_rule": "TRIGGER_RULE_ALL_DONE",
	    "retry": {
	      "strategy": "BACKOFF_STRATEGY_LINEAR",
	      "retry_on": ["ERROR_CLASS_TIMEOUT", "ERROR_CLASS_TRANSIENT"],
	      "no_retry_on": ["ERROR_CLASS_VALIDATION"]
	    }
	  }]
	}`)
	shortForm := []byte(`{
	  "api_version": "nexus.dev/v1", "namespace": "default", "name": "enum-spelling",
	  "tasks": [{
	    "id": "a", "task_ref": "x.y", "version": 1,
	    "idempotency": "best_effort",
	    "trigger_rule": "all_done",
	    "retry": {
	      "strategy": "linear",
	      "retry_on": ["timeout", "transient"],
	      "no_retry_on": ["validation"]
	    }
	  }]
	}`)

	a, err := Compile(protoForm, FormatJSON)
	if err != nil {
		t.Fatalf("proto-spelled spec rejected, which is exactly F52: %v", err)
	}
	b, err := Compile(shortForm, FormatJSON)
	if err != nil {
		t.Fatalf("short-spelled spec rejected: %v", err)
	}

	if a.Hash != b.Hash {
		t.Errorf("the two spellings of one workflow produced different content addresses\n"+
			"  proto form: %s\n  short form: %s\n"+
			"so the platform would treat them as two workflows", a.Hash, b.Hash)
	}

	got := a.Proto.GetTasks()[0]
	if got.GetIdempotency() != nexusv1.IdempotencyMode_IDEMPOTENCY_MODE_BEST_EFFORT {
		t.Errorf("idempotency = %v, want BEST_EFFORT", got.GetIdempotency())
	}
	if got.GetTriggerRule() != nexusv1.TriggerRule_TRIGGER_RULE_ALL_DONE {
		t.Errorf("trigger_rule = %v, want ALL_DONE", got.GetTriggerRule())
	}
	if got.GetRetry().GetStrategy() != nexusv1.BackoffStrategy_BACKOFF_STRATEGY_LINEAR {
		t.Errorf("strategy = %v, want LINEAR", got.GetRetry().GetStrategy())
	}
	if len(got.GetRetry().GetRetryOn()) != 2 {
		t.Errorf("retry_on = %v, want 2 classes", got.GetRetry().GetRetryOn())
	}
}

// Normalization must be idempotent: a stored, already-canonical document that is
// resubmitted has to hash to what it hashed before, or "resubmitting a
// byte-identical spec is a no-op" stops holding.
func TestNormalizeEnumsIsIdempotent(t *testing.T) {
	doc := map[string]any{
		"tasks": []any{map[string]any{
			"idempotency":  "none",
			"trigger_rule": "all_success",
			"retry": map[string]any{
				"strategy": "exponential",
				"retry_on": []any{"transient"},
			},
		}},
	}
	NormalizeEnums(doc)
	NormalizeEnums(doc)

	task := doc["tasks"].([]any)[0].(map[string]any)
	if task["idempotency"] != "none" || task["trigger_rule"] != "all_success" {
		t.Errorf("canonical values were rewritten: %#v", task)
	}
	retry := task["retry"].(map[string]any)
	if retry["strategy"] != "exponential" {
		t.Errorf("strategy rewritten to %v", retry["strategy"])
	}
	if retry["retry_on"].([]any)[0] != "transient" {
		t.Errorf("retry_on rewritten to %v", retry["retry_on"])
	}
}

// validation_error is architecture 4.3's spelling for ERROR_CLASS_VALIDATION,
// and both must land on one stored value (F20) or the two spellings would again
// content-address differently.
func TestValidationErrorAliasNormalizesToValidation(t *testing.T) {
	doc := map[string]any{
		"tasks": []any{map[string]any{
			"retry": map[string]any{
				"no_retry_on": []any{"validation_error", "ERROR_CLASS_VALIDATION", "validation"},
			},
		}},
	}
	NormalizeEnums(doc)
	got := doc["tasks"].([]any)[0].(map[string]any)["retry"].(map[string]any)["no_retry_on"].([]any)
	for i, v := range got {
		if v != "validation" {
			t.Errorf("no_retry_on[%d] = %q, want %q", i, v, "validation")
		}
	}
}

// A stdout_output naming an undeclared output must be rejected at submit time,
// because the run-time symptom is a green task that produced nothing.
func TestStdoutOutputMustNameADeclaredOutput(t *testing.T) {
	body := []byte(`{
	  "api_version": "nexus.dev/v1", "namespace": "default", "name": "bad-stdout",
	  "tasks": [{
	    "id": "a", "task_ref": "x.y", "version": 1,
	    "outputs": {"result": {"type": "json"}},
	    "runtime": {"executor": "subprocess", "command": ["echo"], "stdout_output": "reslt"}
	  }]
	}`)
	if _, err := Compile(body, FormatJSON); err == nil {
		t.Fatal("a stdout_output naming an undeclared output was accepted")
	}

	body = []byte(`{
	  "api_version": "nexus.dev/v1", "namespace": "default", "name": "good-stdout",
	  "tasks": [{
	    "id": "a", "task_ref": "x.y", "version": 1,
	    "outputs": {"result": {"type": "json"}},
	    "runtime": {"executor": "subprocess", "command": ["echo"], "stdout_output": "result"}
	  }]
	}`)
	c, err := Compile(body, FormatJSON)
	if err != nil {
		t.Fatalf("a valid stdout_output was rejected: %v", err)
	}
	if got := c.Proto.GetTasks()[0].GetRuntime().GetStdoutOutput(); got != "result" {
		t.Errorf("stdout_output did not survive the conversion: %q", got)
	}
}

// TestErrorClassAcceptsProtoEnumNames guards a silent-data-loss bug.
//
// $defs/ErrorClass in the JSON Schema accepts BOTH the short authoring spellings
// and the proto enum names, so a spec using ERROR_CLASS_APPLICATION passes
// validation. Before this was fixed, the decoder recognised only the short form
// and returned UNSPECIFIED for the rest; protojson then omitted the zero value,
// the stored node spec carried no retry_on, and the attempt failed with
// "error_class_not_in_retry_on" naming the very class the author had allowed.
// Every spelling the schema admits must survive the decode.
func TestErrorClassAcceptsProtoEnumNames(t *testing.T) {
	pairs := map[string]nexusv1.ErrorClass{
		"transient":                     nexusv1.ErrorClass_ERROR_CLASS_TRANSIENT,
		"ERROR_CLASS_TRANSIENT":         nexusv1.ErrorClass_ERROR_CLASS_TRANSIENT,
		"timeout":                       nexusv1.ErrorClass_ERROR_CLASS_TIMEOUT,
		"ERROR_CLASS_TIMEOUT":           nexusv1.ErrorClass_ERROR_CLASS_TIMEOUT,
		"worker_lost":                   nexusv1.ErrorClass_ERROR_CLASS_WORKER_LOST,
		"ERROR_CLASS_WORKER_LOST":       nexusv1.ErrorClass_ERROR_CLASS_WORKER_LOST,
		"validation":                    nexusv1.ErrorClass_ERROR_CLASS_VALIDATION,
		"validation_error":              nexusv1.ErrorClass_ERROR_CLASS_VALIDATION,
		"ERROR_CLASS_VALIDATION":        nexusv1.ErrorClass_ERROR_CLASS_VALIDATION,
		"permission_denied":             nexusv1.ErrorClass_ERROR_CLASS_PERMISSION_DENIED,
		"ERROR_CLASS_PERMISSION_DENIED": nexusv1.ErrorClass_ERROR_CLASS_PERMISSION_DENIED,
		"application":                   nexusv1.ErrorClass_ERROR_CLASS_APPLICATION,
		"ERROR_CLASS_APPLICATION":       nexusv1.ErrorClass_ERROR_CLASS_APPLICATION,
		"oom":                           nexusv1.ErrorClass_ERROR_CLASS_OOM,
		"ERROR_CLASS_OOM":               nexusv1.ErrorClass_ERROR_CLASS_OOM,
		"not_found":                     nexusv1.ErrorClass_ERROR_CLASS_NOT_FOUND,
		"ERROR_CLASS_NOT_FOUND":         nexusv1.ErrorClass_ERROR_CLASS_NOT_FOUND,
		"infrastructure":                nexusv1.ErrorClass_ERROR_CLASS_INFRASTRUCTURE,
		"ERROR_CLASS_INFRASTRUCTURE":    nexusv1.ErrorClass_ERROR_CLASS_INFRASTRUCTURE,
	}
	for in, want := range pairs {
		if got := errorClass(in); got != want {
			t.Errorf("errorClass(%q) = %v, want %v", in, got, want)
		}
	}
}

// TestErrorClassSpellingsCoverTheSchemaEnum keeps the decoder and the schema from
// drifting apart: any value $defs/ErrorClass admits must decode to something real,
// or authors get silent data loss rather than a validation error.
func TestErrorClassSpellingsCoverTheSchemaEnum(t *testing.T) {
	var doc struct {
		Defs map[string]struct {
			Enum []string `json:"enum"`
		} `json:"$defs"`
	}
	raw, err := SchemaBytes()
	if err != nil {
		t.Fatalf("read embedded schema: %v", err)
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse embedded schema: %v", err)
	}
	vals := doc.Defs["ErrorClass"].Enum
	if len(vals) == 0 {
		t.Fatal("schema $defs/ErrorClass has no enum; test cannot verify coverage")
	}
	for _, v := range vals {
		if errorClass(v) == nexusv1.ErrorClass_ERROR_CLASS_UNSPECIFIED {
			t.Errorf("schema accepts %q but the decoder maps it to UNSPECIFIED, "+
				"so the value would be silently dropped", v)
		}
	}
}
