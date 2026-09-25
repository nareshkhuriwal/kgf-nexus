// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

// Package rest implements the public REST/JSON API from architecture §8.2.
//
// This surface is for humans, the CLI, and the UI. The worker protocol lives on a
// SEPARATE gRPC listener and a separate port: worker traffic is chatty and
// latency-sensitive, and a fleet of workers long-polling must not contend with a
// UI query that scans a run's history (§3.2).
package rest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/khuriwalgroup/kgf-nexus/internal/store"
	"github.com/khuriwalgroup/kgf-nexus/services/api-gateway/authn"
	"github.com/khuriwalgroup/kgf-nexus/services/api-gateway/logs"
	"github.com/khuriwalgroup/kgf-nexus/services/api-gateway/spec"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// WorkflowManager is the workflow-manager's API as the gateway needs it.
//
// The gateway owns NEITHER workflow_specs nor workflow_runs (ADR-0011), so every
// mutation here is a call, not a query. The gateway's job is authentication,
// validation, and shape — not lifecycle.
type WorkflowManager interface {
	PutSpec(ctx context.Context, namespace, name, hash string, canonicalJSON []byte, createdBy string) (row store.SpecRow, created bool, err error)
	// DeleteSpec removes every version of a workflow name, returning how many
	// were removed. It REFUSES (FailedPrecondition) when runs or schedules
	// still reference the workflow; see deleteSpec below for the semantics.
	DeleteSpec(ctx context.Context, namespace, name string) (int, error)
	CreateRun(ctx context.Context, req CreateRunRequest) (run store.RunRow, created bool, err error)
	CancelRun(ctx context.Context, namespace, runID string) (store.RunRow, error)
	PauseRun(ctx context.Context, namespace, runID string) (store.RunRow, error)
	ResumeRun(ctx context.Context, namespace, runID string) (store.RunRow, error)
	// RetryRun retries a failed run from the named nodes, or from every node
	// that did not succeed when nodes is empty. actor attributes the retry in
	// the run's history.
	RetryRun(ctx context.Context, namespace, runID string, nodes []string, actor string) (store.RunRow, error)
}

// ResourceManagerAPI is the resource-manager's API as the gateway needs it.
type ResourceManagerAPI interface {
	DrainWorker(ctx context.Context, namespace, workerID string) error
	Capacity(ctx context.Context, namespace string) (CapacityReport, error)
}

// LogStreamer reads an attempt's durable log (ADR-0020).
//
// The bytes live in the result store, NOT in the metadata store; the index in
// Postgres says where each line is. Implemented by *logs.Reader. Every method
// is bounded, so a gigabyte log costs a page, not the gateway.
type LogStreamer interface {
	Summary(ctx context.Context, attemptID, attemptState string) (store.AttemptLogRow, bool, logs.Status, error)
	ReadLines(ctx context.Context, attemptID string, row store.AttemptLogRow, from int64, limit int) (logs.Page, error)
	Search(ctx context.Context, attemptID string, row store.AttemptLogRow, q logs.SearchQuery) (logs.SearchResult, error)
	Limits() logs.Limits
}

// CreateRunRequest is the run-creation call.
type CreateRunRequest struct {
	Namespace      string
	SpecID         string
	SpecName       string
	IdempotencyKey string
	TriggerKind    string
	Params         map[string]any
	Priority       int32
	Actor          string
}

// CapacityReport is advertised-versus-committed capacity for a namespace.
type CapacityReport struct {
	Namespace  string         `json:"namespace"`
	Advertised map[string]any `json:"advertised"`
	Committed  map[string]any `json:"committed"`
	Workers    int            `json:"workers"`
	// Pending explains a stall. A UI showing an unexplained "0 running, 40
	// pending" is useless during an incident; a structured reason turns it into
	// "waiting_for_resources: gpu".
	PendingAttempts int `json:"pending_attempts"`
}

// API is the REST handler set.
type API struct {
	db    store.GWDB
	auth  *authn.Authenticator
	wfm   WorkflowManager
	rm    ResourceManagerAPI
	logs  LogStreamer
	log   *slog.Logger
	clock func() time.Time
	// limits holds the per-address, auth-failure and per-credential buckets. Each
	// is nil when disabled, which is the zero-config default and what keeps dev
	// and the existing tests unchanged. See ratelimit.go.
	limits gatewayLimiters
	// schedules is nil when no schedule upstream is configured, in which case the
	// schedule routes answer 501 rather than the gateway refusing to start. That
	// keeps a gateway usable against a workflow-manager that predates schedules,
	// which is what a rolling upgrade produces (ADR-0011 rule 5).
	schedules ScheduleManager
	// backfills is nil when no backfill upstream is configured, for the same reason
	// as schedules.
	backfills BackfillManager
	// fleet reports upstream connectivity; nil when not wired.
	fleet FleetHealth
	// identity is nil unless WithIdentity was called, in which case the identity
	// and admin routes are simply not registered. See admin.go.
	identity IdentityAdmin
	// email is nil unless WithEmailFlows was called; then the verification and
	// recovery routes exist and login consults verification state. See
	// email_flows.go.
	email *emailFlows

	// cache is nil unless WithCacheInvalidator was called; its route answers 501.
	cache CacheInvalidator
}

// New builds the REST API.
func New(db store.GWDB, auth *authn.Authenticator, wfm WorkflowManager, rm ResourceManagerAPI, logs LogStreamer, log *slog.Logger) *API {
	if log == nil {
		log = slog.Default()
	}
	return &API{db: db, auth: auth, wfm: wfm, rm: rm, logs: logs, log: log, clock: time.Now}
}

// WithRateLimit enables rate limiting on every route from the one
// per-credential setting, deriving the per-address and auth-failure buckets
// from it (GatewayRateLimitsFor).
//
// Opt-in rather than on by default so enabling it is a deliberate deployment
// decision: a limit guessed wrong is itself an outage, and the right number
// depends on the fleet's size and the console's poll interval.
func (a *API) WithRateLimit(cfg RateLimitConfig) *API {
	return a.WithRateLimits(GatewayRateLimitsFor(cfg))
}

// WithRateLimits enables the three gateway limiters with explicit settings.
func (a *API) WithRateLimits(cfg GatewayRateLimits) *API {
	a.limits = newGatewayLimiters(cfg, a.clock)
	return a
}

// WithSchedules wires the schedule-mutation upstream, enabling those routes.
func (a *API) WithSchedules(m ScheduleManager) *API {
	a.schedules = m
	return a
}

// WithBackfills wires the backfill upstream, enabling those routes.
func (a *API) WithBackfills(m BackfillManager) *API {
	a.backfills = m
	return a
}

// FleetHealth reports the gateway's view of every upstream it dials.
//
// An interface rather than the concrete upstream clients, for the reason every
// other upstream here is one: the rest package must stay testable without
// dialling anything, and a fake that returns a fixed fleet is how the handler's
// behaviour gets pinned.
type FleetHealth interface {
	UpstreamHealth() []UpstreamHealth
}

// UpstreamHealth is one upstream's observed connectivity.
//
// Declared HERE rather than imported from the upstream package, because that
// package already imports this one -- the gateway's REST layer defines the
// shapes its handlers serve, and the upstream layer fills them in. Taking the
// dependency the other way is an import cycle.
type UpstreamHealth struct {
	// Name is the service, as an operator names it.
	Name string `json:"name"`
	// Addr is what this gateway was configured to dial, which is half of every
	// "why can't it see X" investigation.
	Addr string `json:"addr"`
	// State is the gRPC connectivity state: IDLE, CONNECTING, READY,
	// TRANSIENT_FAILURE or SHUTDOWN. Reported verbatim rather than flattened to a
	// boolean, because IDLE (nothing dialled yet) and TRANSIENT_FAILURE (tried and
	// failed) are different problems with different fixes.
	State string `json:"state"`
	// Configured is false when no address was set. Such an upstream is not
	// unhealthy -- it was never asked for -- and showing it as DOWN would send an
	// operator chasing a service this deployment does not run.
	Configured bool `json:"configured"`
}

// WithFleetHealth wires the health reporter, enabling GET /v1/health/fleet.
//
// Nil-tolerant like the schedule upstreams: a gateway assembled without it
// answers 501 on that route rather than refusing to start, so this is additive
// for any deployment that has not wired it.
func (a *API) WithFleetHealth(f FleetHealth) *API {
	a.fleet = f
	return a
}

// Routes builds the mux for §8.2.
//
// Go 1.22+ pattern routing is used rather than a third-party router: the path set
// is small and fixed, and a dependency whose only job is splitting strings is a
// dependency whose CVEs we would have to track for no benefit.
func (a *API) Routes() http.Handler {
	mux := http.NewServeMux()

	mux.Handle("POST /v1/namespaces/{ns}/specs", a.guard(authn.ActionWrite, a.submitSpec))
	// The catalog: one row per workflow NAME, newest version, searchable.
	mux.Handle("GET /v1/namespaces/{ns}/specs", a.guard(authn.ActionRead, a.listCatalog))
	mux.Handle("GET /v1/namespaces/{ns}/specs/{name}/versions", a.guard(authn.ActionRead, a.listSpecVersions))
	// Deleting a workflow definition is ADMIN_WRITE, not WRITE, deliberately.
	// Submitting a spec is additive and reversible -- a bad version is fixed by
	// submitting a better one, and the old one is still there. Deleting one
	// destroys the document irrecoverably and removes a name the rest of the
	// namespace may create runs by. That is the same class of authority as
	// draining a worker, which is already ActionAdminWrite, so it gets the same
	// level rather than being reachable by every CI credential that can deploy.
	mux.Handle("DELETE /v1/namespaces/{ns}/specs/{name}", a.guard(authn.ActionAdminWrite, a.deleteSpec))
	// The DEFINITION, on its own collection rather than under /specs/{name}/.
	// `/specs/id/{id}` would be ambiguous with `/specs/{name}/versions` for a
	// workflow literally named "id", and net/http rejects the pair outright.
	// Addressed by spec id because a run pins a spec_id (ADR-0004), so this is
	// the one address resolving to exactly the bytes a given run executed.
	mux.Handle("GET /v1/namespaces/{ns}/specdefs/{id}", a.guard(authn.ActionRead, a.getSpec))

	mux.Handle("POST /v1/namespaces/{ns}/runs", a.guard(authn.ActionWrite, a.createRun))
	mux.Handle("GET /v1/namespaces/{ns}/runs", a.guard(authn.ActionRead, a.listRuns))
	mux.Handle("GET /v1/namespaces/{ns}/runs/{id}", a.guard(authn.ActionRead, a.getRun))
	mux.Handle("GET /v1/namespaces/{ns}/runs/{id}/events", a.guard(authn.ActionRead, a.getRunEvents))
	mux.Handle("DELETE /v1/namespaces/{ns}/runs/{id}", a.guard(authn.ActionAdminWrite, a.deleteRun))
	mux.Handle("POST /v1/namespaces/{ns}/runs/{id}/cancel", a.guard(authn.ActionWrite, a.cancelRun))
	mux.Handle("POST /v1/namespaces/{ns}/runs/{id}/pause", a.guard(authn.ActionWrite, a.pauseRun))
	mux.Handle("POST /v1/namespaces/{ns}/runs/{id}/resume", a.guard(authn.ActionWrite, a.resumeRun))
	mux.Handle("POST /v1/namespaces/{ns}/runs/{id}/retry", a.guard(authn.ActionWrite, a.retryRun))

	mux.Handle("GET /v1/namespaces/{ns}/attempts/{id}/logs", a.guard(authn.ActionRead, a.attemptLogs))

	// Schedules. Creating one is a write because it creates runs on a timer; the
	// enable/disable pair and delete are writes for the same reason. The preview is
	// a read: it evaluates an expression and touches nothing.
	mux.Handle("POST /v1/namespaces/{ns}/schedules", a.guard(authn.ActionWrite, a.createSchedule))
	mux.Handle("GET /v1/namespaces/{ns}/schedules", a.guard(authn.ActionRead, a.listSchedules))
	mux.Handle("GET /v1/namespaces/{ns}/schedules/preview", a.guard(authn.ActionRead, a.previewSchedule))
	mux.Handle("PUT /v1/namespaces/{ns}/schedules/{name}", a.guard(authn.ActionWrite, a.createSchedule))
	mux.Handle("GET /v1/namespaces/{ns}/schedules/{name}", a.guard(authn.ActionRead, a.getSchedule))
	mux.Handle("DELETE /v1/namespaces/{ns}/schedules/{name}", a.guard(authn.ActionWrite, a.deleteSchedule))
	mux.Handle("POST /v1/namespaces/{ns}/schedules/{name}/enable", a.guard(authn.ActionWrite, a.setScheduleEnabled(true)))
	mux.Handle("POST /v1/namespaces/{ns}/schedules/{name}/disable", a.guard(authn.ActionWrite, a.setScheduleEnabled(false)))

	// Backfills hang off their schedule for creation and listing, and are addressed
	// by their own id thereafter: a backfill outlives the interval that named it and
	// an operator tracks it by identity, not by which schedule it came from.
	mux.Handle("POST /v1/namespaces/{ns}/schedules/{name}/backfills", a.guard(authn.ActionWrite, a.startBackfill))
	mux.Handle("GET /v1/namespaces/{ns}/schedules/{name}/backfills", a.guard(authn.ActionRead, a.listBackfills))
	mux.Handle("GET /v1/namespaces/{ns}/backfills/{id}", a.guard(authn.ActionRead, a.getBackfill))
	mux.Handle("POST /v1/namespaces/{ns}/backfills/{id}/pause", a.guard(authn.ActionWrite, a.backfillAction("pause")))
	mux.Handle("POST /v1/namespaces/{ns}/backfills/{id}/resume", a.guard(authn.ActionWrite, a.backfillAction("resume")))
	mux.Handle("POST /v1/namespaces/{ns}/backfills/{id}/cancel", a.guard(authn.ActionWrite, a.backfillAction("cancel")))

	mux.Handle("GET /v1/namespaces/{ns}/workers", a.guard(authn.ActionRead, a.listWorkers))
	// One worker plus the tasks attributed to it. A read, like the fleet listing
	// it drills into — the drain below stays admin_write because it reshapes the
	// fleet, but "what is this worker running" is the question the fleet table
	// raises and cannot answer.
	mux.Handle("GET /v1/namespaces/{ns}/workers/{id}", a.guard(authn.ActionRead, a.getWorker))
	mux.Handle("POST /v1/namespaces/{ns}/workers/{id}/drain", a.guard(authn.ActionAdminWrite, a.drainWorker))
	mux.Handle("GET /v1/namespaces/{ns}/capacity", a.guard(authn.ActionRead, a.capacity))
	// Quarantine is the drain's sibling and sits at the same authority: it takes
	// capacity out of the pool. The history read is a read. See quarantine.go.
	mux.Handle("POST /v1/namespaces/{ns}/workers/{id}/quarantine", a.guard(authn.ActionAdminWrite, a.quarantineWorker))
	mux.Handle("POST /v1/namespaces/{ns}/workers/{id}/release", a.guard(authn.ActionAdminWrite, a.releaseWorker))
	mux.Handle("GET /v1/namespaces/{ns}/workers/{id}/quarantines", a.guard(authn.ActionRead, a.listWorkerQuarantines))
	// Memo invalidation is a WRITE, not admin: it only ever causes work to run
	// again, never removes a result or a run.
	mux.Handle("POST /v1/namespaces/{ns}/cache/invalidations", a.guard(authn.ActionWrite, a.invalidateCache))

	// The triage panel. A read: it aggregates conditions already visible through
	// /runs, /capacity and a run's events, and exposes no tenant data those do
	// not, so gating it on admin would only stop the operators who most need it
	// from seeing it during an incident.
	mux.Handle("GET /v1/namespaces/{ns}/attention", a.guard(authn.ActionRead, a.attention))

	// Fleet health is NOT namespaced: the control plane is one deployment, and a
	// scheduler that is down is down for every namespace. Guarded on read rather
	// than admin because "is the platform up" is the question every operator
	// needs answered during an incident, and it exposes only service names,
	// configured addresses and connectivity states -- no tenant data.
	mux.Handle("GET /v1/health/fleet", a.guard(authn.ActionRead, a.fleetHealth))

	// The schema the gateway actually enforces, served so authors and the SDK can
	// validate locally against the same grammar rather than a documented copy of it.
	mux.HandleFunc("GET /v1/schemas/workflow-spec-v1.json", a.serveSchema)

	mux.Handle("GET /v1/namespaces/{ns}/platform/config", a.guard(authn.ActionAdminWrite, a.platformGet))
	mux.Handle("PUT /v1/namespaces/{ns}/platform/config", a.guard(authn.ActionAdminWrite, a.platformPut))
	mux.Handle("GET /v1/namespaces/{ns}/platform/plan", a.guard(authn.ActionAdminWrite, a.platformPlan))
	a.registerClusterRoutes(mux)
	a.registerQuotaRoutes(mux)
	a.registerRBACRoutes(mux)
	a.registerIdentityRoutes(mux)

	return mux
}

// registerIdentityRoutes adds the identity and administration surface.
//
// Registered only when identity is wired, so a deployment without it serves
// exactly the route set it served before -- a 404 from a route that was never
// enabled is clearer than a 501 from one that exists but cannot work.
//
// Every route here goes through guard() except login, which cannot: a caller
// logging in has no credential to authorize. That single exception is written out
// explicitly rather than hidden behind a helper, so the property "every route has
// an authorization decision" stays auditable by reading this function.
func (a *API) registerIdentityRoutes(mux *http.ServeMux) {
	if a.identity == nil {
		return
	}

	// THE ONE UNGUARDED ROUTE. Rate-limited and fully audited inside the handler.
	mux.HandleFunc("POST /v1/auth/login", a.login)
	mux.HandleFunc("POST /v1/auth/register", a.registerCustomer)
	// Email verification and password recovery: unguarded for the same reason as
	// login (the caller has no credential), rate limited per IP and per address
	// inside the handlers. Registered only when the flows are wired.
	if a.email != nil {
		mux.HandleFunc("POST /v1/auth/email/verification", a.requestVerificationEmail)
		mux.HandleFunc("POST /v1/auth/email/verify", a.verifyEmail)
		mux.HandleFunc("POST /v1/auth/password/forgot", a.forgotPassword)
		mux.HandleFunc("POST /v1/auth/password/reset", a.resetPasswordWithLink)
	}
	mux.Handle("GET /v1/account/tenant", a.guard(authn.ActionRead, a.currentTenant))
	mux.Handle("GET /v1/platform/tenants", a.guard(authn.ActionRead, a.listTenants))
	mux.Handle("POST /v1/platform/tenants", a.guard(authn.ActionRead, a.createTenant))

	// Logout and whoami need only a valid credential of any kind, so they are
	// guarded on read: every authenticated principal may ask who it is and end its
	// own session.
	mux.Handle("POST /v1/auth/logout", a.guard(authn.ActionRead, a.logout))
	mux.Handle("GET /v1/auth/whoami", a.guard(authn.ActionRead, a.whoami))

	// User administration. ActionAdminWrite throughout: creating, disabling and
	// resetting accounts are the operations that can lock a tenant out or let an
	// attacker in, so they sit behind the strongest grant the matrix has.
	mux.Handle("GET /v1/namespaces/{ns}/users", a.guard(authn.ActionAdminWrite, a.listUsers))
	mux.Handle("POST /v1/namespaces/{ns}/users", a.guard(authn.ActionAdminWrite, a.createUser))
	mux.Handle("PUT /v1/namespaces/{ns}/users/{id}", a.guard(authn.ActionAdminWrite, a.updateUser))
	mux.Handle("POST /v1/namespaces/{ns}/users/{id}/disable", a.guard(authn.ActionAdminWrite, a.setUserDisabled))
	mux.Handle("POST /v1/namespaces/{ns}/users/{id}/enable", a.guard(authn.ActionAdminWrite, a.setUserDisabled))
	mux.Handle("POST /v1/namespaces/{ns}/users/{id}/password", a.guard(authn.ActionAdminWrite, a.resetUserPassword))
	mux.Handle("GET /v1/namespaces/{ns}/users/{id}/sessions", a.guard(authn.ActionAdminWrite, a.listUserSessions))
	mux.Handle("POST /v1/namespaces/{ns}/users/{id}/sessions/revoke", a.guard(authn.ActionAdminWrite, a.revokeUserSessions))

	// Scoped tokens. Listing is admin-only too: the list names every machine
	// credential in the namespace and when each was last used, which is a map of
	// the tenant's integrations.
	mux.Handle("GET /v1/namespaces/{ns}/tokens", a.guard(authn.ActionAdminWrite, a.listTokens))
	mux.Handle("POST /v1/namespaces/{ns}/tokens", a.guard(authn.ActionAdminWrite, a.createToken))
	mux.Handle("POST /v1/namespaces/{ns}/tokens/{id}/revoke", a.guard(authn.ActionAdminWrite, a.revokeToken))

	// The audit trail records who changed what. Admin-only: it names accounts and
	// the actions taken against them.
	mux.Handle("GET /v1/namespaces/{ns}/audit", a.guard(authn.ActionAdminWrite, a.listAudit))
}

// guard authenticates, authorizes for the path's namespace, and then dispatches.
//
// Wrapping every handler means no route can be added without an authorization
// decision: forgetting the check is not possible, because the check is the only
// way to register a route.
func (a *API) guard(action authn.Action, h func(http.ResponseWriter, *http.Request, authn.Principal)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Before authentication on purpose, and keyed on the client ADDRESS, not
		// the presented credential: an unauthenticated flood is exactly the case
		// that needs limiting, authenticating first would spend a database round
		// trip per rejected request, and an unverified Authorization header is
		// attacker-chosen, so keying on it handed out a fresh bucket per request.
		if a.rateLimited(w, r) {
			return
		}
		presented := authn.ExtractBearer(r.Header.Get("Authorization"))
		// An address that has used up its failure budget may not try another
		// credential until it refills. Only when one is presented: a request with
		// no credential is not a guess, and a console loading its sign-in page
		// must not lock its own address out.
		if presented != "" && a.authAttemptBlocked(w, r) {
			return
		}
		// AuthenticateCredential, not Authenticate: it dispatches on the credential's
		// prefix to sessions or scoped tokens and otherwise falls through to the
		// unchanged api_keys lookup. Every credential kind therefore passes through
		// this one enforcement point, which is what keeps "who may do what"
		// answerable by reading this function.
		p, err := a.auth.AuthenticateCredential(r.Context(), presented)
		if errors.Is(err, authn.ErrUnauthenticated) {
			if presented != "" {
				a.recordAuthFailure(r, presented)
			}
			// No detail about WHY. Distinguishing "no such key" from "revoked"
			// would make this endpoint an oracle for probing valid keys.
			writeError(w, http.StatusUnauthorized, "unauthenticated", "valid credentials are required")
			return
		}
		if err != nil {
			// A database outage is not an authentication failure; 503 tells the
			// client to retry rather than to rotate its credential.
			a.log.ErrorContext(r.Context(), "authentication backend failed", "error", err)
			writeError(w, http.StatusServiceUnavailable, "unavailable", "authentication is temporarily unavailable")
			return
		}
		// Only now, with a verified principal, the per-credential bucket, keyed by
		// the credential's ID. Before authorization, so a principal hammering routes
		// it may not use still spends its own allowance.
		if a.credentialRateLimited(w, p) {
			return
		}

		ns := r.PathValue("ns")
		if err := p.Authorize(ns, action); err != nil {
			writeError(w, http.StatusForbidden, "forbidden", "not permitted for this namespace")
			return
		}

		required, mapped := routePermissions[r.Pattern]
		if !mapped {
			writeError(w, 403, "forbidden", "Route has no permission mapping")
			return
		}
		for _, permission := range required {
			if !p.Allows(permission) {
				writeError(w, 403, "forbidden", "Missing permission: "+permission)
				return
			}
		}
		// Identity mutation and delegation remain restricted to unbound administrators.
		if p.Grants != nil {
			for _, permission := range required {
				if permission == "users.manage" || permission == "tokens.manage" || permission == "roles.manage" {
					writeError(w, 403, "forbidden", "Delegation requires a namespace administrator")
					return
				}
			}
		}
		h(w, r.WithContext(authn.WithPrincipal(r.Context(), p)), p)
	})
}

// ── specs ─────────────────────────────────────────────────────────────────

// submitSpec runs the full pipeline: accept JSON or YAML, validate structurally
// and semantically, canonicalize, content-address, store.
//
// A byte-identical resubmit is a NO-OP that returns the existing version with 200
// rather than creating a redundant one (ADR-0004). The distinction between 200 and
// 201 is the only way a client can tell "this was already here" from "I just
// created it", which matters to a CI pipeline deciding whether anything changed.
func (a *API) submitSpec(w http.ResponseWriter, r *http.Request, p authn.Principal) {
	body, err := readBody(r, maxSpecBytes)
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "spec_too_large", err.Error())
		return
	}

	format := spec.DetectFormat(body, contentType(r))
	compiled, err := spec.Compile(body, format)
	if err != nil {
		var vf *spec.ValidationFailure
		if errors.As(err, &vf) {
			// 422, not 400: the request was well-formed HTTP and well-formed JSON;
			// the DOCUMENT is semantically unacceptable. The distinction tells a
			// client whether to fix its transport or its content.
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
				"error":    "spec_invalid",
				"message":  "spec failed validation",
				"problems": vf.Problems,
			})
			return
		}
		a.log.ErrorContext(r.Context(), "spec compilation failed unexpectedly", "error", err)
		writeError(w, http.StatusInternalServerError, "internal", "could not compile spec")
		return
	}

	ns := r.PathValue("ns")
	if got := compiled.Proto.GetNamespace(); got != ns {
		// The path and the document must agree. Silently preferring one would let a
		// caller authenticated for namespace A store a spec claiming namespace B.
		writeError(w, http.StatusUnprocessableEntity, "namespace_mismatch",
			fmt.Sprintf("spec declares namespace %q but was submitted to %q", got, ns))
		return
	}

	row, created, err := a.wfm.PutSpec(r.Context(), ns, compiled.Proto.GetName(), compiled.Hash, compiled.Canonical, p.Name)
	if err != nil {
		a.log.ErrorContext(r.Context(), "failed to store spec", "namespace", ns, "error", err)
		writeError(w, http.StatusBadGateway, "upstream_unavailable", "could not store spec")
		return
	}

	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, map[string]any{
		"id":         row.ID,
		"namespace":  row.Namespace,
		"name":       row.Name,
		"version":    row.Version,
		"spec_hash":  row.SpecHash,
		"created":    created,
		"task_count": len(compiled.Proto.GetTasks()),
	})
}

// fleetHealth reports the gateway's view of every upstream it dials.
//
// CONNECTIVITY, not readiness, and the payload says so in its own body rather
// than relying on the reader to know. See upstream.UpstreamHealth for why the
// cheap answer is the right one here: this is the endpoint called when something
// is already wrong, so it must not itself be able to hang.
func (a *API) fleetHealth(w http.ResponseWriter, r *http.Request, _ authn.Principal) {
	if a.fleet == nil {
		writeError(w, http.StatusNotImplemented, "not_configured",
			"this gateway was not wired with a fleet health reporter")
		return
	}
	ups := a.fleet.UpstreamHealth()

	// "degraded" rather than "unhealthy": the gateway answering at all means the
	// ingress is up, and an upstream that is merely IDLE has not failed -- it has
	// not been dialled yet, which is normal before first use.
	overall := "ok"
	for _, u := range ups {
		if u.Configured && u.State != "READY" && u.State != "IDLE" {
			overall = "degraded"
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":    overall,
		"upstreams": ups,
		// Stated in the payload because a client that treats this as a readiness
		// check will otherwise report a connected-but-failing service as healthy.
		"measures": "gRPC connectivity from this gateway, read without a network " +
			"round trip; not each service's own /readyz",
	})
}

// listCatalog serves the workflow catalog: one entry per workflow name.
//
// Read locally rather than through the workflow-manager, like every other read:
// ADR-0011 lets the gateway read any table, and a catalog listing is a projection
// like the run list.
func (a *API) listCatalog(w http.ResponseWriter, r *http.Request, _ authn.Principal) {
	rows, err := store.GatewayListCatalog(r.Context(), a.db, r.PathValue("ns"),
		strings.TrimSpace(r.URL.Query().Get("search")), intParam(r, "limit", 100))
	if err != nil {
		a.fail(w, r, "list catalog", err)
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, s := range rows {
		out = append(out, map[string]any{
			"id": s.ID, "namespace": s.Namespace, "name": s.Name,
			// The LATEST version and its hash: what a run created by name today
			// would actually execute.
			"latest_version":   s.Version,
			"spec_hash":        s.SpecHash,
			"created_by":       s.CreatedBy,
			"updated_at":       s.CreatedAt,
			"version_count":    s.VersionCount,
			"first_created_at": s.FirstCreatedAt,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"workflows": out})
}

// getSpec serves one spec's DEFINITION, which nothing else exposes.
//
// The run DAG view renders task_nodes, which exist only after a run has been
// created; a catalog has to show a workflow's shape BEFORE anything has run it,
// and that shape lives only in workflow_specs.spec_json.
//
// spec_json is returned as raw JSON rather than re-marshalled through a struct:
// it is stored canonicalized and content-addressed (ADR-0004), so re-encoding it
// risks handing a client bytes whose hash no longer matches the spec_hash
// alongside them.
func (a *API) getSpec(w http.ResponseWriter, r *http.Request, _ authn.Principal) {
	row, err := store.WFMGetSpecByID(r.Context(), a.db, r.PathValue("id"))
	if err != nil {
		a.fail(w, r, "get spec", err)
		return
	}
	// A spec id is globally unique, so it resolves without the namespace -- but
	// the route is namespaced and a caller authorized for one namespace must not
	// read another's definition through it.
	if row.Namespace != r.PathValue("ns") {
		writeError(w, http.StatusNotFound, "not_found", "no such spec in this namespace")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id": row.ID, "namespace": row.Namespace, "name": row.Name,
		"version": row.Version, "spec_hash": row.SpecHash,
		"created_by": row.CreatedBy, "created_at": row.CreatedAt,
		"spec": json.RawMessage(row.SpecJSON),
	})
}

// deleteSpec removes every version of a workflow, or refuses and names the blocker.
//
// REFERENTIAL INTEGRITY, and why this refuses rather than cascading.
//
// Two things point at a workflow spec and they point at it DIFFERENTLY:
//
//   - workflow_runs.spec_id is a hard foreign key to workflow_specs(id) with no
//     ON DELETE clause (migration 0001), so Postgres itself refuses a delete that
//     would orphan a run. Those runs are the immutable execution history the audit
//     trail and the event stream reference; ADR-0004 promises a historical run's
//     exact definition is recoverable forever, and a cascade would break that
//     promise silently for every run in order to satisfy one click.
//
//   - schedules.spec_name is plain TEXT with NO foreign key (migration 0006),
//     because a schedule deliberately tracks the LATEST version of a name rather
//     than pinning one. The database therefore will NOT stop a delete that breaks
//     a schedule: the schedule would keep its next_fire_at, fire on time, fail to
//     resolve the name, and stop producing runs with nothing in the schedule row
//     to say why. That is the silent corruption this handler exists to prevent,
//     and it is checked in application code because nothing else can check it.
//
// So the answer is 409 with the blocker NAMED. A soft delete was the alternative
// and was rejected: a deleted_at column would have to be honoured by the catalog,
// the version listing, the gateway's spec reads AND the scheduler's name
// resolution at fire time -- and that last reader lives in another service. A
// tombstone one reader ignores is worse than no delete, because the operator
// believes the workflow is gone when it is still firing.
//
// The blocker detail is re-derived HERE with a local read rather than carried in
// the gRPC response, because gRPC delivers no response message alongside a non-OK
// status. ADR-0011 lets the gateway read any table, so this costs one query on the
// refusal path only, and it lets the 409 body carry structured fields the console
// can render as links instead of a sentence the client would have to parse.
func (a *API) deleteSpec(w http.ResponseWriter, r *http.Request, p authn.Principal) {
	ns, name := r.PathValue("ns"), r.PathValue("name")

	deleted, err := a.wfm.DeleteSpec(r.Context(), ns, name)
	if err != nil {
		switch status.Code(err) {
		case codes.FailedPrecondition:
			// Re-read the blockers so the body can name them. A failure to read
			// them is not fatal: the REFUSAL is the answer, and answering with a
			// less detailed 409 beats turning a correct refusal into a 500.
			runs, schedules, cerr := store.WFMCountSpecReferences(r.Context(), a.db, ns, name)
			body := map[string]any{
				"error":   "workflow_referenced",
				"message": status.Convert(err).Message(),
			}
			if cerr == nil {
				body["referencing_runs"] = runs
				// Always a list, never null: a client rendering "still used by"
				// should not have to distinguish an empty list from a missing one.
				if schedules == nil {
					schedules = []string{}
				}
				body["referencing_schedules"] = schedules
			}
			// The refusal is audited as loudly as the success. An audit log that
			// records only what happened cannot show what someone TRIED to do,
			// and an attempt to delete a workflow with live history is exactly
			// the event an operator wants to find later.
			a.auditPrincipal(r, p, "spec.delete", "workflow", ns+"/"+name, store.AuditDenied,
				map[string]any{"referencing_runs": runs, "referencing_schedules": schedules})
			writeJSON(w, http.StatusConflict, body)
			return
		case codes.NotFound:
			writeError(w, http.StatusNotFound, "workflow_not_found", "no such workflow in this namespace")
			return
		case codes.InvalidArgument:
			writeError(w, http.StatusBadRequest, "invalid_request", status.Convert(err).Message())
			return
		}
		a.fail(w, r, "delete spec", err)
		return
	}

	// Audited on the way OUT, after the owner confirmed it, so the log records what
	// actually happened rather than what was attempted. The version count is in the
	// detail because it is the only remaining evidence of how much was destroyed.
	a.auditPrincipal(r, p, "spec.delete", "workflow", ns+"/"+name, store.AuditSuccess,
		map[string]any{"versions_deleted": deleted})

	// 200 with the count rather than 204, unlike deleteSchedule: "how many versions
	// did that remove" is information the operator cannot recover afterwards, and a
	// 204 would throw it away.
	writeJSON(w, http.StatusOK, map[string]any{
		"namespace": ns, "name": name, "versions_deleted": deleted, "deleted": true,
	})
}

func (a *API) listSpecVersions(w http.ResponseWriter, r *http.Request, _ authn.Principal) {
	rows, err := store.GatewayListSpecVersions(r.Context(), a.db, r.PathValue("ns"), r.PathValue("name"), intParam(r, "limit", 50))
	if err != nil {
		a.fail(w, r, "list spec versions", err)
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, s := range rows {
		out = append(out, map[string]any{
			"id": s.ID, "name": s.Name, "version": s.Version,
			"spec_hash": s.SpecHash, "created_by": s.CreatedBy, "created_at": s.CreatedAt,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"versions": out})
}

func (a *API) serveSchema(w http.ResponseWriter, r *http.Request) {
	b, err := spec.SchemaBytes()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "schema unavailable")
		return
	}
	w.Header().Set("Content-Type", "application/schema+json")
	// The schema is immutable for the life of the binary, so it is safe to cache
	// hard; a client re-fetching it on every validation is pure waste.
	w.Header().Set("Cache-Control", "public, max-age=3600")
	_, _ = w.Write(b)
}

// ── runs ──────────────────────────────────────────────────────────────────

// createRun is the endpoint where the Idempotency-Key header is honoured.
//
// The three outcomes are all load-bearing:
//   - no key            -> create normally
//   - key + same body   -> return the CACHED response, do not create again
//   - key + DIFFERENT body -> 409
//
// That last case is the interesting one. The lazy implementation returns the
// cached response for any reuse of the key, which silently answers a question
// about request B with the result of request A. Returning 409 catches the client
// bug — a retry loop whose body changed, or a key derived from too little of the
// input — at the moment it is cheapest to find, instead of quietly running the
// wrong workflow.
func (a *API) createRun(w http.ResponseWriter, r *http.Request, p authn.Principal) {
	ns := r.PathValue("ns")

	body, err := readBody(r, maxRunBytes)
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "body_too_large", err.Error())
		return
	}

	// The body is canonicalized BEFORE fingerprinting, so two byte-different but
	// semantically identical requests (different key order, different whitespace)
	// are correctly recognized as the same request rather than 409-ing a
	// well-behaved client.
	canonical, err := spec.CanonicalizeJSON(nonEmptyJSON(body))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}

	var req struct {
		SpecID      string         `json:"spec_id"`
		SpecName    string         `json:"spec_name"`
		TriggerKind string         `json:"trigger_kind"`
		Params      map[string]any `json:"params"`
		Priority    int32          `json:"priority"`
	}
	if err := json.Unmarshal(canonical, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if req.SpecID == "" && req.SpecName == "" {
		writeError(w, http.StatusBadRequest, "missing_spec", "either spec_id or spec_name is required")
		return
	}
	// Reject a malformed spec_id HERE rather than letting Postgres reject the
	// cast. A bad uuid is the CALLER's error, and surfacing it as a 500 tells an
	// operator the server broke when in fact the request was unusable -- which
	// sends them reading gateway logs for a fault that is not there. A
	// well-formed id that names no spec is a different answer (404) and is
	// already handled downstream.
	if req.SpecID != "" {
		if _, perr := uuid.Parse(req.SpecID); perr != nil {
			writeError(w, http.StatusBadRequest, "invalid_spec_id",
				"spec_id must be a UUID; use spec_name to create a run by name")
			return
		}
	}

	idemKey := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	requestHash := store.HashRequest(canonical)

	if idemKey != "" {
		cached, replay, err := store.GatewayClaimIdempotencyKey(r.Context(), a.db, ns, idemKey, requestHash)
		switch {
		case errors.Is(err, store.ErrIdempotencyConflict):
			writeError(w, http.StatusConflict, "idempotency_key_reused",
				"this Idempotency-Key was already used with a different request body")
			return
		case err != nil:
			a.fail(w, r, "claim idempotency key", err)
			return
		case replay:
			// Byte-for-byte the original response. Replaying it rather than
			// re-deriving it means a client sees an identical answer even if the
			// run has since changed state, which is what makes the retry safe to
			// treat as the original call.
			a.log.InfoContext(r.Context(), "replayed cached response for idempotency key",
				"namespace", ns, "idempotency_key", idemKey)
			w.Header().Set("Idempotency-Replayed", "true")
			writeRaw(w, http.StatusOK, cached)
			return
		}
	}

	run, created, err := a.wfm.CreateRun(r.Context(), CreateRunRequest{
		Namespace:      ns,
		SpecID:         req.SpecID,
		SpecName:       req.SpecName,
		IdempotencyKey: idemKey,
		TriggerKind:    orDefault(req.TriggerKind, "api"),
		Params:         req.Params,
		Priority:       req.Priority,
		Actor:          "user:" + p.Name,
	})
	if err != nil {
		// The claim must be RELEASED on failure, or a transient error would poison
		// the key permanently and the client's correct retry could never succeed.
		if idemKey != "" {
			if relErr := store.GatewayReleaseIdempotencyKey(r.Context(), a.db, ns, idemKey); relErr != nil {
				a.log.ErrorContext(r.Context(), "failed to release idempotency claim after a failed creation",
					"namespace", ns, "idempotency_key", idemKey, "error", relErr)
			}
		}
		var quota *store.RunQuotaError
		if errors.As(err, &quota) {
			writeRunQuotaExceeded(w, quota)
			return
		}
		if errors.Is(err, store.ErrMaxActiveRuns) {
			writeError(w, http.StatusTooManyRequests, "max_active_runs", err.Error())
			return
		}
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "spec_not_found", "no such spec")
			return
		}
		if errors.Is(err, store.ErrInvalidRequest) {
			// 422 rather than 400, matching submitSpec: the request was
			// well-formed HTTP and well-formed JSON; the DOCUMENT is not valid.
			writeError(w, http.StatusUnprocessableEntity, "invalid_run", err.Error())
			return
		}
		a.fail(w, r, "create run", err)
		return
	}

	payload := runPayload(run)
	status := http.StatusCreated
	if !created {
		status = http.StatusOK
	}

	if idemKey != "" {
		encoded, err := json.Marshal(payload)
		if err == nil {
			if err := store.GatewayStoreIdempotentResponse(r.Context(), a.db, ns, idemKey, encoded); err != nil {
				// The run EXISTS; only the replay cache failed. Failing the request
				// now would be worse: the client would retry and, finding the key
				// claimed with no cached response, be stuck.
				a.log.ErrorContext(r.Context(), "run created but response not cached for replay",
					"run_id", run.ID, "idempotency_key", idemKey, "error", err)
			}
		}
	}

	writeJSON(w, status, payload)
}

func (a *API) listRuns(w http.ResponseWriter, r *http.Request, _ authn.Principal) {
	f := store.RunFilter{
		SpecName: r.URL.Query().Get("spec"),
		Limit:    intParam(r, "limit", 100),
	}
	if s := r.URL.Query().Get("state"); s != "" {
		f.States = strings.Split(s, ",")
	}
	if b := r.URL.Query().Get("before"); b != "" {
		t, err := time.Parse(time.RFC3339, b)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_before", "before must be an RFC3339 timestamp")
			return
		}
		f.Before = &t
	}

	if f.Limit <= 0 || f.Limit > 500 {
		f.Limit = 100
	}
	f.Lookahead = true
	rows, err := store.GatewayListRuns(r.Context(), a.db, r.PathValue("ns"), f)
	if err != nil {
		a.fail(w, r, "list runs", err)
		return
	}
	hasMore := len(rows) > f.Limit
	if hasMore {
		rows = rows[:f.Limit]
	}
	out := make([]map[string]any, 0, len(rows))
	for _, run := range rows {
		out = append(out, runPayload(run))
	}
	total, err := store.GatewayCountRuns(r.Context(), a.db, r.PathValue("ns"), f)
	if err != nil {
		a.fail(w, r, "count runs", err)
		return
	}
	resp := map[string]any{"runs": out, "total": total}
	// Keyset cursor, so deep pagination neither degrades nor skips rows as new
	// runs arrive mid-listing.
	if hasMore {
		resp["next_before"] = rows[len(rows)-1].CreatedAt.Format(time.RFC3339Nano)
	}
	writeJSON(w, http.StatusOK, resp)
}

// getRun returns the run plus its node and attempt states, which is what a detail
// view needs; making the UI issue three requests to render one page would just
// move the joins to the client.
func (a *API) getRun(w http.ResponseWriter, r *http.Request, p authn.Principal) {
	ns, id := r.PathValue("ns"), r.PathValue("id")
	run, err := store.GatewayGetRun(r.Context(), a.db, ns, id)
	if err != nil {
		a.fail(w, r, "get run", err)
		return
	}
	nodes, err := store.GatewayListRunNodes(r.Context(), a.db, id)
	if err != nil {
		a.fail(w, r, "list run nodes", err)
		return
	}
	attempts, err := store.GatewayListRunAttempts(r.Context(), a.db, id)
	if err != nil {
		a.fail(w, r, "list run attempts", err)
		return
	}
	edges, err := store.GatewayListRunEdges(r.Context(), a.db, id)
	if err != nil {
		a.fail(w, r, "list run edges", err)
		return
	}

	nodeOut := make([]map[string]any, 0, len(nodes))
	for _, n := range nodes {
		nodeOut = append(nodeOut, map[string]any{
			"node_id": n.NodeID, "task_ref": n.TaskRef, "state": n.State,
			"trigger_rule": n.TriggerRule, "max_attempts": n.MaxAttempts,
			"attempts_used": n.AttemptsUsed, "map_index": n.MapIndex,
			"parent_node_id": n.ParentNodeID,
		})
	}
	// Always a list, never null, even when empty: the console distinguishes "a
	// single-node run genuinely has no edges" from "this field was not returned",
	// and a null collapses the two into the reading that hides a missing field.
	edgeOut := make([]map[string]any, 0, len(edges))
	for _, e := range edges {
		edgeOut = append(edgeOut, map[string]any{
			"from_node_id": e.From, "to_node_id": e.To,
			"output_to_input": rawJSON(e.OutputToInput),
		})
	}
	attemptOut := make([]map[string]any, 0, len(attempts))
	for _, at := range attempts {
		attemptOut = append(attemptOut, attemptPayload(at))
	}

	payload := runPayload(run)
	if !p.Allows("tasks.view") {
		nodeOut = []map[string]any{}
		edgeOut = []map[string]any{}
		attemptOut = []map[string]any{}
	}
	if !p.Allows("tasks.results.view") {
		for _, at := range attemptOut {
			delete(at, "error")
			delete(at, "metrics")
		}
	}
	// Dynamic-expansion records (§5.4): how many children each template made, or
	// why it made none. Always a list, for the same reason as edges.
	expOut := []map[string]any{}
	if p.Allows("tasks.view") {
		exps, err := store.GatewayListRunExpansions(r.Context(), a.db, run.ID)
		if err != nil {
			a.fail(w, r, "list run expansions", err)
			return
		}
		refusals, err := store.GatewayListRunFanInRefusals(r.Context(), a.db, run.ID)
		if err != nil {
			a.fail(w, r, "list fan-in refusals", err)
			return
		}
		// A node failed by a refused fan-in has no attempt to carry the error,
		// so the reason rides on the node itself.
		for _, n := range nodeOut {
			if reason, ok := refusals[n["node_id"].(string)]; ok {
				n["failure_reason"] = reason
			}
		}
		for _, e := range exps {
			expOut = append(expOut, map[string]any{
				"template_node_id": e.TemplateNodeID, "source_node_id": e.SourceNodeID,
				"source_output": e.SourceOutput, "item_count": e.ItemCount,
				"max_fanout": e.MaxFanout, "outcome": e.Outcome,
				"reason": e.Reason, "detail": e.Detail, "created_at": e.CreatedAt,
			})
		}
	}
	payload["nodes"] = nodeOut
	payload["edges"] = edgeOut
	payload["attempts"] = attemptOut
	payload["expansions"] = expOut
	writeJSON(w, http.StatusOK, payload)
}

func (a *API) cancelRun(w http.ResponseWriter, r *http.Request, _ authn.Principal) {
	a.lifecycle(w, r, "cancel", a.wfm.CancelRun)
}

func (a *API) pauseRun(w http.ResponseWriter, r *http.Request, _ authn.Principal) {
	a.lifecycle(w, r, "pause", a.wfm.PauseRun)
}

func (a *API) resumeRun(w http.ResponseWriter, r *http.Request, _ authn.Principal) {
	a.lifecycle(w, r, "resume", a.wfm.ResumeRun)
}

func (a *API) lifecycle(w http.ResponseWriter, r *http.Request, op string, fn func(context.Context, string, string) (store.RunRow, error)) {
	ns, id := r.PathValue("ns"), r.PathValue("id")
	// Namespace-scoped existence check BEFORE the mutation (CONTRACT-FINDINGS
	// F101). The owner acts on a run id alone, and this used to be checked only
	// afterwards, when the upstream client read the run back scoped by
	// namespace: a principal of namespace A who knew a UUID in namespace B could
	// cancel, pause or resume that run and be told 404 for it. The guard only
	// authorizes the PATH's namespace; nothing tied the run to it.
	if _, err := store.GatewayGetRun(r.Context(), a.db, ns, id); err != nil {
		a.fail(w, r, "get run", err)
		return
	}
	run, err := fn(r.Context(), ns, id)
	if errors.Is(err, store.ErrRunStateConflict) {
		// 409 with the reason, not a bare rejection: an operator who clicks pause
		// on a run that just failed deserves to be told it did not apply and why.
		writeError(w, http.StatusConflict, "run_state_conflict", err.Error())
		return
	}
	if err != nil {
		a.fail(w, r, op+" run", err)
		return
	}
	writeJSON(w, http.StatusOK, runPayload(run))
}

// ── events, with SSE live tail ─────────────────────────────────────────────

// getRunEvents returns run history, or streams it live when the client asks for
// server-sent events.
//
// The cursor is the event SEQUENCE, not a timestamp. run_events carries
// UNIQUE(run_id, seq) and is append-only, so sequence is a total order with no
// ties and no clock dependency — which is exactly what a live tail needs in order
// to resume across a reconnect without gaps or duplicates. A timestamp cursor
// would drop or repeat events written in the same millisecond.
func (a *API) getRunEvents(w http.ResponseWriter, r *http.Request, p authn.Principal) {
	ns, id := r.PathValue("ns"), r.PathValue("id")
	// Namespace-scoped existence check first: without it, event history would be
	// readable across tenants by guessing a run UUID.
	if _, err := store.GatewayGetRun(r.Context(), a.db, ns, id); err != nil {
		a.fail(w, r, "get run", err)
		return
	}

	after := int32(intParam(r, "after_seq", 0))
	// Last-Event-ID is how SSE clients resume automatically after a dropped
	// connection; honouring it means a reconnect costs nothing.
	if lastID := r.Header.Get("Last-Event-ID"); lastID != "" {
		if n, err := strconv.Atoi(lastID); err == nil && n > 0 {
			after = int32(n)
		}
	}

	if wantsSSE(r) {
		a.streamEvents(w, r, id, after)
		return
	}

	events, err := store.GatewayListRunEvents(r.Context(), a.db, id, after, intParam(r, "limit", 500))
	if err != nil {
		a.fail(w, r, "list run events", err)
		return
	}
	out := make([]map[string]any, 0, len(events))
	for _, e := range events {
		out = append(out, permittedEventPayload(e, p))
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": out})
}

// streamEvents holds the connection open and pushes new events as they land.
//
// Polling the append-only table rather than subscribing in-process because the
// events are written by ANOTHER service (event-service owns run_events) and
// possibly another replica, which this process has no in-memory signal for. The
// sequence cursor makes the poll exact rather than approximate: we never re-send
// and never skip.
func (a *API) streamEvents(w http.ResponseWriter, r *http.Request, runID string, after int32) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming_unsupported", "this server cannot stream")
		return
	}

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	// Buffering proxies would defeat a live tail entirely by holding events until
	// the response ends, which for a stream is never.
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ticker := time.NewTicker(sseInterval)
	defer ticker.Stop()
	keepalive := time.NewTicker(sseKeepalive)
	defer keepalive.Stop()

	ctx := r.Context()
	for {
		current, authErr := a.auth.AuthenticateCredential(ctx, authn.ExtractBearer(r.Header.Get("Authorization")))
		if authErr != nil || !current.Allows("runs.view") {
			return
		}
		events, err := store.GatewayListRunEvents(ctx, a.db, runID, after, sseBatch)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			a.log.ErrorContext(ctx, "SSE event read failed", "run_id", runID, "error", err)
			fmt.Fprintf(w, "event: error\ndata: {\"error\":\"read_failed\"}\n\n")
			flusher.Flush()
			return
		}
		for _, e := range events {
			payload, err := json.Marshal(permittedEventPayload(e, current))
			if err != nil {
				continue
			}
			// id: lets the client resume via Last-Event-ID after a drop.
			fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", e.Seq, e.Kind, payload)
			after = e.Seq
		}
		if len(events) > 0 {
			flusher.Flush()
			// More may be waiting; drain before parking on the ticker so a burst
			// is not delivered one poll interval at a time.
			if len(events) == sseBatch {
				continue
			}
		}

		select {
		case <-ctx.Done():
			// The client hung up. Returning cleanly, with no error log: a closed
			// tail is the normal end of every SSE stream, and logging it as a
			// failure would bury real errors under UI navigation noise.
			return
		case <-keepalive.C:
			// A comment frame proves liveness to intermediaries that would
			// otherwise reap an idle connection mid-run.
			fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
		case <-ticker.C:
		}
	}
}

// ── attempts, workers, capacity ───────────────────────────────────────────

func (a *API) listWorkers(w http.ResponseWriter, r *http.Request, _ authn.Principal) {
	rows, err := store.GatewayListWorkers(r.Context(), a.db, r.PathValue("ns"))
	if err != nil {
		a.fail(w, r, "list workers", err)
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, wk := range rows {
		out = append(out, workerPayload(wk))
	}
	writeJSON(w, http.StatusOK, map[string]any{"workers": out})
}

// getWorker returns one worker plus the tasks attributed to it.
//
// Shaped after getRun, and for the same reason: the question this address exists
// to answer is "what is this worker doing", and a worker row on its own cannot
// answer it. Making the console issue a second request per worker — or worse,
// walk every run in the namespace and filter attempts client-side, which is the
// only thing the existing route set allows — would move a join the database does
// with one partial index onto the browser.
//
// Two attempt lists, not one:
//
//   - `running` is DISPATCHED + RUNNING, the same predicate as the fleet row's
//     active_attempts count, so the two can never disagree.
//   - `recent` is the terminal tail, bounded. It exists because a DEAD worker's
//     running set is necessarily empty, and "what was it doing when it went" is
//     the whole question asked of a worker that just died.
//
// ActionRead, matching listWorkers: this exposes no tenant data /workers and
// /runs/{id} do not already expose between them, and gating the drill-down on
// admin would lock out exactly the operators who open it during an incident. The
// drain action beside it stays ActionAdminWrite, because that one changes the
// fleet.
func (a *API) getWorker(w http.ResponseWriter, r *http.Request, _ authn.Principal) {
	ns, id := r.PathValue("ns"), r.PathValue("id")
	// Namespace-scoped resolve FIRST, so a worker UUID from another tenant is a
	// 404 before any attempt of theirs is read.
	wk, err := store.GatewayGetWorker(r.Context(), a.db, ns, id)
	if err != nil {
		a.fail(w, r, "get worker", err)
		return
	}
	running, err := store.GatewayWorkerRunningAttempts(r.Context(), a.db, id)
	if err != nil {
		a.fail(w, r, "list worker running attempts", err)
		return
	}
	recent, err := store.GatewayWorkerRecentAttempts(r.Context(), a.db, id, intParam(r, "recent_limit", workerRecentLimit))
	if err != nil {
		a.fail(w, r, "list worker recent attempts", err)
		return
	}

	runningOut := make([]map[string]any, 0, len(running))
	for _, at := range running {
		runningOut = append(runningOut, workerAttemptPayload(at))
	}
	recentOut := make([]map[string]any, 0, len(recent))
	for _, at := range recent {
		recentOut = append(recentOut, workerAttemptPayload(at))
	}

	payload := workerPayload(wk)
	payload["running_attempts"] = runningOut
	payload["recent_attempts"] = recentOut
	writeJSON(w, http.StatusOK, payload)
}

// workerRecentLimit bounds the historical tail. 25 fills a screen without
// turning a detail page into a history export; the store clamps anything larger
// because there is no index on worker_id for terminal attempt states.
const workerRecentLimit = 25

// drainWorker asks the worker to stop taking new work and finish what it has.
//
// The command does not reach the worker here: the control plane never dials a
// worker (ADR-0005). It marks the worker DRAINING, and the DRAIN directive rides
// the next heartbeat RESPONSE. At a 5s heartbeat that is a 5s worst case, which is
// entirely adequate for a deploy or a scale-in — hence 202 Accepted rather than
// 200, because the drain has been recorded, not yet performed.
func (a *API) drainWorker(w http.ResponseWriter, r *http.Request, _ authn.Principal) {
	ns, id := r.PathValue("ns"), r.PathValue("id")
	if _, err := store.GatewayGetWorker(r.Context(), a.db, ns, id); err != nil {
		a.fail(w, r, "get worker", err)
		return
	}
	if err := a.rm.DrainWorker(r.Context(), ns, id); err != nil {
		a.fail(w, r, "drain worker", err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"worker_id": id,
		"state":     "DRAINING",
		"detail":    "the DRAIN directive will be delivered on the worker's next heartbeat response",
	})
}

func (a *API) capacity(w http.ResponseWriter, r *http.Request, _ authn.Principal) {
	report, err := a.rm.Capacity(r.Context(), r.PathValue("ns"))
	if err != nil {
		a.fail(w, r, "read capacity", err)
		return
	}
	writeJSON(w, http.StatusOK, report)
}

// fail maps an internal error onto a status code.
//
// Not-found is distinguished from a genuine fault so a client can tell "you asked
// for something that does not exist" from "we are broken"; everything else is 500
// with the detail in the log, never in the response, because an error string can
// carry SQL or internal identifiers.
func (a *API) fail(w http.ResponseWriter, r *http.Request, op string, err error) {
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "not found")
		return
	}
	a.log.ErrorContext(r.Context(), "request failed", "op", op, "error", err)
	writeError(w, http.StatusInternalServerError, "internal", "request failed")
}
