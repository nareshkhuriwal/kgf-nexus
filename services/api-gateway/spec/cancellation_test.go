// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package spec

import (
	"fmt"
	"testing"
)

func cancellationSpec(policy string) []byte {
	return []byte(fmt.Sprintf(`{"api_version":"nexus.dev/v1","namespace":"ns","name":"w",
 "tasks":[{"id":"a","task_ref":"pay.charge:v1","idempotency":"none","cancellation":%s}]}`, policy))
}

// TestUninterruptibleDeclarationReachesTheWireSpec pins ADR-0015's authoring
// surface: the declaration and its bound survive schema validation and the
// hand-written proto conversion, so the agent actually receives them.
func TestUninterruptibleDeclarationReachesTheWireSpec(t *testing.T) {
	c, err := Compile(cancellationSpec(`{"uninterruptible":true,"max_uninterruptible_ms":120000}`), FormatJSON)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	cp := c.Proto.GetTasks()[0].GetCancellation()
	if !cp.GetUninterruptible() || cp.GetMaxUninterruptibleMs() != 120000 {
		t.Fatalf("cancellation policy lost in conversion: %+v", cp)
	}

	// Absent means interruptible, and must stay absent rather than become an
	// explicit false: a spec that never mentions cancellation must hash the
	// same as it did before the field existed.
	plain, err := Compile(cancellationSpec(`{}`), FormatJSON)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if plain.Proto.GetTasks()[0].GetCancellation().GetUninterruptible() {
		t.Fatal("an empty cancellation block became uninterruptible")
	}
}

// TestUninterruptibleBoundsAreEnforcedAtSubmit: an unbounded or over-long wait
// and a bound without the declaration are both rejected before a run exists.
func TestUninterruptibleBoundsAreEnforcedAtSubmit(t *testing.T) {
	for name, policy := range map[string]string{
		"over the one-hour schema maximum": `{"uninterruptible":true,"max_uninterruptible_ms":3600001}`,
		"negative bound":                   `{"uninterruptible":true,"max_uninterruptible_ms":-1}`,
		"bound without the declaration":    `{"max_uninterruptible_ms":60000}`,
		"bound with declaration false":     `{"uninterruptible":false,"max_uninterruptible_ms":60000}`,
		"unknown field":                    `{"uninterruptible":true,"forever":true}`,
	} {
		if _, err := Compile(cancellationSpec(policy), FormatJSON); err == nil {
			t.Errorf("%s: accepted %s", name, policy)
		}
	}
	// The ceiling-by-default form is valid: the worker supplies the bound.
	if _, err := Compile(cancellationSpec(`{"uninterruptible":true}`), FormatJSON); err != nil {
		t.Errorf("uninterruptible with no explicit bound rejected: %v", err)
	}
}
