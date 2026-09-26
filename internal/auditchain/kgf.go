// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package auditchain

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

// KGFSource names Nexus as the producer in a KGF audit event.
const KGFSource = "kgf-nexus"

// KGFEvent is one Nexus audit row in KGF Cloud's audit event shape.
//
// KGF Cloud records audit events as (organization_id, id, action,
// resource_id, subscription_id, resource_group, created_at) -- the columns
// of its audit_events table and of its project export. Those fields come
// first, named and typed as KGF names them; everything Nexus knows beyond
// that travels alongside so nothing is lost in the mapping, including the
// chain position and hashes, so tamper evidence survives the hand-over.
// The full mapping is documented in OPERATIONS §25.2.
type KGFEvent struct {
	OrganizationID string `json:"organization_id"`
	ID             string `json:"id"`
	Action         string `json:"action"`
	ResourceID     string `json:"resource_id"`
	SubscriptionID string `json:"subscription_id,omitempty"`
	ResourceGroup  string `json:"resource_group,omitempty"`
	CreatedAt      string `json:"created_at"`

	// Who: the verified KGF principal when there was one, otherwise the
	// Nexus principal ("nexus:<actor_kind>:<label>").
	Principal  string `json:"principal"`
	OnBehalfOf string `json:"on_behalf_of,omitempty"`
	RequestID  string `json:"request_id,omitempty"`
	Outcome    string `json:"outcome"`
	Source     string `json:"source"`
	// Attributed is true when organization and principal come from a
	// verified KGF context rather than from Nexus's own identity.
	Attributed bool `json:"kgf_attributed"`

	Nexus KGFNexusDetail `json:"nexus"`
	Chain *KGFChain      `json:"chain,omitempty"`
}

// KGFNexusDetail carries the Nexus-side facts KGF's schema has no column for.
type KGFNexusDetail struct {
	Namespace  string          `json:"namespace"`
	AuditID    int64           `json:"audit_id"`
	Action     string          `json:"action"`
	ActorID    *string         `json:"actor_id,omitempty"`
	ActorKind  string          `json:"actor_kind"`
	ActorLabel string          `json:"actor_label"`
	TargetKind string          `json:"target_kind"`
	TargetID   string          `json:"target_id"`
	ClientIP   string          `json:"client_ip,omitempty"`
	Detail     json.RawMessage `json:"detail"`
}

// KGFChain is the row's place in the Nexus hash chain.
type KGFChain struct {
	Encoding string `json:"encoding"`
	Seq      int64  `json:"seq"`
	PrevHash string `json:"prev_hash"`
	RowHash  string `json:"row_hash"`
}

// ToKGF maps an export record to KGF Cloud's audit event shape.
func ToKGF(rec Record) KGFEvent {
	k := KGF{}
	if rec.KGF != nil {
		k = *rec.KGF
	}
	ev := KGFEvent{
		OrganizationID: k.Organization,
		Action:         "nexus." + rec.Action,
		SubscriptionID: k.Subscription,
		ResourceGroup:  k.ResourceGroup,
		OnBehalfOf:     k.OnBehalfOf,
		RequestID:      rec.RequestID,
		Outcome:        rec.Outcome,
		Source:         KGFSource,
		Attributed:     k.Organization != "" && k.Principal != "",
		Nexus: KGFNexusDetail{
			Namespace: rec.Namespace, AuditID: rec.ID, Action: rec.Action, ActorID: rec.ActorID,
			ActorKind: rec.ActorKind, ActorLabel: rec.ActorLabel, TargetKind: rec.TargetKind,
			TargetID: rec.TargetID, ClientIP: rec.ClientIP, Detail: rec.Detail,
		},
	}
	// A stable id: the same row always maps to the same KGF event id, so a
	// redelivered batch de-duplicates on KGF's (organization_id, id) key.
	if rec.Seq != nil {
		ev.ID = "nexus:" + rec.Namespace + ":" + strconv.FormatInt(*rec.Seq, 10)
		ev.Chain = &KGFChain{Encoding: Version, Seq: *rec.Seq}
		if rec.PrevHash != nil {
			ev.Chain.PrevHash = *rec.PrevHash
		}
		if rec.RowHash != nil {
			ev.Chain.RowHash = *rec.RowHash
		}
	} else {
		ev.ID = "nexus:" + rec.Namespace + ":id-" + strconv.FormatInt(rec.ID, 10)
	}
	if t, err := time.Parse(TimeLayout, rec.OccurredAt); err == nil {
		ev.CreatedAt = t.UTC().Format(time.RFC3339Nano)
	} else {
		ev.CreatedAt = rec.OccurredAt
	}
	if k.Principal != "" {
		ev.Principal = k.Principal
	} else {
		who := rec.ActorLabel
		if who == "" && rec.ActorID != nil {
			who = *rec.ActorID
		}
		ev.Principal = "nexus:" + rec.ActorKind + ":" + who
	}
	ev.ResourceID = kgfResourceID(k, rec)
	return ev
}

// kgfResourceID renders the target as a KGF ARM-style scope when the KGF
// scope is known (/organizations/{org}/subscriptions/{sub}/resourceGroups/
// {rg}/providers/KGF.Nexus/namespaces/{ns}/{kind}/{id}, the form KGF's authz
// Scope.String produces), and as a nexus:// URI otherwise.
func kgfResourceID(k KGF, rec Record) string {
	tail := "namespaces/" + rec.Namespace
	if rec.TargetKind != "" {
		tail += "/" + rec.TargetKind
		if rec.TargetID != "" {
			tail += "/" + strings.ReplaceAll(rec.TargetID, "/", "%2F")
		}
	}
	if k.Organization != "" && k.Subscription != "" && k.ResourceGroup != "" {
		return "/organizations/" + k.Organization + "/subscriptions/" + k.Subscription +
			"/resourceGroups/" + k.ResourceGroup + "/providers/KGF.Nexus/" + tail
	}
	return "nexus://" + tail
}
