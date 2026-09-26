// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0
package authn

// KGF Cloud context authentication (ADR-0024): a request forwarded by
// kgf-gateway carries a signed X-KGF-Context; this file turns a verified
// context into an ordinary Nexus principal of the namespace mapped from the
// KGF scope, holding exactly the Nexus permissions its KGF role grants.

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/khuriwalgroup/kgf-nexus/internal/kgfctx"
	"github.com/khuriwalgroup/kgf-nexus/internal/store"
)

// KindKGF is a caller authenticated by a KGF context.
const KindKGF PrincipalKind = "kgf"

// KGFIdentity is the verified KGF identity behind a principal.
type KGFIdentity struct {
	Organization  string `json:"organization"`
	Subscription  string `json:"subscription"`
	ResourceGroup string `json:"resource_group"`
	// Principal and Role are the EFFECTIVE ones: the person a delegating
	// service acts for, when there is one.
	Principal string `json:"principal"`
	Role      string `json:"role"`
	// Via is the KGF service that presented a delegated context.
	Via       string `json:"via,omitempty"`
	RequestID string `json:"request_id,omitempty"`
	Issuer    string `json:"issuer"`
	TokenID   string `json:"token_id"`
}

// AuditDetail is what an audit row records about the KGF identity.
func (k *KGFIdentity) AuditDetail() map[string]any {
	d := map[string]any{
		"organization": k.Organization, "subscription": k.Subscription, "resource_group": k.ResourceGroup,
		"principal": k.Principal, "role": k.Role, "issuer": k.Issuer, "token_id": k.TokenID,
	}
	if k.Via != "" {
		d["via"] = k.Via
	}
	return d
}

// Errors the guard maps to distinct answers.
var (
	// ErrKGFScopeRequired: the context has no subscription and resource group,
	// so there is no workload scope to map to a namespace (403).
	ErrKGFScopeRequired = errors.New("kgf context names no subscription and resource group")
	// ErrKGFServiceNotAllowed: a KGF service acting for itself that is not
	// configured in NEXUS_KGF_SERVICE_PRINCIPALS (403).
	ErrKGFServiceNotAllowed = errors.New("kgf service principal is not allowed")
)

// KGFActions maps every Nexus permission that a KGF principal can hold to the
// KGF.Nexus action that grants it (ADR-0024 §4). A permission absent from
// this map is never granted to a KGF principal: deny by default. users.* is
// deliberately absent because KGF owns people. audit.manage (retention, legal
// hold, purge and SIEM sinks, ADR-0025) is deliberately absent too: a flat
// KGF operator role is Contributor, and "may shorten retention and purge the
// evidence of its own actions" must not ride on a wildcard. It stays with the
// platform owner's Nexus machine credential (an admin API key, or a service
// account an administrator bound), and KGF's own audit pipeline receives the
// trail through a kgf-format sink that credential configures.
var KGFActions = map[string]string{
	"overview.view":              "KGF.Nexus/pipelines/read",
	"workflows.view":             "KGF.Nexus/pipelines/read",
	"workflows.publish":          "KGF.Nexus/pipelines/write",
	"workflows.delete":           "KGF.Nexus/pipelines/delete",
	"workflows.cache.invalidate": "KGF.Nexus/pipelines/cache/action",
	"runs.create":                "KGF.Nexus/pipelines/run",
	"runs.view":                  "KGF.Nexus/pipelines/runs/read",
	"tasks.view":                 "KGF.Nexus/pipelines/runs/read",
	"tasks.logs.view":            "KGF.Nexus/pipelines/runs/logs/read",
	"tasks.results.view":         "KGF.Nexus/pipelines/runs/results/read",
	"runs.cancel":                "KGF.Nexus/pipelines/runs/cancel/action",
	"runs.pause":                 "KGF.Nexus/pipelines/runs/pause/action",
	"runs.resume":                "KGF.Nexus/pipelines/runs/resume/action",
	"runs.retry":                 "KGF.Nexus/pipelines/runs/retry/action",
	"runs.delete":                "KGF.Nexus/pipelines/runs/delete",
	"schedules.view":             "KGF.Nexus/schedules/read",
	"schedules.create":           "KGF.Nexus/schedules/write",
	"schedules.update":           "KGF.Nexus/schedules/write",
	"schedules.delete":           "KGF.Nexus/schedules/delete",
	"backfills.view":             "KGF.Nexus/schedules/backfills/read",
	"backfills.create":           "KGF.Nexus/schedules/backfills/write",
	"backfills.update":           "KGF.Nexus/schedules/backfills/write",
	"workers.view":               "KGF.Nexus/workers/read",
	"workers.drain":              "KGF.Nexus/workers/drain/action",
	"workers.quarantine":         "KGF.Nexus/workers/quarantine/action",
	"infrastructure.view":        "KGF.Nexus/infrastructure/read",
	"infrastructure.configure":   "KGF.Nexus/infrastructure/write",
	"quotas.view":                "KGF.Nexus/quotas/read",
	"quotas.manage":              "KGF.Nexus/quotas/write",
	"usage.view":                 "KGF.Nexus/usage/read",
	// ADR-0028: events, triggers, sensors and lineage.
	"notifications.view":      "KGF.Nexus/notifications/read",
	"notifications.manage":    "KGF.Nexus/notifications/write",
	"triggers.view":           "KGF.Nexus/triggers/read",
	"triggers.create":         "KGF.Nexus/triggers/write",
	"triggers.update":         "KGF.Nexus/triggers/write",
	"triggers.delete":         "KGF.Nexus/triggers/delete",
	"sensors.view":            "KGF.Nexus/sensors/read",
	"sensors.create":          "KGF.Nexus/sensors/write",
	"sensors.update":          "KGF.Nexus/sensors/write",
	"sensors.delete":          "KGF.Nexus/sensors/delete",
	"assets.view":             "KGF.Nexus/assets/read",
	"audit.view":              "KGF.Nexus/auditEvents/read",
	"roles.view":              "KGF.Nexus/roles/read",
	"roles.manage":            "KGF.Nexus/roles/write",
	"service_accounts.view":   "KGF.Nexus/serviceAccounts/read",
	"service_accounts.manage": "KGF.Nexus/serviceAccounts/write",
	"tokens.view":             "KGF.Nexus/serviceAccounts/read",
	"tokens.manage":           "KGF.Nexus/serviceAccounts/write",
}

// kgfIdentityGrantActions are KGF's GrantActions (kgf-cloud pkg/authz): the
// actions Contributor subtracts.
var kgfIdentityGrantActions = []string{
	"KGF.Identity/roleAssignments/write", "KGF.Identity/roleAssignments/delete",
	"KGF.Identity/roleDefinitions/write", "KGF.Identity/roleDefinitions/delete",
	"KGF.Identity/servicePrincipals/write", "KGF.Identity/servicePrincipals/delete",
	"KGF.Identity/apiKeys/write", "KGF.Identity/apiKeys/delete", "KGF.Identity/sessions/delete",
}

// kgfRoleDefinition mirrors the built-in definitions kgf-gateway derives from
// a flat role (kgf-cloud services/gateway app.RoleAssignments and
// pkg/authz/builtin.go): operator is Contributor, viewer is Reader.
func kgfRoleDefinition(role string) (actions, notActions []string, ok bool) {
	switch role {
	case kgfctx.RoleOperator:
		return []string{"*"}, kgfIdentityGrantActions, true
	case kgfctx.RoleViewer:
		return []string{"*/read", "*/list"}, nil, true
	}
	return nil, nil, false
}

// kgfMatch is kgf-cloud pkg/authz.matchPattern: "*" matches any run of
// characters including separators; case-insensitive; no regular expressions.
func kgfMatch(pattern, action string) bool {
	pattern, action = strings.ToLower(pattern), strings.ToLower(action)
	if pattern == "*" {
		return true
	}
	if !strings.Contains(pattern, "*") {
		return pattern == action
	}
	parts := strings.Split(pattern, "*")
	if !strings.HasPrefix(action, parts[0]) {
		return false
	}
	action = action[len(parts[0]):]
	last := parts[len(parts)-1]
	if !strings.HasSuffix(action, last) {
		return false
	}
	action = action[:len(action)-len(last)]
	for _, part := range parts[1 : len(parts)-1] {
		if part == "" {
			continue
		}
		i := strings.Index(action, part)
		if i < 0 {
			return false
		}
		action = action[i+len(part):]
	}
	return true
}

// KGFRoleAllows reports whether a KGF flat role grants an action.
func KGFRoleAllows(role, action string) bool {
	actions, not, ok := kgfRoleDefinition(role)
	if !ok {
		return false
	}
	for _, p := range not {
		if kgfMatch(p, action) {
			return false
		}
	}
	for _, p := range actions {
		if kgfMatch(p, action) {
			return true
		}
	}
	return false
}

// KGFGrants is the Nexus permission set a KGF role holds.
func KGFGrants(role string) map[string]bool {
	out := map[string]bool{}
	for perm, action := range KGFActions {
		if Known(perm) && KGFRoleAllows(role, action) {
			out[perm] = true
		}
	}
	return out
}

// Provisioner maps a KGF scope to a Nexus namespace, creating it on first use.
type Provisioner interface {
	ProvisionKGFNamespace(ctx context.Context, s store.KGFScope) (string, error)
}

// StoreProvisioner provisions through the gateway's database.
type StoreProvisioner struct{ DB store.GWDB }

// ProvisionKGFNamespace implements Provisioner.
func (s StoreProvisioner) ProvisionKGFNamespace(ctx context.Context, sc store.KGFScope) (string, error) {
	return store.GatewayProvisionKGFNamespace(ctx, s.DB, sc)
}

// KGFAuthenticator verifies KGF contexts and resolves them to principals.
type KGFAuthenticator struct {
	Verifier    kgfctx.Verifier
	Provisioner Provisioner
	// ServicePrincipals maps a KGF service principal ("svc:kgf-billing") to
	// the flat role it acts with when it calls Nexus for itself. Services not
	// listed are refused (ADR-0024 §4).
	ServicePrincipals map[string]string
	Now               func() time.Time

	// provisioned caches scope → namespace. Provisioning is idempotent, so
	// the cache only saves a transaction per request; it never decides.
	provisioned sync.Map
}

// ParseServicePrincipals reads "svc:a=viewer,svc:b=operator".
func ParseServicePrincipals(s string) (map[string]string, error) {
	out := map[string]string{}
	for _, item := range strings.Split(s, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		name, role, ok := strings.Cut(item, "=")
		name, role = strings.TrimSpace(name), strings.TrimSpace(role)
		if !ok || !strings.HasPrefix(name, "svc:") || (role != kgfctx.RoleOperator && role != kgfctx.RoleViewer) {
			return nil, fmt.Errorf("%q: want svc:<name>=operator|viewer", item)
		}
		out[name] = role
	}
	return out, nil
}

// Authenticate verifies a context and returns the principal. Verification
// failures are ErrUnauthenticated (one opaque 401); a missing scope or an
// unlisted service are distinct 403s; a database fault is returned as is
// (503).
func (k *KGFAuthenticator) Authenticate(ctx context.Context, token string) (Principal, error) {
	now := time.Now()
	if k.Now != nil {
		now = k.Now()
	}
	c, err := k.Verifier.Verify(token, now)
	if err != nil {
		return Principal{}, fmt.Errorf("%w: %v", ErrUnauthenticated, err)
	}
	principal, role := c.Effective()
	via := ""
	if c.IsDelegated() {
		via = c.Principal
	}
	if c.IsService() {
		mapped, ok := k.ServicePrincipals[c.Principal]
		if !ok {
			return Principal{}, ErrKGFServiceNotAllowed
		}
		role = mapped
	}
	scope := store.KGFScope{Organization: c.Organization, Subscription: c.Subscription, ResourceGroup: c.ResourceGroup}
	if !scope.Complete() {
		return Principal{}, ErrKGFScopeRequired
	}
	key := store.KGFNamespace(scope)
	if _, ok := k.provisioned.Load(key); !ok {
		ns, err := k.Provisioner.ProvisionKGFNamespace(ctx, scope)
		if err != nil {
			return Principal{}, fmt.Errorf("provision kgf namespace: %w", err)
		}
		if ns != key {
			return Principal{}, fmt.Errorf("provision kgf namespace: derived %s, provisioned %s", key, ns)
		}
		k.provisioned.Store(key, true)
	}
	grants := KGFGrants(role)
	names := make([]string, 0, 1)
	names = append(names, "KGF "+role)
	sort.Strings(names)
	return Principal{
		// Rate limiting and logs key on this: stable per KGF person and scope,
		// unlike the token id, which changes on every request.
		KeyID:     "kgf:" + c.Organization + ":" + principal,
		Namespace: key,
		Name:      principal,
		// Never admin: a KGF principal's authority is its grants alone.
		Role:      RoleReader,
		Kind:      KindKGF,
		Grants:    grants,
		RoleNames: names,
		KGF: &KGFIdentity{
			Organization: c.Organization, Subscription: c.Subscription, ResourceGroup: c.ResourceGroup,
			Principal: principal, Role: role, Via: via, RequestID: c.RequestID, Issuer: c.Issuer, TokenID: c.TokenID,
		},
	}, nil
}
