// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package dag

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
)

// fanInOf feeds n children whose value is a string of `size` chars, i.e. an
// element of size+2 bytes, and returns the builder and the first error.
func fanInOf(n, size int, limit int64) (*FanInBuilder, error) {
	b := NewFanInBuilder("process", "result", n, limit)
	for i := 0; i < n; i++ {
		v, _ := json.Marshal(strings.Repeat("x", size))
		ref, _ := InlineJSONRef(v)
		if err := b.Add(ref); err != nil {
			return b, err
		}
	}
	return b, nil
}

// exactSize is the assembled array for fanInOf: elements, commas, brackets.
func exactSize(n, size int) int64 { return int64(n*(size+2) + (n - 1) + 2) }

func TestFanInBuilder_UnderAtAndOverTheCap(t *testing.T) {
	const n, size = 20, 100
	exact := exactSize(n, size)

	b, err := fanInOf(n, size, exact+1)
	if err != nil {
		t.Fatalf("under the cap refused: %v", err)
	}
	ref, _ := b.Finish()
	payload, _, _ := inlinePayload(ref)
	if int64(len(payload)) != exact {
		t.Fatalf("assembled %d bytes, the size model says %d", len(payload), exact)
	}

	if _, err := fanInOf(n, size, exact); err != nil {
		t.Fatalf("exactly at the cap must be accepted: %v", err)
	}

	b, err = fanInOf(n, size, exact-1)
	var tl *FanInTooLarge
	if !errors.As(err, &tl) {
		t.Fatalf("one byte over the cap: err = %v, want *FanInTooLarge", err)
	}
	if tl.Limit != exact-1 || tl.Children != n-1 {
		t.Errorf("refusal = %+v, want limit %d after %d children", tl, exact-1, n-1)
	}
	if !strings.Contains(tl.Error(), "NEXUS_DAG_MAX_FANIN_BYTES") || !strings.Contains(tl.Error(), FanInTooLargeReason) {
		t.Errorf("reason does not say how to fix it: %s", tl.Error())
	}
	// Bounded memory: the builder never held more than the limit.
	if int64(b.Size()) > exact-1 {
		t.Errorf("builder holds %d bytes past a %d limit", b.Size(), exact-1)
	}
}

// The early stop: with a tiny limit and a huge fan-out, the builder stops at
// the first element that crosses it rather than building everything.
func TestFanInBuilder_StopsEarly(t *testing.T) {
	b, err := fanInOf(10000, 1000, 5000)
	var tl *FanInTooLarge
	if !errors.As(err, &tl) {
		t.Fatalf("err = %v", err)
	}
	if tl.Children > 5 || b.Size() > 5000 {
		t.Fatalf("accepted %d children / %d bytes before stopping at a 5000-byte limit", tl.Children, b.Size())
	}
}

func TestValidate_StaticFanInBound(t *testing.T) {
	spec := mapSpec(1000, nexusv1.TriggerRule_TRIGGER_RULE_ALL_SUCCESS)
	spec.Tasks[1].Outputs["result"].MaxBytes = 1 << 10 // 1000 x 1 KiB ~ 1 MiB
	if _, errs := Validate(spec, ValidationOptions{MaxFanInBytes: 2 << 20}); errs.Has(ErrMapFanInSize) {
		t.Fatalf("a fan-in that always fits was rejected: %v", errs)
	}
	_, errs := Validate(spec, ValidationOptions{MaxFanInBytes: 512 << 10})
	if !errs.Has(ErrMapFanInSize) {
		t.Fatalf("a fan-in that can exceed the limit was accepted: %v", errs)
	}
	// Undeclared size: nothing to check statically; the runtime bound applies.
	spec.Tasks[1].Outputs["result"].MaxBytes = 0
	if _, errs := Validate(spec, ValidationOptions{MaxFanInBytes: 512 << 10}); errs.Has(ErrMapFanInSize) {
		t.Fatalf("undeclared size rejected: %v", errs)
	}
}
