// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package worker

import (
	"context"
	"testing"

	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
)

// TestNoStoreWorkerHonoursTheConfiguredInlineThreshold: a worker with no object
// store used to compare against the compiled-in 64 KiB default, so an operator
// who LOWERED the threshold still had store-less workers inlining up to 64 KiB
// through the control plane. It must use the configured value (ADR-0016).
func TestNoStoreWorkerHonoursTheConfiguredInlineThreshold(t *testing.T) {
	c := &fakeReportClient{}
	r, reg, _ := newTestRunner(t, c)
	r.InlineThreshold = 16
	a := assignment("inl1", `printf '{"payload":"more than sixteen bytes"}' > "$NEXUS_OUTPUT_FILE"`)
	if ok, reason := reg.Admit(a.GetAttemptId(), a.GetSpec().GetResources().GetRequest()); !ok {
		t.Fatalf("admit: %s", reason)
	}
	r.Run(context.Background(), a)

	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.results) != 1 {
		t.Fatalf("want one report, got %d", len(c.results))
	}
	res := c.results[0]
	if res.GetOutcome() == nexusv1.AttemptOutcome_ATTEMPT_OUTCOME_SUCCEEDED {
		t.Fatalf("a %d-byte result was inlined past a 16-byte threshold: %v",
			len(`{"payload":"more than sixteen bytes"}`), res.GetOutputs())
	}
	if res.GetError().GetClass() != nexusv1.ErrorClass_ERROR_CLASS_INFRASTRUCTURE {
		t.Errorf("class = %v, want INFRASTRUCTURE (no store to hold the result)", res.GetError().GetClass())
	}
}
