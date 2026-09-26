// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package rest

// Routes and shared helpers for ADR-0028: notifications, webhook triggers,
// sensors and asset lineage. The configuration tables are gateway-owned, so
// every mutation writes its audit row in the SAME transaction as the change.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/khuriwalgroup/kgf-nexus/internal/secretbox"
	"github.com/khuriwalgroup/kgf-nexus/internal/store"
	"github.com/khuriwalgroup/kgf-nexus/services/api-gateway/authn"
)

// TriggeredRunCreator creates a run for a webhook trigger through the
// workflow-manager (its CreateTriggeredRun RPC).
type TriggeredRunCreator interface {
	CreateTriggeredRun(ctx context.Context, req TriggeredRunRequest) (runID string, created bool, err error)
}

// TriggeredRunRequest mirrors the workflow-manager's request.
type TriggeredRunRequest struct {
	Namespace      string
	SpecName       string
	SpecVersion    *int32
	IdempotencyKey string
	TriggerKind    string
	TriggerDetail  map[string]any
	Params         map[string]any
	Priority       int32
}

// EventsConfig wires ADR-0028's optional pieces.
type EventsConfig struct {
	// Secrets seals channel and trigger secrets; nil refuses inline secrets.
	Secrets *secretbox.Box
	// TriggeredRuns creates webhook runs; nil makes the hook route answer 503.
	TriggeredRuns TriggeredRunCreator
	// AllowPrivateTargets permits http:// and private addresses in channel URLs.
	AllowPrivateTargets bool
	// PublicURL, when set, turns a trigger's hook path into an absolute URL.
	PublicURL string
	// TrustedProxies are the peers whose Forwarded / X-Forwarded-* headers
	// decide the hook URL, ahead of PublicURL (public_url.go). Empty: none.
	TrustedProxies []netip.Prefix
}

// WithEvents enables the notification, trigger, sensor and asset routes.
func (a *API) WithEvents(c EventsConfig) *API {
	a.events = &c
	return a
}

func (a *API) registerEventRoutes(mux *http.ServeMux) {
	// Notification channels and rules.
	mux.Handle("GET /v1/namespaces/{ns}/notification-channels", a.guard(authn.ActionRead, a.listChannels))
	mux.Handle("POST /v1/namespaces/{ns}/notification-channels", a.guard(authn.ActionAdminWrite, a.createChannel))
	mux.Handle("GET /v1/namespaces/{ns}/notification-channels/{name}", a.guard(authn.ActionRead, a.getChannel))
	mux.Handle("PUT /v1/namespaces/{ns}/notification-channels/{name}", a.guard(authn.ActionAdminWrite, a.updateChannel))
	mux.Handle("DELETE /v1/namespaces/{ns}/notification-channels/{name}", a.guard(authn.ActionAdminWrite, a.deleteChannel))
	mux.Handle("POST /v1/namespaces/{ns}/notification-channels/{name}/test", a.guard(authn.ActionAdminWrite, a.testChannel))
	mux.Handle("GET /v1/namespaces/{ns}/notification-rules", a.guard(authn.ActionRead, a.listRules))
	mux.Handle("POST /v1/namespaces/{ns}/notification-rules", a.guard(authn.ActionAdminWrite, a.createRule))
	mux.Handle("GET /v1/namespaces/{ns}/notification-rules/{name}", a.guard(authn.ActionRead, a.getRule))
	mux.Handle("PUT /v1/namespaces/{ns}/notification-rules/{name}", a.guard(authn.ActionAdminWrite, a.updateRule))
	mux.Handle("DELETE /v1/namespaces/{ns}/notification-rules/{name}", a.guard(authn.ActionAdminWrite, a.deleteRule))
	mux.Handle("GET /v1/namespaces/{ns}/notification-deliveries", a.guard(authn.ActionRead, a.listDeliveries))
	mux.Handle("GET /v1/namespaces/{ns}/notification-deliveries/{id}", a.guard(authn.ActionRead, a.getDelivery))
	mux.Handle("POST /v1/namespaces/{ns}/notification-deliveries/{id}/redrive", a.guard(authn.ActionAdminWrite, a.redriveDelivery))

	// Webhook triggers.
	mux.Handle("GET /v1/namespaces/{ns}/triggers", a.guard(authn.ActionRead, a.listTriggers))
	mux.Handle("POST /v1/namespaces/{ns}/triggers", a.guard(authn.ActionWrite, a.createTrigger))
	mux.Handle("GET /v1/namespaces/{ns}/triggers/{name}", a.guard(authn.ActionRead, a.getTrigger))
	mux.Handle("PUT /v1/namespaces/{ns}/triggers/{name}", a.guard(authn.ActionWrite, a.updateTrigger))
	mux.Handle("DELETE /v1/namespaces/{ns}/triggers/{name}", a.guard(authn.ActionWrite, a.deleteTrigger))
	mux.Handle("POST /v1/namespaces/{ns}/triggers/{name}/rotate-secret", a.guard(authn.ActionWrite, a.rotateTriggerSecret))
	mux.Handle("GET /v1/namespaces/{ns}/triggers/{name}/deliveries", a.guard(authn.ActionRead, a.listTriggerDeliveries))

	// Sensors.
	mux.Handle("GET /v1/namespaces/{ns}/sensors", a.guard(authn.ActionRead, a.listSensors))
	mux.Handle("POST /v1/namespaces/{ns}/sensors", a.guard(authn.ActionWrite, a.createSensor))
	mux.Handle("GET /v1/namespaces/{ns}/sensors/{name}", a.guard(authn.ActionRead, a.getSensor))
	mux.Handle("PUT /v1/namespaces/{ns}/sensors/{name}", a.guard(authn.ActionWrite, a.updateSensor))
	mux.Handle("DELETE /v1/namespaces/{ns}/sensors/{name}", a.guard(authn.ActionWrite, a.deleteSensor))
	mux.Handle("GET /v1/namespaces/{ns}/sensors/{name}/observations", a.guard(authn.ActionRead, a.listSensorObservations))

	// Asset lineage. The asset is addressed by ?uri= because a URI contains
	// slashes that a path segment cannot carry.
	mux.Handle("GET /v1/namespaces/{ns}/assets", a.guard(authn.ActionRead, a.listAssets))
	mux.Handle("GET /v1/namespaces/{ns}/assets/detail", a.guard(authn.ActionRead, a.getAsset))
	mux.Handle("GET /v1/namespaces/{ns}/assets/graph", a.guard(authn.ActionRead, a.assetGraph))

	// THE SECOND UNGUARDED ROUTE FAMILY (the first is login): a webhook sender
	// has no credential. It authenticates by HMAC signature inside the handler,
	// which also rate-limits by address and audits every refusal.
	mux.HandleFunc("POST /v1/hooks/{ns}/{trigger}", a.receiveHook)
}

var configName = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,62}$`)

func validName(w http.ResponseWriter, name string) bool {
	if !configName.MatchString(name) {
		writeError(w, http.StatusBadRequest, "invalid_name",
			"name must be 1-63 characters of lower-case letters, digits, '.', '_' or '-', starting with a letter or digit")
		return false
	}
	return true
}

// decodeStrict decodes a bounded JSON body, refusing unknown fields: a typo in
// a notification rule must fail loudly, not silently match nothing.
func decodeStrict(w http.ResponseWriter, r *http.Request, limit int64, out any) bool {
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, limit))
	d.DisallowUnknownFields()
	d.UseNumber()
	if err := d.Decode(out); err != nil {
		rejectBody(w, err, http.StatusBadRequest, "malformed_json", "request body is not valid: "+err.Error())
		return false
	}
	return true
}

func (a *API) eventsEnabled(w http.ResponseWriter) bool {
	if a.events == nil {
		writeError(w, http.StatusNotImplemented, "not_configured", "notifications, triggers and sensors are not enabled on this gateway")
		return false
	}
	return true
}

// withTx runs fn in a transaction and commits it.
func (a *API) withTx(ctx context.Context, fn func(tx pgx.Tx) error) error {
	tx, err := a.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Audit rows for the ADR-0028 surfaces are written with auditTx
// (service_accounts.go) inside the change's own transaction: fail closed, with
// KGF attribution. Detail must never hold secrets.

// failConfig maps store errors from the ADR-0028 surfaces.
func (a *API) failConfig(w http.ResponseWriter, r *http.Request, op string, err error) {
	var pg *pgconn.PgError
	if a.auditUnavailable(w, r, err) {
		return
	}
	switch {
	case errors.Is(err, store.ErrNotifyNotFound), errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", strings.TrimPrefix(err.Error(), "not found: "))
	case errors.Is(err, store.ErrRevisionConflict):
		writeError(w, http.StatusConflict, "revision_conflict", "the object changed since you read it; reload and retry")
	case errors.As(err, &pg) && pg.Code == "23505":
		writeError(w, http.StatusConflict, "already_exists", "an object with that name already exists in this namespace")
	case errors.As(err, &pg) && pg.Code == "23514":
		writeError(w, http.StatusUnprocessableEntity, "invalid", "the request violates a constraint: "+pg.ConstraintName)
	case errors.Is(err, secretbox.ErrNoKey):
		writeError(w, http.StatusServiceUnavailable, "secret_key_unconfigured",
			"storing a secret needs NEXUS_SECRET_KEY on the gateway; configure it, or use a secret_ref instead")
	default:
		a.fail(w, r, op, err)
	}
}

// specExists reports whether a workflow name has any version in ns.
func (a *API) specExists(ctx context.Context, ns, name string, version *int32) (bool, error) {
	var ok bool
	err := a.db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM workflow_specs WHERE namespace = $1 AND name = $2 AND ($3::int IS NULL OR version = $3))`,
		ns, name, version).Scan(&ok)
	return ok, err
}

// numToInt32 reads a json.Number field.
func numToInt32(v *json.Number, def int32, min, max int64, field string) (int32, error) {
	if v == nil || *v == "" {
		return def, nil
	}
	n, err := v.Int64()
	if err != nil || n < min || n > max {
		return 0, fmt.Errorf("%s must be an integer between %d and %d", field, min, max)
	}
	return int32(n), nil
}
