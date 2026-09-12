// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package rest

import (
	"net/http"
	"time"

	"github.com/khuriwalgroup/kgf-nexus/internal/store"
	"github.com/khuriwalgroup/kgf-nexus/services/api-gateway/authn"
)

// attention serves the console's "needs attention" panel: a bounded, ranked list
// of conditions an operator should look at now.
//
// ONE store call, not one per condition. The panel renders as a unit, so five
// round trips would make its latency the sum of five plans and -- worse -- would
// compose five separately-timed snapshots into one view, where a run could
// appear as both "failed" and "still running".
//
// Every item carries its own link target and sentence, so the panel needs no
// follow-up request to render a row. That is the difference between a triage
// list and a list of links.
func (a *API) attention(w http.ResponseWriter, r *http.Request, _ authn.Principal) {
	f := store.AttentionFilter{
		Limit:        intParam(r, "limit", store.DefaultAttentionLimit),
		BlockedFor:   durationParam(r, "blocked_for", store.DefaultAttentionBlockedFor),
		FailedWithin: durationParam(r, "failed_within", store.DefaultAttentionFailedWithin),
		StalledFor:   durationParam(r, "stalled_for", store.DefaultAttentionStalledFor),
	}

	items, err := store.GatewayListAttention(r.Context(), a.db, r.PathValue("ns"), f)
	if err != nil {
		a.fail(w, r, "list attention", err)
		return
	}

	out := make([]map[string]any, 0, len(items))
	for _, it := range items {
		out = append(out, attentionPayload(it))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items": out,
		// The count is the number RETURNED, and the flag says whether the cap
		// truncated it. Stated explicitly rather than left for the client to
		// infer from len(items) == limit, because that inference is wrong
		// exactly when the list happens to be the cap's length, and an operator
		// who believes they are seeing everything when they are not is the
		// failure this panel exists to prevent.
		"count":     len(items),
		"truncated": len(items) == f.EffectiveLimit(),
	})
}

// attentionPayload renders one item.
//
// Written out field by field rather than by tagging the store struct, matching
// runPayload and attemptPayload: the wire shape is the API's contract and the
// store struct is an internal read model, and letting the two be the same type
// makes every future store field an accidental API change.
func attentionPayload(it store.AttentionItem) map[string]any {
	p := map[string]any{
		"kind":     it.Kind,
		"severity": it.Severity,
		"summary":  it.Summary,
		"detail":   it.Detail,
		// The link target, resolved server-side so the UI never has to build a
		// URL out of a kind. Deep-linked to the node or attempt where there is
		// one, because the operator's next move is to look at that specific
		// thing, not at the run's overview.
		"link":        attentionLink(it),
		"run_id":      it.RunID,
		"run_state":   it.RunState,
		"spec_name":   it.SpecName,
		"occurred_at": it.OccurredAt,
	}
	// Absent rather than empty for the optional identifiers: a row that has no
	// node is not a row with a node named "". The UI can then test presence
	// rather than truthiness.
	if it.NodeID != "" {
		p["node_id"] = it.NodeID
	}
	if it.AttemptID != "" {
		p["attempt_id"] = it.AttemptID
	}
	// reason is the machine-readable code behind the sentence. The UI keys its
	// call to action off this -- `attempt retry --force` is correct for exactly
	// one value and wrong for every other -- so it is exposed alongside the
	// prose rather than folded into it.
	if it.Reason != "" {
		p["reason"] = it.Reason
	}
	return p
}

// attentionLink builds the console path for an item.
//
// Server-side because the mapping from kind to destination is a product
// decision, and duplicating it in the UI would let the two drift; the panel
// would then send an operator to a page that does not show the thing the row is
// about.
func attentionLink(it store.AttentionItem) string {
	base := "/runs/" + it.RunID
	switch {
	case it.AttemptID != "":
		return base + "?attempt=" + it.AttemptID
	case it.NodeID != "":
		return base + "?node=" + it.NodeID
	default:
		return base
	}
}

// durationParam reads a Go duration query parameter, falling back to def.
//
// A duration string ("5m", "2h") rather than a bare number of seconds, because
// the three windows this endpoint takes have very different natural units and a
// bare integer would need a documented unit per parameter that nothing enforces.
// An unparseable value falls back rather than 400s, matching intParam: a
// malformed window is a client bug that should still render the panel, not an
// error page during the incident the panel exists for.
func durationParam(r *http.Request, name string, def time.Duration) time.Duration {
	v := r.URL.Query().Get(name)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return def
	}
	return d
}
