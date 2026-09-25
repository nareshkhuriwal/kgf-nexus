// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

// Package specenum rewrites the enum-valued fields of a decoded WorkflowSpec
// document to their SHORT canonical spelling ("required", not
// "IDEMPOTENCY_MODE_REQUIRED").
//
// It is shared by everything that computes a spec's content address: the
// gateway (services/api-gateway/spec, before it canonicalizes and stores a
// submitted spec) and nexusctl (cli/internal/spec, before it prints or compares
// the hash it expects). The content address is sha256 of the canonical bytes
// of the NORMALIZED document, so a client that hashed the author's spelling
// computed a different address from the server for every spec with an enum in
// it (CONTRACT-FINDINGS F124). The Python SDK carries the same tables
// (sdk/python/nexus/canonical.py, normalize_enums); the golden file
// sdk/python/tests/parity/spec_hash_golden.json holds all three to one answer.
//
// Why SHORT, and why normalize at all: see services/api-gateway/spec/normalize.go
// (F52). The proto spelling stays accepted on input forever; only the stored,
// hashed form is pinned.
package specenum

import "strings"

// Normalize rewrites every enum-valued field of a decoded document in place.
// Values it does not recognize are left untouched, and a document already in
// canonical form passes through unchanged, so it is idempotent.
func Normalize(doc any) {
	m, ok := doc.(map[string]any)
	if !ok {
		return
	}
	tasks, _ := m["tasks"].([]any)
	for _, raw := range tasks {
		t, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		normalizeField(t, "idempotency", IdempotencyAliases)
		normalizeField(t, "trigger_rule", TriggerRuleAliases)

		r, ok := t["retry"].(map[string]any)
		if !ok {
			continue
		}
		normalizeField(r, "strategy", BackoffAliases)
		normalizeList(r, "retry_on", ErrorClassAliases)
		normalizeList(r, "no_retry_on", ErrorClassAliases)
	}
}

func normalizeField(m map[string]any, key string, aliases map[string]string) {
	s, ok := m[key].(string)
	if !ok {
		return
	}
	if canonical, known := aliases[s]; known {
		m[key] = canonical
	}
}

func normalizeList(m map[string]any, key string, aliases map[string]string) {
	items, ok := m[key].([]any)
	if !ok {
		return
	}
	for i, raw := range items {
		s, ok := raw.(string)
		if !ok {
			continue
		}
		if canonical, known := aliases[s]; known {
			items[i] = canonical
		}
	}
}

// The alias tables map BOTH spellings to the canonical short form. They are
// built from the proto constant's prefix rather than written out twice, so
// adding an enum value cannot leave the two spellings out of step.
//
// Exported read-only for the parity tests; never mutate them.
var (
	IdempotencyAliases = aliasTable("IDEMPOTENCY_MODE_", "required", "best_effort", "none")
	TriggerRuleAliases = aliasTable("TRIGGER_RULE_",
		"all_success", "all_done", "one_success", "one_failed", "none_failed", "all_skipped")
	BackoffAliases    = aliasTable("BACKOFF_STRATEGY_", "fixed", "linear", "exponential")
	ErrorClassAliases = errorClassTable()
)

func aliasTable(prefix string, shortForms ...string) map[string]string {
	out := make(map[string]string, len(shortForms)*2)
	for _, s := range shortForms {
		out[s] = s
		out[prefix+strings.ToUpper(s)] = s
	}
	return out
}

// errorClassTable needs a hand-written entry that aliasTable cannot derive:
// `validation_error` is architecture 4.3's spelling for the class common.proto
// calls ERROR_CLASS_VALIDATION, so the short form and the proto suffix do not
// correspond (CONTRACT-FINDINGS F20). It normalizes to `validation`, the
// spelling the schema's own default and the proto agree on.
func errorClassTable() map[string]string {
	out := aliasTable("ERROR_CLASS_",
		"transient", "timeout", "worker_lost", "validation", "permission_denied",
		"application", "oom", "not_found", "infrastructure")
	out["validation_error"] = "validation"
	return out
}
