// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package store

import "testing"

// TestAuditOutcomesAreTheThree: the trail is filtered and exported by
// outcome, and an outcome outside the three ("OK", which tenant onboarding
// wrote for every tenant it created) is a row no filter finds.
func TestAuditOutcomesAreTheThree(t *testing.T) {
	for _, s := range []string{AuditSuccess, AuditDenied, AuditError} {
		if !ValidAuditOutcome(s) {
			t.Errorf("%q rejected", s)
		}
	}
	for _, s := range []string{"OK", "ok", "Success", "", "failed"} {
		if ValidAuditOutcome(s) {
			t.Errorf("%q accepted", s)
		}
	}
}
