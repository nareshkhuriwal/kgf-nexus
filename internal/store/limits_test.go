// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package store

import "testing"

func TestClampLimitCapsRatherThanResetting(t *testing.T) {
	for _, tc := range []struct{ in, want int }{
		{-3, 100}, {0, 100}, {1, 1}, {500, 500}, {501, 500}, {1 << 30, 500},
	} {
		if got := ClampLimit(tc.in, 100, 500); got != tc.want {
			t.Errorf("ClampLimit(%d, 100, 500) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// TestPageDefaultsFitTheirMaximums: a default above its own cap would be
// clamped on every call that names no limit.
func TestPageDefaultsFitTheirMaximums(t *testing.T) {
	for name, pair := range map[string][2]int{
		"run": {DefaultRunPage, MaxRunPage}, "catalog": {DefaultCatalogPage, MaxCatalogPage},
		"version": {DefaultVersionPage, MaxVersionPage}, "event": {DefaultEventPage, MaxEventPage},
		"worker": {DefaultWorkerTail, MaxWorkerTail}, "audit": {DefaultAuditPage, MaxAuditPage},
		"users": {DefaultUserList, MaxUserList}, "sessions": {DefaultSessionPage, MaxSessionPage},
		"tokens": {DefaultTokenPage, MaxTokenPage}, "schedules": {DefaultSchedule, MaxSchedulePage},
		"backfills": {DefaultBackfill, MaxBackfillPage}, "deliveries": {DefaultDelivery, MaxDeliveryPage},
		"receipts": {DefaultReceipts, MaxReceiptsPage}, "observations": {DefaultObservation, MaxObservationPage},
		"assets": {DefaultAssetPage, MaxAssetPage}, "usage": {DefaultUsagePage, MaxUsagePage},
		"attention": {DefaultAttentionLimit, MaxAttentionItems}, "users page": {100, MaxUsersPage},
	} {
		if pair[0] < 1 || pair[0] > pair[1] {
			t.Errorf("%s: default %d does not fit its maximum %d", name, pair[0], pair[1])
		}
	}
}
