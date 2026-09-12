package spec

import "strings"

// NormalizeEnums rewrites every enum-valued field in a validated document to its
// SHORT canonical spelling, in place.
//
// Why normalize at all, rather than merely accepting both spellings: the stored
// document is CONTENT-ADDRESSED (ADR-0004). Without this, the same workflow
// written by the Python SDK ("IDEMPOTENCY_MODE_REQUIRED") and by hand
// ("required") would canonicalize to different bytes and therefore to different
// sha256 spec versions. The platform would report them as two distinct
// workflows, "resubmitting a byte-identical spec is a no-op" would quietly stop
// holding across authoring surfaces, and a team that moved a workflow from YAML
// to the SDK would see a spurious new version with no semantic change.
// Normalizing before canonicalization is what makes ADR-0004's "both surfaces
// produce the identical document" literally true of the stored bytes.
//
// SHORT is the canonical direction because it is the published authoring form:
// architecture 4.3's YAML, the schema's `default` keywords and the documentation
// all use it, and storing the protobuf constant would mean the document an
// operator reads back does not match the document the docs told them to write.
// The proto spelling remains ACCEPTED on input forever; only the stored form is
// pinned.
//
// This runs AFTER ValidateStructure, so every value it sees is already one of
// the two known spellings and an unrecognized string is impossible. Anything it
// does not recognize is therefore left untouched rather than defaulted, since
// silently rewriting an unknown value would be worse than storing it verbatim.
func NormalizeEnums(doc any) {
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
		normalizeField(t, "idempotency", idempotencyAliases)
		normalizeField(t, "trigger_rule", triggerRuleAliases)

		r, ok := t["retry"].(map[string]any)
		if !ok {
			continue
		}
		normalizeField(r, "strategy", backoffAliases)
		normalizeList(r, "retry_on", errorClassAliases)
		normalizeList(r, "no_retry_on", errorClassAliases)
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

// The alias tables map BOTH spellings to the canonical short form, so a document
// already in canonical form passes through unchanged and the function is
// idempotent — which matters because a resubmitted stored document must hash to
// what it hashed to the first time.
//
// They are built from the proto constant's prefix rather than written out twice,
// so adding an enum value cannot leave the two spellings out of step.

var (
	idempotencyAliases = aliasTable("IDEMPOTENCY_MODE_", "required", "best_effort", "none")
	triggerRuleAliases = aliasTable("TRIGGER_RULE_",
		"all_success", "all_done", "one_success", "one_failed", "none_failed", "all_skipped")
	backoffAliases    = aliasTable("BACKOFF_STRATEGY_", "fixed", "linear", "exponential")
	errorClassAliases = errorClassTable()
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
// correspond (CONTRACT-FINDINGS.md F20). It normalizes to `validation`, the
// spelling the schema's own default and the proto agree on.
func errorClassTable() map[string]string {
	out := aliasTable("ERROR_CLASS_",
		"transient", "timeout", "worker_lost", "validation", "permission_denied",
		"application", "oom", "not_found", "infrastructure")
	out["validation_error"] = "validation"
	return out
}
