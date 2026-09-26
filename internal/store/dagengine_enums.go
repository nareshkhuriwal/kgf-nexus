// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"bytes"
	"encoding/json"
)

// normalizeSpecEnums rewrites the authoring spellings of enum values in a stored
// WorkflowSpec document into the proto enum names that protojson requires.
//
// WHY THIS EXISTS. The gateway content-addresses and stores the AUTHOR'S document
// (ADR-0004): canonicalization normalizes key order, numbers and escaping, but it
// deliberately does not rewrite values, because the content address has to remain
// a hash of what the author actually submitted. The JSON Schema accepts both the
// short authoring spellings ("application") and the proto enum names
// ("ERROR_CLASS_APPLICATION") for the same constant, so either can be sitting in
// workflow_specs.spec_json.
//
// protojson understands only the proto names. Worse, it does not complain: given
// "application" for an enum field it discards the value and returns no error, so a
// retry_on the author wrote arrives here as an empty list. The attempt then fails
// with "error_class_not_in_retry_on" naming the exact class the author allowed,
// and no log anywhere records that the policy was dropped. Every retry_on and
// no_retry_on in the system was silently inert.
//
// Normalizing on the way IN, rather than making the gateway store proto names,
// keeps the content address a function of the submitted bytes and leaves every
// spec already in the table working (CONTRACT-FINDINGS.md F20, F52).
func normalizeSpecEnums(raw []byte) []byte {
	// Cheap pre-check: the overwhelming majority of specs contain no enum value
	// needing a rewrite, and this runs on the run-materialization path.
	if !bytes.Contains(raw, []byte(`"retry"`)) &&
		!bytes.Contains(raw, []byte(`"idempotency"`)) &&
		!bytes.Contains(raw, []byte(`"trigger_rule"`)) &&
		!bytes.Contains(raw, []byte(`"strategy"`)) &&
		!bytes.Contains(raw, []byte(`"capacity"`)) {
		return raw
	}
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		// Not our error to report: the caller's protojson pass will produce a far
		// better message than anything we could add here.
		return raw
	}
	if !rewriteEnums(doc, "") {
		return raw
	}
	out, err := json.Marshal(doc)
	if err != nil {
		return raw
	}
	return out
}

// enumPrefixes maps a field name to the proto enum prefix its values carry.
// A value is rewritten only when the short form is a known member, so an already
// proto-named value and an unknown value both pass through untouched.
var enumPrefixes = map[string]map[string]string{
	"retry_on":     errorClassNames,
	"no_retry_on":  errorClassNames,
	"strategy":     backoffStrategyNames,
	"idempotency":  idempotencyModeNames,
	"trigger_rule": triggerRuleNames,
	// PlacementSpec.capacity (ADR-0030). Without it protojson's DiscardUnknown
	// silently turned "spot_preferred" and "on_demand_only" into the zero value
	// SPOT_ALLOWED: the spec was accepted, and the constraint vanished at run
	// materialization -- found by the simulated spot e2e (tests/gpusim).
	"capacity": capacityPreferenceNames,
}

var capacityPreferenceNames = map[string]string{
	"spot_allowed":   "CAPACITY_PREFERENCE_SPOT_ALLOWED",
	"spot_preferred": "CAPACITY_PREFERENCE_SPOT_PREFERRED",
	"on_demand_only": "CAPACITY_PREFERENCE_ON_DEMAND_ONLY",
}

var errorClassNames = map[string]string{
	"transient": "ERROR_CLASS_TRANSIENT", "timeout": "ERROR_CLASS_TIMEOUT",
	"worker_lost": "ERROR_CLASS_WORKER_LOST", "validation": "ERROR_CLASS_VALIDATION",
	// architecture 4.3's YAML spells this one differently from common.proto.
	"validation_error":  "ERROR_CLASS_VALIDATION",
	"permission_denied": "ERROR_CLASS_PERMISSION_DENIED",
	"application":       "ERROR_CLASS_APPLICATION", "oom": "ERROR_CLASS_OOM",
	"not_found": "ERROR_CLASS_NOT_FOUND", "infrastructure": "ERROR_CLASS_INFRASTRUCTURE",
	"rate_limited": "ERROR_CLASS_RATE_LIMITED",
	"preempted":    "ERROR_CLASS_PREEMPTED",
}

var backoffStrategyNames = map[string]string{
	"fixed": "BACKOFF_STRATEGY_FIXED", "linear": "BACKOFF_STRATEGY_LINEAR",
	"exponential": "BACKOFF_STRATEGY_EXPONENTIAL",
}

var idempotencyModeNames = map[string]string{
	"none": "IDEMPOTENCY_MODE_NONE", "best_effort": "IDEMPOTENCY_MODE_BEST_EFFORT",
	"required": "IDEMPOTENCY_MODE_REQUIRED",
}

var triggerRuleNames = map[string]string{
	"all_success": "TRIGGER_RULE_ALL_SUCCESS", "all_done": "TRIGGER_RULE_ALL_DONE",
	"all_failed": "TRIGGER_RULE_ALL_FAILED", "one_success": "TRIGGER_RULE_ONE_SUCCESS",
	"one_failed": "TRIGGER_RULE_ONE_FAILED", "none_failed": "TRIGGER_RULE_NONE_FAILED",
}

// rewriteEnums walks the document in place and reports whether it changed anything.
func rewriteEnums(node any, field string) bool {
	changed := false
	switch v := node.(type) {
	case map[string]any:
		for k, child := range v {
			if s, ok := child.(string); ok {
				if names, isEnum := enumPrefixes[k]; isEnum {
					if proto, known := names[s]; known {
						v[k] = proto
						changed = true
					}
					continue
				}
			}
			if rewriteEnums(child, k) {
				changed = true
			}
		}
	case []any:
		names, isEnum := enumPrefixes[field]
		for i, child := range v {
			if s, ok := child.(string); ok && isEnum {
				if proto, known := names[s]; known {
					v[i] = proto
					changed = true
				}
				continue
			}
			if rewriteEnums(child, field) {
				changed = true
			}
		}
	}
	return changed
}
