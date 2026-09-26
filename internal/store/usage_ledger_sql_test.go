// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"strings"
	"testing"
)

// TestLedgerStatementCarriesUsageUnits guards the string replacements that
// derive the 0036 ledger statement from the 0027 one. If recordUsageSQL is
// edited so a replacement no longer matches, the derived statement would
// silently stop recording usage units; this fails instead.
func TestLedgerStatementCarriesUsageUnits(t *testing.T) {
	if recordUsageWithUnitsSQL == recordUsageSQL {
		t.Fatal("no replacement applied")
	}
	for _, want := range []string{
		"|| COALESCE((SELECT u.units FROM attempt_usage u WHERE u.attempt_id = a.id), '{}'::jsonb) AS custom",
		"AS usage_units",
		"'usage_units', usage_units)",
	} {
		if !strings.Contains(recordUsageWithUnitsSQL, want) {
			t.Errorf("derived ledger statement lacks %q", want)
		}
	}
	if strings.Contains(recordUsageSQL, "attempt_usage") {
		t.Error("the pre-0036 statement must not reference attempt_usage")
	}
}
