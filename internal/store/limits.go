// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package store

// Page sizes for the listings the gateway serves: the default when a caller
// names none, and the most one page may hold.
//
// Declared once, here, because two layers apply them and they must agree: the
// gateway validates and caps the `limit` query parameter (rest.listLimit) and
// the store clamps again (ClampLimit) so a caller that is not the gateway cannot
// ask Postgres for an unbounded page either. A gateway cap above the store's
// would silently shrink a page the handler had promised.
const (
	DefaultRunPage     = 100
	MaxRunPage         = 500
	DefaultCatalogPage = 100
	MaxCatalogPage     = 500
	DefaultVersionPage = 50
	MaxVersionPage     = 500
	DefaultEventPage   = 500
	MaxEventPage       = 1000
	// A worker's terminal-attempt tail: 25 fills a screen without turning a
	// detail page into a history export, and the cap is low because there is
	// no index on worker_id for terminal attempt states.
	DefaultWorkerTail  = 25
	MaxWorkerTail      = 200
	DefaultAuditPage   = 100
	MaxAuditPage       = 1000
	DefaultUserList    = 100
	MaxUserList        = 500
	DefaultSessionPage = 50
	MaxSessionPage     = 200
	DefaultTokenPage   = 100
	MaxTokenPage       = 500
	DefaultSchedule    = 100
	MaxSchedulePage    = 500
	DefaultBackfill    = 50
	MaxBackfillPage    = 200
	DefaultDelivery    = 100
	MaxDeliveryPage    = 500
	DefaultReceipts    = 50
	MaxReceiptsPage    = 200
	DefaultObservation = 50
	MaxObservationPage = 500
	DefaultAssetPage   = 200
	MaxAssetPage       = 1000
	DefaultUsagePage   = 100
	MaxUsagePage       = 500
	MaxAttentionItems  = 200
)

// ClampLimit is the store's half of the page-size rule: a non-positive limit
// means def, and anything above max is capped AT max.
//
// Capped, not reset to the default. Several listings used to answer a limit
// over their maximum with their default page, so limit=600 on the run list
// returned 100 rows -- fewer than limit=101 would have -- and the audit listing
// did the same above 1000.
func ClampLimit(limit, def, max int) int {
	switch {
	case limit <= 0:
		return def
	case limit > max:
		return max
	default:
		return limit
	}
}
