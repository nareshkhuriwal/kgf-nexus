// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package auditchain

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestToKGFMapsOntoKGFAuditEvents(t *testing.T) {
	seq := int64(12)
	prev, hash := strings.Repeat("a", 64), strings.Repeat("b", 64)
	rec := Record{ID: 99, Namespace: "team-a", Seq: &seq, OccurredAt: "2026-09-25T10:11:12.345678Z",
		ActorKind: "user", ActorLabel: "ana@x", Action: "token.create", TargetKind: "token", TargetID: "t/1",
		Outcome: "success", Detail: json.RawMessage(`{"scopes":["read"]}`), RequestID: "req_7",
		KGF:      &KGF{Organization: "org-1", Subscription: "sub-1", ResourceGroup: "rg-1", Principal: "svc:portal", OnBehalfOf: "user:ana"},
		PrevHash: &prev, RowHash: &hash}
	ev := ToKGF(rec)
	if ev.OrganizationID != "org-1" || ev.ID != "nexus:team-a:12" || ev.Action != "nexus.token.create" ||
		ev.SubscriptionID != "sub-1" || ev.ResourceGroup != "rg-1" || ev.CreatedAt != "2026-09-25T10:11:12.345678Z" ||
		ev.Principal != "svc:portal" || ev.OnBehalfOf != "user:ana" || ev.RequestID != "req_7" || !ev.Attributed {
		t.Fatalf("event = %+v", ev)
	}
	if ev.ResourceID != "/organizations/org-1/subscriptions/sub-1/resourceGroups/rg-1/providers/KGF.Nexus/namespaces/team-a/token/t%2F1" {
		t.Errorf("resource_id = %s", ev.ResourceID)
	}
	if ev.Chain == nil || ev.Chain.RowHash != hash || ev.Chain.Seq != 12 || ev.Nexus.AuditID != 99 {
		t.Errorf("chain/nexus = %+v %+v", ev.Chain, ev.Nexus)
	}
	// The KGF event carries every hashed field, so the row stays verifiable.
	b, _ := json.Marshal(ev)
	for _, k := range []string{`"organization_id"`, `"id"`, `"action"`, `"resource_id"`, `"created_at"`} {
		if !strings.Contains(string(b), k) {
			t.Errorf("missing KGF column %s: %s", k, b)
		}
	}

	// Without a verified KGF context: Nexus identity, a nexus:// resource.
	rec.KGF = nil
	ev = ToKGF(rec)
	if ev.OrganizationID != "" || ev.Attributed || ev.Principal != "nexus:user:ana@x" ||
		ev.ResourceID != "nexus://namespaces/team-a/token/t%2F1" {
		t.Errorf("unattributed event = %+v", ev)
	}
}
