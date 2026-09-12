package spec

import (
	"encoding/json"
	"fmt"

	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
)

// ToProto converts an already-schema-validated document into the wire type.
//
// Conversion is hand-written rather than protojson because the AUTHORING form and
// the WIRE form deliberately differ: the schema accepts readable enum spellings
// ("exponential", "all_success", "required") while the proto uses prefixed
// SCREAMING_SNAKE constants. protojson would reject every one of those, forcing
// authors to write BACKOFF_STRATEGY_EXPONENTIAL in their YAML — a leak of
// protobuf encoding conventions into a user-facing surface for no benefit.
//
// This runs AFTER ValidateStructure, so it may assume the document's shape and
// treats any surprise as a bug rather than as user error.
func ToProto(doc any) (*nexusv1.WorkflowSpec, error) {
	m, ok := doc.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("spec document is %T, want object", doc)
	}

	out := &nexusv1.WorkflowSpec{
		ApiVersion:    str(m, "api_version"),
		Namespace:     str(m, "namespace"),
		Name:          str(m, "name"),
		Description:   str(m, "description"),
		RunTimeoutMs:  i64(m, "run_timeout_ms"),
		FailFast:      boolean(m, "fail_fast"),
		MaxActiveRuns: i32(m, "max_active_runs"),
		Priority:      i32(m, "priority"),
		Labels:        strMap(m, "labels"),
		Params:        valueSpecMap(m, "params"),
	}

	for i, raw := range arr(m, "tasks") {
		t, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("tasks[%d] is %T, want object", i, raw)
		}
		ts, err := taskToProto(t)
		if err != nil {
			return nil, fmt.Errorf("tasks[%d]: %w", i, err)
		}
		out.Tasks = append(out.Tasks, ts)
	}

	for i, raw := range arr(m, "edges") {
		e, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("edges[%d] is %T, want object", i, raw)
		}
		out.Edges = append(out.Edges, &nexusv1.Edge{
			FromNodeId:    str(e, "from_node_id"),
			ToNodeId:      str(e, "to_node_id"),
			OutputToInput: strMap(e, "output_to_input"),
		})
	}
	return out, nil
}

func taskToProto(t map[string]any) (*nexusv1.TaskSpec, error) {
	ts := &nexusv1.TaskSpec{
		Id:                 str(t, "id"),
		TaskRef:            str(t, "task_ref"),
		Version:            i32(t, "version"),
		Inputs:             valueSpecMap(t, "inputs"),
		Outputs:            valueSpecMap(t, "outputs"),
		Queue:              strOr(t, "queue", "default"),
		NodeSelector:       strMap(t, "node_selector"),
		PreferSameWorkerAs: str(t, "prefer_same_worker_as"),
		ConcurrencyKey:     str(t, "concurrency_key"),
		ConcurrencyLimit:   i32(t, "concurrency_limit"),
		Priority:           i32(t, "priority"),
		// Defaults applied HERE, not left to proto3 zero values. proto3 cannot
		// distinguish "absent" from "zero", so relying on the zero value would
		// silently turn an omitted trigger_rule into UNSPECIFIED and an omitted
		// idempotency into UNSPECIFIED — losing the platform's two most
		// consequential defaults (all_success and required).
		TriggerRule: triggerRule(strOr(t, "trigger_rule", "all_success")),
		Idempotency: idempotencyMode(strOr(t, "idempotency", "required")),
	}

	if r, ok := obj(t, "resources"); ok {
		ts.Resources = &nexusv1.ResourceRequest{}
		if v, ok := obj(r, "request"); ok {
			ts.Resources.Request = resourceVector(v)
		}
		if v, ok := obj(r, "limit"); ok {
			ts.Resources.Limit = resourceVector(v)
		}
	}

	if to, ok := obj(t, "timeouts"); ok {
		ts.Timeouts = &nexusv1.Timeouts{
			ScheduleToStartMs: i64(to, "schedule_to_start_ms"),
			StartToCloseMs:    i64(to, "start_to_close_ms"),
			ScheduleToCloseMs: i64(to, "schedule_to_close_ms"),
			HeartbeatMs:       i64(to, "heartbeat_ms"),
		}
	}

	if r, ok := obj(t, "retry"); ok {
		rp := &nexusv1.RetryPolicy{
			MaxAttempts:            i32Or(r, "max_attempts", 3),
			Strategy:               backoffStrategy(strOr(r, "strategy", "exponential")),
			InitialMs:              i64(r, "initial_ms"),
			MaxMs:                  i64(r, "max_ms"),
			Multiplier:             f64(r, "multiplier"),
			Jitter:                 strOr(r, "jitter", "full"),
			RetryOnDifferentWorker: boolean(r, "retry_on_different_worker"),
		}
		for _, c := range arr(r, "retry_on") {
			if s, ok := c.(string); ok {
				rp.RetryOn = append(rp.RetryOn, errorClass(s))
			}
		}
		for _, c := range arr(r, "no_retry_on") {
			if s, ok := c.(string); ok {
				rp.NoRetryOn = append(rp.NoRetryOn, errorClass(s))
			}
		}
		ts.Retry = rp
	}

	if c, ok := obj(t, "cache"); ok {
		ts.Cache = &nexusv1.CachePolicy{
			Enabled: boolean(c, "enabled"),
			TtlMs:   i64(c, "ttl_ms"),
		}
	}

	if rt, ok := obj(t, "runtime"); ok {
		rs := &nexusv1.RuntimeSpec{
			Executor:     strOr(rt, "executor", "subprocess"),
			Image:        str(rt, "image"),
			Env:          strMap(rt, "env"),
			WorkingDir:   str(rt, "working_dir"),
			StdoutOutput: str(rt, "stdout_output"),
		}
		for _, c := range arr(rt, "command") {
			if s, ok := c.(string); ok {
				rs.Command = append(rs.Command, s)
			}
		}
		for _, a := range arr(rt, "args") {
			if s, ok := a.(string); ok {
				rs.Args = append(rs.Args, s)
			}
		}
		for _, s := range arr(rt, "secrets") {
			sm, ok := s.(map[string]any)
			if !ok {
				continue
			}
			// Only name and ref are carried. There is deliberately no field here
			// that could hold a secret VALUE: the worker resolves the ref at
			// execution time, so a compromise of the metadata store leaks no
			// credentials.
			rs.Secrets = append(rs.Secrets, &nexusv1.SecretRef{
				Name: str(sm, "name"),
				Ref:  str(sm, "ref"),
			})
		}
		ts.Runtime = rs
	}
	return ts, nil
}

func resourceVector(m map[string]any) *nexusv1.ResourceVector {
	rv := &nexusv1.ResourceVector{
		CpuMillis:      i64(m, "cpu_millis"),
		MemoryBytes:    i64(m, "memory_bytes"),
		GpuCount:       i32(m, "gpu_count"),
		GpuKind:        str(m, "gpu_kind"),
		GpuMemoryBytes: i64(m, "gpu_memory_bytes"),
		DiskBytes:      i64(m, "disk_bytes"),
	}
	if c, ok := obj(m, "custom"); ok && len(c) > 0 {
		rv.Custom = make(map[string]int64, len(c))
		for k, v := range c {
			rv.Custom[k] = toI64(v)
		}
	}
	return rv
}

func valueSpecMap(m map[string]any, key string) map[string]*nexusv1.ValueSpec {
	src, ok := obj(m, key)
	if !ok || len(src) == 0 {
		return nil
	}
	out := make(map[string]*nexusv1.ValueSpec, len(src))
	for name, raw := range src {
		vm, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		out[name] = &nexusv1.ValueSpec{
			Type:        str(vm, "type"),
			Required:    boolean(vm, "required"),
			DefaultJson: str(vm, "default_json"),
		}
	}
	return out
}

// The enum mappings below default to the documented platform default rather than
// to the proto UNSPECIFIED zero value. An unrecognized string cannot reach here:
// the schema's enum keyword rejected it in stage one.

func triggerRule(s string) nexusv1.TriggerRule {
	switch s {
	case "all_done":
		return nexusv1.TriggerRule_TRIGGER_RULE_ALL_DONE
	case "one_success":
		return nexusv1.TriggerRule_TRIGGER_RULE_ONE_SUCCESS
	case "one_failed":
		return nexusv1.TriggerRule_TRIGGER_RULE_ONE_FAILED
	case "none_failed":
		return nexusv1.TriggerRule_TRIGGER_RULE_NONE_FAILED
	case "all_skipped":
		return nexusv1.TriggerRule_TRIGGER_RULE_ALL_SKIPPED
	default:
		return nexusv1.TriggerRule_TRIGGER_RULE_ALL_SUCCESS
	}
}

func idempotencyMode(s string) nexusv1.IdempotencyMode {
	switch s {
	case "best_effort":
		return nexusv1.IdempotencyMode_IDEMPOTENCY_MODE_BEST_EFFORT
	case "none":
		return nexusv1.IdempotencyMode_IDEMPOTENCY_MODE_NONE
	default:
		return nexusv1.IdempotencyMode_IDEMPOTENCY_MODE_REQUIRED
	}
}

func backoffStrategy(s string) nexusv1.BackoffStrategy {
	switch s {
	case "fixed":
		return nexusv1.BackoffStrategy_BACKOFF_STRATEGY_FIXED
	case "linear":
		return nexusv1.BackoffStrategy_BACKOFF_STRATEGY_LINEAR
	default:
		return nexusv1.BackoffStrategy_BACKOFF_STRATEGY_EXPONENTIAL
	}
}

func errorClass(s string) nexusv1.ErrorClass {
	// BOTH the authoring spellings and the proto enum names are accepted, because
	// $defs/ErrorClass in the JSON Schema permits all of them. Accepting a value at
	// the schema gate and then dropping it here is the worst of both: the submission
	// succeeds, so the author believes the policy took effect, but the class decodes
	// to UNSPECIFIED, protojson omits it as a zero value, and the stored node spec
	// carries no retry_on at all. The attempt then fails with
	// "error_class_not_in_retry_on" naming a class the author explicitly allowed, and
	// nothing anywhere reports that their input was discarded (CONTRACT-FINDINGS.md
	// F20, F52).
	switch s {
	case "transient", "ERROR_CLASS_TRANSIENT":
		return nexusv1.ErrorClass_ERROR_CLASS_TRANSIENT
	case "timeout", "ERROR_CLASS_TIMEOUT":
		return nexusv1.ErrorClass_ERROR_CLASS_TIMEOUT
	case "worker_lost", "ERROR_CLASS_WORKER_LOST":
		return nexusv1.ErrorClass_ERROR_CLASS_WORKER_LOST
	// Architecture 4.3's YAML writes validation_error while common.proto has
	// ERROR_CLASS_VALIDATION, and an author who copied the published example must
	// not be rejected for it.
	case "validation", "validation_error", "ERROR_CLASS_VALIDATION":
		return nexusv1.ErrorClass_ERROR_CLASS_VALIDATION
	case "permission_denied", "ERROR_CLASS_PERMISSION_DENIED":
		return nexusv1.ErrorClass_ERROR_CLASS_PERMISSION_DENIED
	case "application", "ERROR_CLASS_APPLICATION":
		return nexusv1.ErrorClass_ERROR_CLASS_APPLICATION
	case "oom", "ERROR_CLASS_OOM":
		return nexusv1.ErrorClass_ERROR_CLASS_OOM
	case "not_found", "ERROR_CLASS_NOT_FOUND":
		return nexusv1.ErrorClass_ERROR_CLASS_NOT_FOUND
	case "infrastructure", "ERROR_CLASS_INFRASTRUCTURE":
		return nexusv1.ErrorClass_ERROR_CLASS_INFRASTRUCTURE
	default:
		return nexusv1.ErrorClass_ERROR_CLASS_UNSPECIFIED
	}
}

// ── accessors ─────────────────────────────────────────────────────────────
// Total functions over an already-validated document: a missing or mistyped
// field yields the zero value rather than an error, because the schema has
// already rejected anything genuinely wrong and a second layer of error plumbing
// here would only obscure the conversion.

func str(m map[string]any, k string) string {
	s, _ := m[k].(string)
	return s
}

func strOr(m map[string]any, k, def string) string {
	if s, ok := m[k].(string); ok && s != "" {
		return s
	}
	return def
}

func boolean(m map[string]any, k string) bool {
	b, _ := m[k].(bool)
	return b
}

func obj(m map[string]any, k string) (map[string]any, bool) {
	o, ok := m[k].(map[string]any)
	return o, ok
}

func arr(m map[string]any, k string) []any {
	a, _ := m[k].([]any)
	return a
}

func strMap(m map[string]any, k string) map[string]string {
	src, ok := obj(m, k)
	if !ok || len(src) == 0 {
		return nil
	}
	out := make(map[string]string, len(src))
	for kk, vv := range src {
		if s, ok := vv.(string); ok {
			out[kk] = s
		}
	}
	return out
}

func toI64(v any) int64 {
	switch n := v.(type) {
	case json.Number:
		i, err := n.Int64()
		if err == nil {
			return i
		}
		f, ferr := n.Float64()
		if ferr == nil {
			return int64(f)
		}
	case float64:
		return int64(n)
	case int:
		return int64(n)
	case int64:
		return n
	}
	return 0
}

func i64(m map[string]any, k string) int64 { return toI64(m[k]) }

func i32(m map[string]any, k string) int32 { return int32(toI64(m[k])) }

func i32Or(m map[string]any, k string, def int32) int32 {
	if _, present := m[k]; !present {
		return def
	}
	return int32(toI64(m[k]))
}

func f64(m map[string]any, k string) float64 {
	switch n := m[k].(type) {
	case json.Number:
		f, err := n.Float64()
		if err == nil {
			return f
		}
	case float64:
		return n
	}
	return 0
}
