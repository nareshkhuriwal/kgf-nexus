// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package rest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/khuriwalgroup/kgf-nexus/internal/notify"
	"github.com/khuriwalgroup/kgf-nexus/internal/paramap"
	"github.com/khuriwalgroup/kgf-nexus/internal/secretbox"
	"github.com/khuriwalgroup/kgf-nexus/internal/store"
	"github.com/khuriwalgroup/kgf-nexus/services/api-gateway/authn"
)

const (
	// maxHookBody bounds an inbound webhook payload. Payloads are mapped onto
	// params, which are themselves bounded (maxRunBytes); a larger body is a
	// sender putting data where a reference belongs.
	maxHookBody = 1 << 20
	// receiptRetention keeps the delivery log. It must exceed the maximum
	// timestamp tolerance so a replay is always either a duplicate receipt or
	// a stale timestamp.
	receiptRetention = 7 * 24 * time.Hour
	// maxRotationGrace bounds how long a rotated-out secret keeps working.
	maxRotationGrace = 7 * 24 * time.Hour
)

func (a *API) hookPath(ns, id string) string {
	return "/v1/hooks/" + ns + "/" + id
}

// triggerPayload renders a trigger. r decides the public hook URL (F156): url
// is present whenever the gateway knows its public address, and url_source
// always says where it came from ("forwarded", "config" or "unknown").
func (a *API) triggerPayload(r *http.Request, t store.WebhookTrigger) map[string]any {
	out := map[string]any{
		"id": t.ID, "namespace": t.Namespace, "name": t.Name, "workflow": t.SpecName,
		"params_mapping": t.ParamsMapping, "idempotency_key_path": t.IdempotencyKeyPath,
		"priority": t.Priority, "tolerance_seconds": t.ToleranceSeconds, "enabled": t.Enabled,
		"secret_rotated_at": t.SecretRotatedAt, "previous_secret_expires_at": t.PreviousExpiresAt,
		"created_by": t.CreatedBy, "created_at": t.CreatedAt, "updated_at": t.UpdatedAt,
		"last_fired_at": t.LastFiredAt, "fire_count": t.FireCount, "revision": t.Revision,
		"path":             a.hookPath(t.Namespace, t.ID),
		"signature_scheme": "X-Nexus-Signature: v1=hex(HMAC-SHA256(secret, X-Nexus-Timestamp + \".\" + body))",
	}
	out["url_source"] = urlSourceUnknown
	if a.events != nil {
		origin, source := a.events.hookOrigin(r)
		out["url_source"] = source
		if origin != "" {
			out["url"] = origin + a.hookPath(t.Namespace, t.ID)
		}
	}
	if t.SpecVersion != nil {
		out["workflow_version"] = *t.SpecVersion
	}
	return out
}

type triggerRequest struct {
	Name               string         `json:"name"`
	Workflow           string         `json:"workflow"`
	WorkflowVersion    versionPin     `json:"workflow_version"`
	ParamsMapping      map[string]any `json:"params_mapping"`
	IdempotencyKeyPath *string        `json:"idempotency_key_path"`
	Priority           *json.Number   `json:"priority"`
	ToleranceSeconds   *json.Number   `json:"tolerance_seconds"`
	Enabled            *bool          `json:"enabled"`
	Revision           int64          `json:"revision"`
}

// buildTrigger validates a request onto base.
func (a *API) buildTrigger(ctx context.Context, ns string, req triggerRequest, base store.WebhookTrigger) (store.WebhookTrigger, int, string, error) {
	t := base
	t.Namespace = ns
	changed := req.Workflow != "" && req.Workflow != base.SpecName
	if req.Workflow != "" {
		t.SpecName = req.Workflow
	}
	if t.SpecName == "" {
		return t, 422, "invalid_trigger", errors.New("workflow is required")
	}
	var err error
	if t.SpecVersion, err = req.WorkflowVersion.apply(t.SpecVersion, changed); err != nil {
		return t, 400, "invalid", err
	}
	if ok, err := a.specExists(ctx, ns, t.SpecName, t.SpecVersion); err != nil {
		return t, 500, "", err
	} else if !ok {
		return t, 422, "unknown_workflow", fmt.Errorf("workflow %q (version %v) does not exist in namespace %s", t.SpecName, t.SpecVersion, ns)
	}
	if req.ParamsMapping != nil {
		t.ParamsMapping = req.ParamsMapping
	}
	if err := paramap.Validate(t.ParamsMapping); err != nil {
		return t, 422, "invalid_mapping", err
	}
	if req.IdempotencyKeyPath != nil {
		t.IdempotencyKeyPath = strings.TrimSpace(*req.IdempotencyKeyPath)
	}
	if t.IdempotencyKeyPath != "" {
		if _, err := paramap.ParsePath(t.IdempotencyKeyPath); err != nil {
			return t, 422, "invalid_mapping", fmt.Errorf("idempotency_key_path: %w", err)
		}
	}
	if t.Priority, err = numToInt32(req.Priority, orDefault32(t.Priority, 100), 0, 1000, "priority"); err != nil {
		return t, 400, "invalid", err
	}
	if t.ToleranceSeconds, err = numToInt32(req.ToleranceSeconds, orDefault32(t.ToleranceSeconds, 300), 30, 3600, "tolerance_seconds"); err != nil {
		return t, 400, "invalid", err
	}
	if req.Enabled != nil {
		t.Enabled = *req.Enabled
	}
	return t, 0, "", nil
}

func orDefault32(v, d int32) int32 {
	if v == 0 {
		return d
	}
	return v
}

func (a *API) listTriggers(w http.ResponseWriter, r *http.Request, _ authn.Principal) {
	if !a.eventsEnabled(w) {
		return
	}
	rows, err := store.TriggerList(r.Context(), a.db, r.PathValue("ns"))
	if err != nil {
		a.failConfig(w, r, "list triggers", err)
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, t := range rows {
		out = append(out, a.triggerPayload(r, t))
	}
	writeJSON(w, http.StatusOK, map[string]any{"triggers": out})
}

func (a *API) getTrigger(w http.ResponseWriter, r *http.Request, _ authn.Principal) {
	if !a.eventsEnabled(w) {
		return
	}
	t, err := store.TriggerGet(r.Context(), a.db, r.PathValue("ns"), r.PathValue("name"))
	if err != nil {
		a.failConfig(w, r, "get trigger", err)
		return
	}
	writeJSON(w, http.StatusOK, a.triggerPayload(r, t))
}

func (a *API) createTrigger(w http.ResponseWriter, r *http.Request, p authn.Principal) {
	if !a.eventsEnabled(w) {
		return
	}
	var req triggerRequest
	if !decodeStrict(w, r, maxConfigBody, &req) || !validName(w, req.Name) {
		return
	}
	ns := r.PathValue("ns")
	t, code, errCode, err := a.buildTrigger(r.Context(), ns, req, store.WebhookTrigger{Enabled: true, ParamsMapping: map[string]any{}})
	if err != nil {
		if code == 500 {
			a.failConfig(w, r, "validate trigger", err)
		} else {
			writeError(w, code, errCode, err.Error())
		}
		return
	}
	t.ID, t.Name, t.CreatedBy = uuid.NewString(), req.Name, p.Name
	if p.UserID != "" {
		t.CreatedByID = &p.UserID
	}
	secret := randomSecret()
	ct, err := a.events.Secrets.Seal([]byte(secret), secretbox.Context("webhook_triggers", ns, t.ID))
	if err != nil {
		a.failConfig(w, r, "seal trigger secret", err)
		return
	}
	var out store.WebhookTrigger
	err = a.withTx(r.Context(), func(tx pgx.Tx) error {
		var err error
		if out, err = store.TriggerCreate(r.Context(), tx, t, ct); err != nil {
			return err
		}
		return auditTx(r, tx, p, "trigger.create", "webhook_trigger", out.ID,
			map[string]any{"name": out.Name, "workflow": out.SpecName})
	})
	if err != nil {
		a.failConfig(w, r, "create trigger", err)
		return
	}
	body := a.triggerPayload(r, out)
	// Shown ONCE. It cannot be read back; rotate it to get a new one.
	body["secret"] = secret
	writeJSON(w, http.StatusCreated, body)
}

func (a *API) updateTrigger(w http.ResponseWriter, r *http.Request, p authn.Principal) {
	if !a.eventsEnabled(w) {
		return
	}
	var req triggerRequest
	if !decodeStrict(w, r, maxConfigBody, &req) {
		return
	}
	ns, name := r.PathValue("ns"), r.PathValue("name")
	if req.Name != "" && req.Name != name {
		writeError(w, http.StatusBadRequest, "immutable", "a trigger's name cannot be changed")
		return
	}
	cur, err := store.TriggerGet(r.Context(), a.db, ns, name)
	if err != nil {
		a.failConfig(w, r, "get trigger", err)
		return
	}
	t, code, errCode, err := a.buildTrigger(r.Context(), ns, req, cur)
	if err != nil {
		if code == 500 {
			a.failConfig(w, r, "validate trigger", err)
		} else {
			writeError(w, code, errCode, err.Error())
		}
		return
	}
	var out store.WebhookTrigger
	err = a.withTx(r.Context(), func(tx pgx.Tx) error {
		var err error
		if out, err = store.TriggerUpdate(r.Context(), tx, t, req.Revision); err != nil {
			return err
		}
		return auditTx(r, tx, p, "trigger.update", "webhook_trigger", out.ID,
			map[string]any{"name": out.Name, "workflow": out.SpecName, "enabled": out.Enabled})
	})
	if err != nil {
		a.failConfig(w, r, "update trigger", err)
		return
	}
	writeJSON(w, http.StatusOK, a.triggerPayload(r, out))
}

func (a *API) deleteTrigger(w http.ResponseWriter, r *http.Request, p authn.Principal) {
	if !a.eventsEnabled(w) {
		return
	}
	ns, name := r.PathValue("ns"), r.PathValue("name")
	err := a.withTx(r.Context(), func(tx pgx.Tx) error {
		id, err := store.TriggerDelete(r.Context(), tx, ns, name)
		if err != nil {
			return err
		}
		return auditTx(r, tx, p, "trigger.delete", "webhook_trigger", id, map[string]any{"name": name})
	})
	if err != nil {
		a.failConfig(w, r, "delete trigger", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// rotateTriggerSecret issues a new secret. The old one keeps verifying for
// grace_seconds (default 24h, 0 = revoke now), so senders can switch over.
func (a *API) rotateTriggerSecret(w http.ResponseWriter, r *http.Request, p authn.Principal) {
	if !a.eventsEnabled(w) {
		return
	}
	var req struct {
		GraceSeconds *json.Number `json:"grace_seconds"`
	}
	body, _ := readBody(r, maxConfigBody)
	if len(bytes.TrimSpace(body)) > 0 {
		d := json.NewDecoder(bytes.NewReader(body))
		d.DisallowUnknownFields()
		d.UseNumber()
		if err := d.Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "malformed_json", err.Error())
			return
		}
	}
	grace := 24 * time.Hour
	if req.GraceSeconds != nil {
		g, err := numToInt32(req.GraceSeconds, 0, 0, int64(maxRotationGrace/time.Second), "grace_seconds")
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid", err.Error())
			return
		}
		grace = time.Duration(g) * time.Second
	}
	ns, name := r.PathValue("ns"), r.PathValue("name")
	cur, err := store.TriggerGet(r.Context(), a.db, ns, name)
	if err != nil {
		a.failConfig(w, r, "get trigger", err)
		return
	}
	ctx := secretbox.Context("webhook_triggers", ns, cur.ID)
	secret := randomSecret()
	ct, err := a.events.Secrets.Seal([]byte(secret), ctx)
	if err != nil {
		a.failConfig(w, r, "seal trigger secret", err)
		return
	}
	var out store.WebhookTrigger
	err = a.withTx(r.Context(), func(tx pgx.Tx) error {
		oldCT, _, err := store.TriggerSecrets(r.Context(), tx, ns, cur.ID)
		if err != nil {
			return err
		}
		var prev []byte
		var prevExp *time.Time
		if grace > 0 {
			// Re-sealed under the current primary key, so a key rotation in
			// progress does not strand the grace-period secret.
			if pt, oerr := a.events.Secrets.Open(oldCT, ctx); oerr == nil {
				if prev, err = a.events.Secrets.Seal(pt, ctx); err != nil {
					return err
				}
				exp := time.Now().Add(grace)
				prevExp = &exp
			}
		}
		if out, err = store.TriggerRotate(r.Context(), tx, ns, name, ct, prev, prevExp); err != nil {
			return err
		}
		return auditTx(r, tx, p, "trigger.secret.rotate", "webhook_trigger", out.ID,
			map[string]any{"name": out.Name, "grace_seconds": int64(grace.Seconds())})
	})
	if err != nil {
		a.failConfig(w, r, "rotate trigger secret", err)
		return
	}
	resp := a.triggerPayload(r, out)
	resp["secret"] = secret
	writeJSON(w, http.StatusOK, resp)
}

func (a *API) listTriggerDeliveries(w http.ResponseWriter, r *http.Request, _ authn.Principal) {
	if !a.eventsEnabled(w) {
		return
	}
	t, err := store.TriggerGet(r.Context(), a.db, r.PathValue("ns"), r.PathValue("name"))
	if err != nil {
		a.failConfig(w, r, "get trigger", err)
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	rows, err := store.TriggerListReceipts(r.Context(), a.db, t.ID, limit)
	if err != nil {
		a.failConfig(w, r, "list trigger deliveries", err)
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, x := range rows {
		out = append(out, map[string]any{"received_at": x.ReceivedAt, "outcome": x.Outcome, "http_status": x.HTTPStatus,
			"run_id": x.RunID, "idempotency_key": x.IdempotencyKey, "detail": x.Detail, "signature_digest": x.SignatureDigest[:16]})
	}
	writeJSON(w, http.StatusOK, map[string]any{"deliveries": out})
}

// ── the hook ───────────────────────────────────────────────────────────────

// receiveHook authenticates a signed delivery and starts (or finds) its run.
//
// Refusals say WHAT was wrong but never which secret was tried or why a
// signature failed beyond "does not verify", and an unknown or disabled
// trigger is a 404 either way, so the route cannot be used to enumerate
// triggers.
func (a *API) receiveHook(w http.ResponseWriter, r *http.Request) {
	if a.rateLimited(w, r) {
		return
	}
	if a.events == nil {
		writeError(w, http.StatusNotFound, "not_found", "not found")
		return
	}
	ns, tid := r.PathValue("ns"), r.PathValue("trigger")
	deny := func(status int, code, msg, triggerID string) {
		a.auditHook(r, ns, triggerID, store.AuditDenied, map[string]any{"reason": code})
		writeError(w, status, code, msg)
	}
	if _, err := uuid.Parse(tid); err != nil {
		deny(http.StatusNotFound, "not_found", "not found", "")
		return
	}
	t, err := store.TriggerGet(r.Context(), a.db, ns, tid)
	if errors.Is(err, store.ErrNotifyNotFound) || (err == nil && !t.Enabled) {
		deny(http.StatusNotFound, "not_found", "not found", tid)
		return
	}
	if err != nil {
		a.fail(w, r, "load trigger", err)
		return
	}
	body, err := readBody(r, maxHookBody)
	if err != nil {
		deny(http.StatusRequestEntityTooLarge, "payload_too_large", err.Error(), t.ID)
		return
	}
	tsHeader, sigHeader := r.Header.Get("X-Nexus-Timestamp"), r.Header.Get("X-Nexus-Signature")
	if tsHeader == "" || sigHeader == "" {
		deny(http.StatusUnauthorized, "signature_required", "X-Nexus-Timestamp and X-Nexus-Signature are required", t.ID)
		return
	}
	ts, err := strconv.ParseInt(tsHeader, 10, 64)
	if err != nil {
		deny(http.StatusUnauthorized, "bad_timestamp", "X-Nexus-Timestamp must be Unix seconds", t.ID)
		return
	}
	if skew := a.clock().Sub(time.Unix(ts, 0)); skew > time.Duration(t.ToleranceSeconds)*time.Second || skew < -time.Duration(t.ToleranceSeconds)*time.Second {
		deny(http.StatusUnauthorized, "stale_timestamp",
			fmt.Sprintf("timestamp is outside the %ds tolerance; check the sender's clock", t.ToleranceSeconds), t.ID)
		return
	}
	if !a.verifyHook(r.Context(), t, tsHeader, body, sigHeader) {
		deny(http.StatusUnauthorized, "bad_signature", "signature does not verify", t.ID)
		return
	}
	// Replay protection. The digest is over the signature, which covers the
	// timestamp and the body: a byte-identical resend is a replay; a sender's
	// retry with a fresh timestamp is a new delivery and is deduplicated by the
	// idempotency key below instead.
	sum := sha256.Sum256([]byte(tsHeader + "\x00" + sigHeader))
	digest := hex.EncodeToString(sum[:])
	fresh, err := store.TriggerClaimReceipt(r.Context(), a.db, ns, t.ID, digest)
	if err != nil {
		a.fail(w, r, "record webhook receipt", err)
		return
	}
	if !fresh {
		deny(http.StatusConflict, "replayed", "this signed delivery was already received", t.ID)
		return
	}
	_ = store.TriggerPruneReceipts(r.Context(), a.db, t.ID, receiptRetention)
	finish := func(outcome string, status int, runID, key, detail string) {
		if err := store.TriggerFinishReceipt(context.WithoutCancel(r.Context()), a.db, t.ID, digest, outcome, status, runID, key, detail); err != nil {
			a.log.ErrorContext(r.Context(), "could not record webhook outcome", "trigger_id", t.ID, "error", err)
		}
	}
	var payload any
	d := json.NewDecoder(bytes.NewReader(body))
	d.UseNumber()
	if len(bytes.TrimSpace(body)) == 0 {
		payload = map[string]any{}
	} else if err := d.Decode(&payload); err != nil {
		finish("rejected", 400, "", "", "body is not JSON")
		deny(http.StatusBadRequest, "malformed_json", "the body must be JSON", t.ID)
		return
	}
	doc := map[string]any{"payload": payload, "trigger": map[string]any{"id": t.ID, "name": t.Name}}
	idem := "webhook:" + t.ID + ":" + digest[:32]
	if t.IdempotencyKeyPath != "" {
		v, found, err := paramap.Eval(t.IdempotencyKeyPath, doc)
		if err != nil || !found || paramap.Stringify(v) == "" {
			finish("rejected", 422, "", "", "no idempotency key at "+t.IdempotencyKeyPath)
			deny(http.StatusUnprocessableEntity, "missing_idempotency_key", "the payload has no value at "+t.IdempotencyKeyPath, t.ID)
			return
		}
		key := paramap.Stringify(v)
		if len(key) > 200 {
			k := sha256.Sum256([]byte(key))
			key = "sha256:" + hex.EncodeToString(k[:])
		}
		idem = "webhook:" + t.ID + ":" + key
	}
	params, err := paramap.Apply(t.ParamsMapping, doc)
	if err != nil {
		finish("rejected", 422, "", idem, err.Error())
		deny(http.StatusUnprocessableEntity, "unmapped_params", err.Error(), t.ID)
		return
	}
	if a.events.TriggeredRuns == nil {
		finish("rejected", 503, "", idem, "no workflow-manager upstream")
		writeError(w, http.StatusServiceUnavailable, "unavailable", "run creation is not configured on this gateway")
		return
	}
	runID, created, err := a.events.TriggeredRuns.CreateTriggeredRun(r.Context(), TriggeredRunRequest{
		Namespace: ns, SpecName: t.SpecName, SpecVersion: t.SpecVersion, IdempotencyKey: idem,
		TriggerKind: "webhook", Priority: t.Priority, Params: params,
		TriggerDetail: map[string]any{"trigger_id": t.ID, "trigger_name": t.Name, "delivery": digest[:16],
			"received_at": a.clock().UTC().Format(time.RFC3339Nano)},
	})
	if err != nil {
		st := status.Convert(err)
		code, httpStatus := "unavailable", http.StatusServiceUnavailable
		switch st.Code() {
		case codes.NotFound:
			code, httpStatus = "unknown_workflow", http.StatusUnprocessableEntity
		case codes.InvalidArgument:
			code, httpStatus = "invalid_params", http.StatusUnprocessableEntity
		case codes.ResourceExhausted:
			code, httpStatus = "run_limit", http.StatusTooManyRequests
		}
		// Marked rejected, so the sender's retry of this same signed delivery is
		// accepted rather than refused as a replay.
		finish("rejected", httpStatus, "", idem, code)
		a.log.WarnContext(r.Context(), "webhook run creation failed", "trigger_id", t.ID, "error", err)
		a.auditHook(r, ns, t.ID, store.AuditError, map[string]any{"reason": code})
		writeError(w, httpStatus, code, st.Message())
		return
	}
	outcome := "accepted"
	if !created {
		outcome = "deduplicated"
	}
	finish(outcome, http.StatusAccepted, runID, idem, "")
	a.auditHook(r, ns, t.ID, store.AuditSuccess, map[string]any{"run_id": runID, "created": created, "trigger": t.Name})
	writeJSON(w, http.StatusAccepted, map[string]any{"run_id": runID, "created": created, "idempotency_key": idem})
}

func (a *API) verifyHook(ctx context.Context, t store.WebhookTrigger, ts string, body []byte, sig string) bool {
	if a.events.Secrets == nil {
		return false
	}
	cur, prev, err := store.TriggerSecrets(ctx, a.db, t.Namespace, t.ID)
	if err != nil {
		return false
	}
	actx := secretbox.Context("webhook_triggers", t.Namespace, t.ID)
	for _, ct := range [][]byte{cur, prev} {
		if len(ct) == 0 {
			continue
		}
		secret, err := a.events.Secrets.Open(ct, actx)
		if err != nil {
			continue
		}
		if notify.VerifySignature(secret, ts, body, sig) {
			return true
		}
	}
	return false
}

// auditHook records a webhook delivery. Failures to audit are logged: an
// unregistered namespace in the path has no tenant to own the row.
func (a *API) auditHook(r *http.Request, ns, triggerID, outcome string, detail map[string]any) {
	if err := store.GatewayAppendAudit(context.WithoutCancel(r.Context()), a.db, store.AuditEntry{
		Namespace: ns, ActorKind: "anonymous", ActorLabel: "webhook", Action: "trigger.fire",
		TargetKind: "webhook_trigger", TargetID: triggerID, Outcome: outcome, Detail: detail, ClientIP: clientIP(r),
	}); err != nil {
		a.log.WarnContext(r.Context(), "could not audit a webhook delivery", "namespace", ns, "outcome", outcome, "error", err)
	}
}
