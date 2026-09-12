// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package runs

import (
	"testing"
)

// A spec declares which params are required; until this check existed nothing
// read that declaration, so a run omitting one was accepted, dispatched, and
// failed inside the task -- burning a retry budget on a problem no retry fixes
// and reporting a bad request as an execution failure.
func TestMissingRequiredParamsNamesEveryOmission(t *testing.T) {
	spec := []byte(`{"params":{
		"order_id":{"type":"string","required":true},
		"amount_cents":{"type":"int","required":true},
		"note":{"type":"string"}
	}}`)

	got := missingRequiredParams(spec, map[string]any{"note": "hello"})

	// Sorted, so two identical requests produce identical messages; an error
	// whose wording shifts between attempts cannot be matched against a log.
	if len(got) != 2 || got[0] != "amount_cents" || got[1] != "order_id" {
		t.Errorf("missing = %v, want [amount_cents order_id]", got)
	}
}

func TestMissingRequiredParamsAcceptsACompleteRequest(t *testing.T) {
	spec := []byte(`{"params":{"source":{"type":"string","required":true}}}`)

	if got := missingRequiredParams(spec, map[string]any{"source": "s3://bucket/key"}); len(got) != 0 {
		t.Errorf("missing = %v, want none", got)
	}
}

// A defaulted param is not missing: the default is what makes it optional in
// practice, and rejecting it would break every run relying on the spec's own
// fallback.
func TestMissingRequiredParamsIgnoresDefaultedParams(t *testing.T) {
	spec := []byte(`{"params":{
		"region":{"type":"string","required":true,"default_json":"\"eu-west-1\""}
	}}`)

	if got := missingRequiredParams(spec, nil); len(got) != 0 {
		t.Errorf("a param with a default was reported missing: %v", got)
	}
}

// Presence, not type. Declaring "int" and passing a string is a type error the
// task surfaces with far better context; a half-implemented checker here would
// be a second, weaker opinion about the same question.
func TestMissingRequiredParamsChecksPresenceNotType(t *testing.T) {
	spec := []byte(`{"params":{"amount":{"type":"int","required":true}}}`)

	if got := missingRequiredParams(spec, map[string]any{"amount": "not-a-number"}); len(got) != 0 {
		t.Errorf("a present-but-wrongly-typed param was reported missing: %v", got)
	}
}

// A spec with no params, an empty spec, and an unparseable one must all be
// permissive: a malformed spec is parsePolicy's error to report, and duplicating
// it here would give one fault two different messages.
func TestMissingRequiredParamsIsPermissiveWhereItCannotJudge(t *testing.T) {
	for name, spec := range map[string][]byte{
		"no params": []byte(`{"tasks":[]}`),
		"empty":     nil,
		"malformed": []byte(`{not json`),
	} {
		if got := missingRequiredParams(spec, nil); len(got) != 0 {
			t.Errorf("%s: reported %v, want none", name, got)
		}
	}
}
