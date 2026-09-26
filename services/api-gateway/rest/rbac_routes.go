// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0
package rest

// routePermissions maps every guarded route to the permissions it requires.
// A route missing here is refused (guard), and TestEveryProtectedRouteHasPermission
// fails the build for one registered without an entry.
//
// Two routes carry a further, body-dependent check in their handler, noted
// inline: creating versus updating a role (revision 0 or not), and creating a
// user with an initial role.
var routePermissions = map[string][]string{
	"GET /v1/account/tenant":                              {},
	"GET /v1/platform/tenants":                            {},
	"POST /v1/platform/tenants":                           {},
	"GET /v1/namespaces/{ns}/roles":                       {"roles.view"},
	"PUT /v1/namespaces/{ns}/roles/{name}":                {"roles.view"}, // + roles.create (revision 0) or roles.update, in putRole
	"DELETE /v1/namespaces/{ns}/roles/{name}":             {"roles.delete"},
	"GET /v1/namespaces/{ns}/users/{id}/roles":            {"users.view"},
	"PUT /v1/namespaces/{ns}/users/{id}/roles":            {"users.roles.assign", "roles.view"},
	"POST /v1/namespaces/{ns}/specs":                      {"workflows.publish"},
	"GET /v1/namespaces/{ns}/specs":                       {"workflows.view"},
	"GET /v1/namespaces/{ns}/specs/{name}/versions":       {"workflows.view"},
	"DELETE /v1/namespaces/{ns}/specs/{name}":             {"workflows.delete"},
	"GET /v1/namespaces/{ns}/specdefs/{id}":               {"workflows.view"},
	"POST /v1/namespaces/{ns}/runs":                       {"runs.create", "workflows.view"},
	"GET /v1/namespaces/{ns}/runs":                        {"runs.view"},
	"GET /v1/namespaces/{ns}/runs/{id}":                   {"runs.view"},
	"GET /v1/namespaces/{ns}/runs/{id}/events":            {"runs.view"},
	"DELETE /v1/namespaces/{ns}/runs/{id}":                {"runs.delete"},
	"POST /v1/namespaces/{ns}/runs/{id}/cancel":           {"runs.cancel"},
	"POST /v1/namespaces/{ns}/runs/{id}/pause":            {"runs.pause"},
	"POST /v1/namespaces/{ns}/runs/{id}/resume":           {"runs.resume"},
	"POST /v1/namespaces/{ns}/runs/{id}/retry":            {"runs.retry"},
	"GET /v1/namespaces/{ns}/attempts/{id}/logs":          {"tasks.logs.view"},
	"POST /v1/namespaces/{ns}/schedules":                  {"schedules.create", "workflows.view", "runs.create"},
	"GET /v1/namespaces/{ns}/schedules":                   {"schedules.view"},
	"GET /v1/namespaces/{ns}/schedules/preview":           {"schedules.view"},
	"PUT /v1/namespaces/{ns}/schedules/{name}":            {"schedules.update", "workflows.view", "runs.create"},
	"GET /v1/namespaces/{ns}/schedules/{name}":            {"schedules.view"},
	"DELETE /v1/namespaces/{ns}/schedules/{name}":         {"schedules.delete"},
	"POST /v1/namespaces/{ns}/schedules/{name}/enable":    {"schedules.update"},
	"POST /v1/namespaces/{ns}/schedules/{name}/disable":   {"schedules.update"},
	"POST /v1/namespaces/{ns}/schedules/{name}/backfills": {"backfills.create", "runs.create"},
	"GET /v1/namespaces/{ns}/schedules/{name}/backfills":  {"backfills.view"},
	"GET /v1/namespaces/{ns}/backfills/{id}":              {"backfills.view"},
	"POST /v1/namespaces/{ns}/backfills/{id}/pause":       {"backfills.update"},
	"POST /v1/namespaces/{ns}/backfills/{id}/resume":      {"backfills.update"},
	"POST /v1/namespaces/{ns}/backfills/{id}/cancel":      {"backfills.update"},
	"GET /v1/namespaces/{ns}/workers":                     {"workers.view"},
	"GET /v1/namespaces/{ns}/workers/{id}":                {"workers.view"},
	"POST /v1/namespaces/{ns}/workers/{id}/drain":         {"workers.drain"},
	"GET /v1/namespaces/{ns}/capacity":                    {"workers.view"},
	"POST /v1/namespaces/{ns}/workers/{id}/quarantine":    {"workers.quarantine"},
	"POST /v1/namespaces/{ns}/workers/{id}/release":       {"workers.quarantine"},
	"GET /v1/namespaces/{ns}/workers/{id}/quarantines":    {"workers.view"},
	"POST /v1/namespaces/{ns}/cache/invalidations":        {"workflows.cache.invalidate"},
	"GET /v1/namespaces/{ns}/attention":                   {"overview.view", "runs.view", "workers.view"},
	"GET /v1/health/fleet":                                {"workers.view"},
	"GET /v1/namespaces/{ns}/quotas":                      {"quotas.view"},
	"PUT /v1/namespaces/{ns}/quotas":                      {"quotas.manage"},
	"GET /v1/namespaces/{ns}/usage":                       {"usage.view"},
	"GET /v1/platform/usage":                              {},
	"GET /v1/namespaces/{ns}/platform/config":             {"infrastructure.view"},
	"PUT /v1/namespaces/{ns}/platform/config":             {"infrastructure.configure"},
	"GET /v1/namespaces/{ns}/platform/plan":               {"infrastructure.view"},
	// Cluster registration has its own grants since 0032 (ADR-0024 §7).
	"GET /v1/namespaces/{ns}/platform/clusters":           {"clusters.view"},
	"PUT /v1/namespaces/{ns}/platform/clusters/{name}":    {"clusters.configure"},
	"DELETE /v1/namespaces/{ns}/platform/clusters/{name}": {"clusters.configure"},
	"POST /v1/auth/logout":                                {},
	"GET /v1/auth/whoami":                                 {},
	"GET /v1/auth/memberships":                            {},
	"GET /v1/namespaces/{ns}/users":                       {"users.view"},
	"POST /v1/namespaces/{ns}/users":                      {"users.create"}, // + users.roles.assign when role_name is set
	"PUT /v1/namespaces/{ns}/users/{id}":                  {"users.update"},
	"POST /v1/namespaces/{ns}/users/{id}/disable":         {"users.disable"},
	"POST /v1/namespaces/{ns}/users/{id}/enable":          {"users.enable"},
	"POST /v1/namespaces/{ns}/users/{id}/password":        {"users.password.reset"},
	"GET /v1/namespaces/{ns}/users/{id}/sessions":         {"users.view"},
	"POST /v1/namespaces/{ns}/users/{id}/sessions/revoke": {"users.sessions.revoke"},
	"GET /v1/namespaces/{ns}/tokens":                      {"tokens.view"},
	"POST /v1/namespaces/{ns}/tokens":                     {"tokens.create"},
	"POST /v1/namespaces/{ns}/tokens/{id}/revoke":         {"tokens.revoke"},
	"GET /v1/namespaces/{ns}/audit":                       {"audit.view"},

	// ADR-0024: identity providers, linked identities, service accounts, owners.
	"GET /v1/namespaces/{ns}/sso/providers":                       {"sso.view"},
	"POST /v1/namespaces/{ns}/sso/providers":                      {"sso.configure"},
	"PUT /v1/namespaces/{ns}/sso/providers/{id}":                  {"sso.configure"},
	"DELETE /v1/namespaces/{ns}/sso/providers/{id}":               {"sso.configure"},
	"GET /v1/namespaces/{ns}/sso/providers/{id}/mappings":         {"sso.view"},
	"PUT /v1/namespaces/{ns}/sso/providers/{id}/mappings":         {"sso.mappings.update", "roles.view"},
	"GET /v1/namespaces/{ns}/users/{id}/identities":               {"users.view"},
	"DELETE /v1/namespaces/{ns}/users/{id}/identities/{identity}": {"users.update"},
	"GET /v1/namespaces/{ns}/service-accounts":                    {"service_accounts.view"},
	"POST /v1/namespaces/{ns}/service-accounts":                   {"service_accounts.create"},
	"PUT /v1/namespaces/{ns}/service-accounts/{id}":               {"service_accounts.update"},
	"POST /v1/namespaces/{ns}/service-accounts/{id}/disable":      {"service_accounts.disable"},
	"POST /v1/namespaces/{ns}/service-accounts/{id}/enable":       {"service_accounts.update"},
	"GET /v1/namespaces/{ns}/service-accounts/{id}/roles":         {"service_accounts.view"},
	"PUT /v1/namespaces/{ns}/service-accounts/{id}/roles":         {"service_accounts.roles.assign", "roles.view"},
	"GET /v1/namespaces/{ns}/owners":                              {"owners.view"},
	"POST /v1/namespaces/{ns}/owners":                             {"owners.grant"},
	"DELETE /v1/namespaces/{ns}/owners/{user}":                    {"owners.grant"},

	// Attempt replay input (ADR-0022). It carries upstream RESULTS (inline
	// input values), so it needs the result-view grant as well as run and
	// task visibility. Its own block keeps the table above aligned as is.
	assignmentRoute: {"runs.view", "tasks.view", "tasks.results.view"},
}
