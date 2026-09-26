// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package rest

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/khuriwalgroup/kgf-nexus/internal/auditchain"
	"github.com/khuriwalgroup/kgf-nexus/internal/kgfctx"
	"github.com/khuriwalgroup/kgf-nexus/internal/metrics"
	"github.com/khuriwalgroup/kgf-nexus/internal/store"
	"github.com/khuriwalgroup/kgf-nexus/services/api-gateway/auditexport"
	"github.com/khuriwalgroup/kgf-nexus/services/api-gateway/authn"
)

// ── Which audit failures block the action (ADR-0025 §2, F139) ────────────
//
// Until ADR-0025 every audit write failed OPEN: a credential could be minted
// with no audit record if the insert failed. Each action is now classed:
//
//	AuditGrant   grants access or changes security configuration. FAIL
//	             CLOSED: the change and its audit row commit in ONE
//	             transaction, so a failed audit write aborts the change and
//	             the caller gets 503 audit_unavailable.
//	AuditRevoke  removes access. NEVER blocked: refusing a revocation because
//	             the audit store is down would keep a credential alive that
//	             an operator is trying to kill. A failed write is logged at
//	             ERROR ("AUDIT RECORD LOST", with the action and target so the
//	             log line is the fallback record) and counted in
//	             nexus_audit_write_failures_total{blocked="false"}, which is
//	             alerted on (OPERATIONS §25.2).
//	AuditOps     operational actions (retries, schedules, quarantine). Fail
//	             open, logged and counted: blocking them would hamper the
//	             operator during the incident the audit store's outage is part
//	             of, with no security benefit.
//
// A DENIED or ERROR outcome of any action is never blocked either: the action
// did not happen, and failing the response differently would only give an
// attacker a signal. Store-level writers that already audit inside the
// change's transaction (RBAC, cluster/platform/quota config, run deletion,
// tenant creation, email-link consumption) are fail-closed by construction.
//
// TestEveryAuditedActionIsClassified keeps this table complete: an action
// string that appears in the code and not here fails the build's tests.
type AuditClass int

const (
	AuditOps AuditClass = iota
	AuditGrant
	AuditRevoke
)

func (c AuditClass) String() string {
	return [...]string{"operational", "security_grant", "security_revoke"}[c]
}

var auditActionClasses = map[string]AuditClass{
	// authentication and sessions
	"session.login":  AuditGrant,
	"session.logout": AuditRevoke,
	// users
	"user.create":                   AuditGrant,
	"user.update":                   AuditGrant,
	"user.enable":                   AuditGrant,
	"user.disable":                  AuditRevoke,
	"user.password_reset":           AuditGrant,
	"user.password_reset_requested": AuditOps, // a link was mailed; no access changes
	"user.email_verification_sent":  AuditOps,
	"user.email_verified":           AuditGrant,
	"user.sessions_revoked":         AuditRevoke,
	"user.roles.assign":             AuditGrant,
	// machine credentials
	"token.create": AuditGrant,
	"token.revoke": AuditRevoke,
	// authorization and tenancy
	"role.save":     AuditGrant,
	"role.delete":   AuditRevoke,
	"tenant.create": AuditGrant,
	// the audit subsystem's own configuration
	"audit.policy.update":      AuditGrant,
	"audit.legal_hold.set":     AuditGrant,
	"audit.legal_hold.release": AuditGrant,
	"audit.purge":              AuditGrant,
	"audit.sink.create":        AuditGrant,
	"audit.sink.update":        AuditGrant,
	"audit.sink.rotate_secret": AuditGrant,
	"audit.sink.delete":        AuditGrant,
	// infrastructure configuration (audited in-transaction by the store)
	"platform.config.update":    AuditGrant,
	"platform.cluster.register": AuditGrant,
	"platform.cluster.update":   AuditGrant,
	"platform.cluster.delete":   AuditGrant,
	"quota.update":              AuditGrant,
	// operations
	"run.delete":        AuditOps,
	"run.retry":         AuditOps,
	"spec.delete":       AuditOps,
	"schedule.delete":   AuditOps,
	"worker.quarantine": AuditOps,
	"worker.release":    AuditOps,
	"cache.invalidate":  AuditOps,
}

// ClassOf returns an action's class. Unknown actions are treated as grants
// (fail closed): forgetting to classify must not silently fail open.
func ClassOf(action string) AuditClass {
	if c, ok := auditActionClasses[action]; ok {
		return c
	}
	return AuditGrant
}

// errAuditUnavailable marks a failure of the audit write itself.
var errAuditUnavailable = errors.New("audit record could not be written")

type auditWriteError struct{ err error }

func (e auditWriteError) Error() string { return errAuditUnavailable.Error() + ": " + e.err.Error() }
func (e auditWriteError) Unwrap() []error {
	return []error{errAuditUnavailable, e.err}
}

// auditWriteFailures counts audit writes that failed, by class and outcome.
// Package-level so every handler path records into the one family; nil until
// RegisterAuditMetrics runs, which makes it a no-op in tests.
var auditWriteFailures *metrics.CounterVec

// RegisterAuditMetrics creates nexus_audit_write_failures_total.
func RegisterAuditMetrics(reg *metrics.Registry) {
	auditWriteFailures = reg.NewCounter("nexus_audit_write_failures_total",
		"Audit rows that could not be written, by action class and whether the action was blocked.",
		"class", "blocked")
}

func countAuditFailure(c AuditClass, blocked bool) {
	if auditWriteFailures != nil {
		auditWriteFailures.With(metrics.Labels{"class": c.String(), "blocked": strconv.FormatBool(blocked)}).Inc()
	}
}

// auditedTx runs a security-sensitive change and its audit row in ONE
// transaction. fn performs the change through the transactional
// IdentityAdmin and returns the entry describing it; the entry is written
// only when fn succeeded. A failed audit write aborts the change and is
// reported as errAuditUnavailable.
func (a *API) auditedTx(r *http.Request, fn func(IdentityAdmin) (store.AuditEntry, error)) error {
	return a.identity.WithinTx(r.Context(), func(id IdentityAdmin) error {
		e, err := fn(id)
		if err != nil {
			return err
		}
		if e.ClientIP == "" {
			e.ClientIP = clientIP(r)
		}
		if err := id.AppendAudit(r.Context(), e); err != nil {
			countAuditFailure(ClassOf(e.Action), true)
			a.log.ErrorContext(r.Context(), "audit write failed; the change was rolled back",
				"action", e.Action, "error", err)
			return auditWriteError{err}
		}
		return nil
	})
}

// auditUnavailable answers 503 when err is an audit-write failure.
func (a *API) auditUnavailable(w http.ResponseWriter, _ *http.Request, err error) bool {
	if !errors.Is(err, errAuditUnavailable) {
		return false
	}
	writeError(w, http.StatusServiceUnavailable, "audit_unavailable",
		"the change was not made because its audit record could not be written; retry shortly")
	return true
}

// principalEntry builds the audit entry for an action by an authenticated caller.
func principalEntry(r *http.Request, p authn.Principal, action, targetKind, targetID, outcome string, detail map[string]any) store.AuditEntry {
	kind := string(p.Kind)
	if kind == "" {
		kind = string(authn.KindAPIKey)
	}
	e := store.AuditEntry{
		Namespace: p.Namespace, ActorKind: kind, ActorLabel: p.Name,
		Action: action, TargetKind: targetKind, TargetID: targetID,
		Outcome: outcome, Detail: detail, ClientIP: clientIP(r),
	}
	if p.UserID != "" {
		uid := p.UserID
		e.ActorID = &uid
	}
	return e
}

// auditPrincipal records an action AFTER it happened, for the classes that
// are not blocked by a failed write (revocations, operations, denials). A
// security grant must use auditedTx instead.
func (a *API) auditPrincipal(r *http.Request, p authn.Principal, action, targetKind, targetID, outcome string, detail map[string]any) {
	a.auditAttempt(r, principalEntry(r, p, action, targetKind, targetID, outcome, detail))
}

func (a *API) auditAttempt(r *http.Request, e store.AuditEntry) {
	if a.identity == nil {
		return
	}
	if e.ClientIP == "" {
		e.ClientIP = clientIP(r)
	}
	if err := a.identity.AppendAudit(r.Context(), e); err != nil {
		c := ClassOf(e.Action)
		countAuditFailure(c, false)
		a.log.ErrorContext(r.Context(), "AUDIT RECORD LOST: could not write audit record",
			"action", e.Action, "outcome", e.Outcome, "class", c.String(),
			"target_kind", e.TargetKind, "target_id", e.TargetID, "error", err)
		if c == AuditGrant && e.Outcome == store.AuditSuccess {
			// A grant should never reach here (it belongs in auditedTx); say so.
			a.log.ErrorContext(r.Context(), "a security grant was audited outside its transaction", "action", e.Action)
		}
	}
}

// ── listing and export ───────────────────────────────────────────────────

func parseAuditFilter(r *http.Request) (store.AuditFilter, error) {
	q := r.URL.Query()
	f := store.AuditFilter{
		Action: q.Get("action"), ActorID: q.Get("actor_id"), Outcome: q.Get("outcome"),
		Cursor: q.Get("cursor"), Limit: intParam(r, "limit", 100),
	}
	for name, dst := range map[string]**time.Time{"since": &f.Since, "until": &f.Until} {
		if v := q.Get(name); v != "" {
			t, err := time.Parse(time.RFC3339Nano, v)
			if err != nil {
				return f, errors.New(name + " must be an RFC 3339 timestamp")
			}
			*dst = &t
		}
	}
	return f, nil
}

func auditEntryPayload(e store.AuditRow) map[string]any {
	return map[string]any{
		"id": e.ID, "namespace": e.Namespace,
		"actor_id": e.ActorID, "actor_kind": e.ActorKind, "actor_label": e.ActorLabel,
		"action": e.Action, "target_kind": e.TargetKind, "target_id": e.TargetID,
		"outcome": e.Outcome, "detail": rawJSON(e.Detail),
		"client_ip": e.ClientIP, "occurred_at": e.OccurredAt,
		"chain_seq": e.Seq, "row_hash": e.RowHash,
	}
}

// listAudit serves one page, most recent first, with a keyset cursor.
func (a *API) listAudit(w http.ResponseWriter, r *http.Request, _ authn.Principal) {
	f, err := parseAuditFilter(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_filter", err.Error())
		return
	}
	rows, next, err := a.identity.ListAudit(r.Context(), r.PathValue("ns"), f)
	if errors.Is(err, store.ErrBadCursor) {
		writeError(w, http.StatusBadRequest, "invalid_cursor", "cursor was not issued by this server")
		return
	}
	if err != nil {
		a.fail(w, r, "list audit", err)
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, e := range rows {
		out = append(out, auditEntryPayload(e))
	}
	resp := map[string]any{"entries": out, "next_cursor": nil}
	if next != "" {
		resp["next_cursor"] = next
	}
	writeJSON(w, http.StatusOK, resp)
}

// exportAudit streams the trail as NDJSON (verifiable: header, rows in chain
// order, trailer with the chain head) or CSV (spreadsheets, SIEM import).
func (a *API) exportAudit(w http.ResponseWriter, r *http.Request, _ authn.Principal) {
	ns := r.PathValue("ns")
	f, err := parseAuditFilter(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_filter", err.Error())
		return
	}
	f.Cursor, f.Limit = "", 0
	format := r.URL.Query().Get("format")
	if format == "" {
		format = "ndjson"
	}
	if format != "ndjson" && format != "csv" && format != "kgf" {
		writeError(w, http.StatusBadRequest, "invalid_format", "format must be ndjson, csv or kgf")
		return
	}
	filters := map[string]string{}
	for _, k := range []string{"action", "actor_id", "outcome", "since", "until"} {
		if v := r.URL.Query().Get(k); v != "" {
			filters[k] = v
		}
	}
	flusher, _ := w.(http.Flusher)
	stamp := time.Now().UTC().Format("20060102T150405Z")
	started := false
	var head *auditchain.Checkpoint
	var nd *auditchain.NDJSONWriter
	var cw *auditchain.CSVWriter
	var kw *auditchain.KGFWriter
	n := 0
	err = store.GatewayExportAudit(r.Context(), a.db, ns, f, func(m store.AuditExportMeta) error {
		started = true
		head = m.Head
		w.Header().Set("Cache-Control", "no-store")
		if format == "kgf" {
			// KGF Cloud's audit event shape, one per line (OPERATIONS §25.2).
			w.Header().Set("Content-Type", "application/x-ndjson")
			w.Header().Set("Content-Disposition", `attachment; filename="audit-`+ns+`-`+stamp+`.kgf.ndjson"`)
			kw = auditchain.NewKGFWriter(w)
			return nil
		}
		if format == "csv" {
			w.Header().Set("Content-Type", "text/csv; charset=utf-8")
			w.Header().Set("Content-Disposition", `attachment; filename="audit-`+ns+`-`+stamp+`.csv"`)
			var err error
			cw, err = auditchain.NewCSVWriter(w)
			return err
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.Header().Set("Content-Disposition", `attachment; filename="audit-`+ns+`-`+stamp+`.ndjson"`)
		nd = auditchain.NewNDJSONWriter(w)
		h := auditchain.Header{Namespace: ns, Complete: len(filters) == 0, Filters: filters,
			Anchor: m.Anchor, GeneratedAt: time.Now().UTC().Format(auditchain.TimeLayout)}
		if m.ChainStartedAt != nil {
			h.ChainStartedAt = m.ChainStartedAt.UTC().Format(auditchain.TimeLayout)
		}
		return nd.WriteHeader(h)
	}, func(row store.AuditRow) error {
		var err error
		switch {
		case kw != nil:
			err = kw.Write(row.Record())
		case cw != nil:
			err = cw.Write(row.Record())
		default:
			err = nd.Write(row.Record())
		}
		n++
		if err == nil && n%1000 == 0 {
			switch {
			case kw != nil:
				err = kw.Flush()
			case cw != nil:
				err = cw.Flush()
			default:
				err = nd.Flush()
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		return err
	})
	if err != nil {
		if !started {
			a.fail(w, r, "export audit", err)
			return
		}
		// Mid-stream: the status is already sent. For NDJSON the missing trailer
		// makes the verifier report the export as truncated, which is the honest
		// signal; CSV simply ends.
		a.log.ErrorContext(r.Context(), "audit export aborted mid-stream", "namespace", ns, "rows", n, "error", err)
		return
	}
	switch {
	case kw != nil:
		_ = kw.Flush()
	case cw != nil:
		_ = cw.Flush()
	default:
		_ = nd.Close(head)
	}
}

// ── retention and legal hold ─────────────────────────────────────────────

func (a *API) getAuditPolicy(w http.ResponseWriter, r *http.Request, _ authn.Principal) {
	p, err := store.GatewayGetAuditPolicy(r.Context(), a.db, r.PathValue("ns"))
	if err != nil {
		a.fail(w, r, "get audit policy", err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (a *API) putAuditPolicy(w http.ResponseWriter, r *http.Request, p authn.Principal) {
	var b struct {
		RetentionDays *int  `json:"retention_days"`
		Revision      int64 `json:"revision"`
	}
	if err := decodeRBACBody(w, r, &b); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if b.RetentionDays != nil && (*b.RetentionDays < 1 || *b.RetentionDays > 36500) {
		writeError(w, http.StatusUnprocessableEntity, "invalid_retention", "retention_days must be 1..36500, or null to keep forever")
		return
	}
	a.applyAuditPolicy(w, r, store.AuditPolicyChange{SetRetention: true, RetentionDays: b.RetentionDays,
		Revision: b.Revision, Actor: p.Name},
		principalEntry(r, p, "audit.policy.update", "audit_policy", r.PathValue("ns"), store.AuditSuccess,
			map[string]any{"retention_days": b.RetentionDays, "previous_revision": b.Revision}))
}

func (a *API) setLegalHold(w http.ResponseWriter, r *http.Request, p authn.Principal) {
	var b struct {
		Hold     *bool  `json:"hold"`
		Reason   string `json:"reason"`
		Revision int64  `json:"revision"`
	}
	if err := decodeRBACBody(w, r, &b); err != nil || b.Hold == nil {
		writeError(w, http.StatusBadRequest, "invalid_json", `body must be {"hold": true|false, "reason": "...", "revision": N}`)
		return
	}
	b.Reason = strings.TrimSpace(b.Reason)
	if *b.Hold && b.Reason == "" {
		writeError(w, http.StatusUnprocessableEntity, "reason_required", "a legal hold needs a reason (matter, ticket or case reference)")
		return
	}
	action := "audit.legal_hold.release"
	if *b.Hold {
		action = "audit.legal_hold.set"
	}
	a.applyAuditPolicy(w, r, store.AuditPolicyChange{LegalHold: b.Hold, Reason: b.Reason, Revision: b.Revision, Actor: p.Name},
		principalEntry(r, p, action, "audit_policy", r.PathValue("ns"), store.AuditSuccess,
			map[string]any{"reason": b.Reason, "previous_revision": b.Revision}))
}

func (a *API) applyAuditPolicy(w http.ResponseWriter, r *http.Request, c store.AuditPolicyChange, e store.AuditEntry) {
	pol, err := store.GatewayPutAuditPolicy(r.Context(), a.db, r.PathValue("ns"), c, e)
	switch {
	case errors.Is(err, store.ErrRevisionConflict):
		writeJSON(w, http.StatusConflict, map[string]any{"error": "conflict",
			"message": "the policy changed; reload and retry", "current": pol})
	case err != nil && isAuditAppendError(err):
		countAuditFailure(AuditGrant, true)
		writeError(w, http.StatusServiceUnavailable, "audit_unavailable",
			"the change was not made because its audit record could not be written; retry shortly")
	case err != nil:
		a.fail(w, r, "put audit policy", err)
	default:
		writeJSON(w, http.StatusOK, pol)
	}
}

// isAuditAppendError recognises a failure inside GatewayAppendAudit by the
// prefix its errors carry.
func isAuditAppendError(err error) bool {
	return strings.Contains(err.Error(), "append audit ")
}

// purgeAudit applies the retention policy now (the loop does it hourly).
func (a *API) purgeAudit(w http.ResponseWriter, r *http.Request, p authn.Principal) {
	ns := r.PathValue("ns")
	tx, err := a.db.Begin(r.Context())
	if err != nil {
		a.fail(w, r, "purge audit", err)
		return
	}
	defer func() { _ = tx.Rollback(r.Context()) }()
	n, err := store.GatewayPurgeAudit(r.Context(), tx, ns)
	if errors.Is(err, store.ErrLegalHold) {
		_ = tx.Rollback(r.Context())
		a.auditPrincipal(r, p, "audit.purge", "audit_policy", ns, store.AuditDenied, map[string]any{"reason": "legal hold"})
		writeError(w, http.StatusConflict, "legal_hold", "the namespace is under legal hold; nothing was purged")
		return
	}
	if err != nil {
		a.fail(w, r, "purge audit", err)
		return
	}
	if err := store.GatewayAppendAudit(r.Context(), tx, principalEntry(r, p, "audit.purge", "audit_policy", ns,
		store.AuditSuccess, map[string]any{"rows_purged": n})); err != nil {
		countAuditFailure(AuditGrant, true)
		a.auditUnavailable(w, r, auditWriteError{err})
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		a.fail(w, r, "purge audit", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"rows_purged": n})
}

// ── sinks ────────────────────────────────────────────────────────────────

func sinkPayload(s store.AuditSink) map[string]any {
	b, _ := json.Marshal(s)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	return m
}

func (a *API) listAuditSinks(w http.ResponseWriter, r *http.Request, _ authn.Principal) {
	sinks, err := store.GatewayListAuditSinks(r.Context(), a.db, r.PathValue("ns"))
	if err != nil {
		a.fail(w, r, "list audit sinks", err)
		return
	}
	out := make([]map[string]any, 0, len(sinks))
	for _, s := range sinks {
		out = append(out, sinkPayload(s))
	}
	writeJSON(w, http.StatusOK, map[string]any{"sinks": out})
}

func (a *API) sinkKeysOrFail(w http.ResponseWriter) bool {
	if a.auditKeys == nil {
		writeError(w, http.StatusServiceUnavailable, "sinks_disabled",
			"audit sinks need NEXUS_AUDIT_SINK_KEY(_FILE) on the gateway (OPERATIONS §25.2)")
		return false
	}
	return true
}

func (a *API) createAuditSink(w http.ResponseWriter, r *http.Request, p authn.Principal) {
	if !a.sinkKeysOrFail(w) {
		return
	}
	ns := r.PathValue("ns")
	var b struct {
		Name      string `json:"name"`
		URL       string `json:"url"`
		BatchSize int    `json:"batch_size"`
		Enabled   *bool  `json:"enabled"`
		FromStart bool   `json:"from_start"`
		Format    string `json:"format"`
	}
	if err := decodeRBACBody(w, r, &b); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	b.Name = strings.TrimSpace(b.Name)
	var problems []string
	if b.Name == "" || len(b.Name) > 100 {
		problems = append(problems, "name is required (at most 100 characters)")
	}
	if err := a.sinkPolicy.CheckURL(b.URL); err != nil {
		problems = append(problems, err.Error())
	}
	if b.BatchSize == 0 {
		b.BatchSize = 100
	}
	if b.BatchSize < 1 || b.BatchSize > 1000 {
		problems = append(problems, "batch_size must be 1..1000")
	}
	if b.Format == "" {
		b.Format = "nexus"
	}
	if b.Format != "nexus" && b.Format != "kgf" {
		problems = append(problems, "format must be nexus or kgf")
	}
	if len(problems) > 0 {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": "invalid_sink", "problems": problems})
		return
	}
	secret, err := auditexport.NewSecret()
	if err != nil {
		a.fail(w, r, "sink secret", err)
		return
	}
	ct, nonce, err := a.auditKeys.Seal(ns, []byte(secret))
	if err != nil {
		a.fail(w, r, "seal sink secret", err)
		return
	}
	enabled := b.Enabled == nil || *b.Enabled
	s, err := store.GatewayCreateAuditSink(r.Context(), a.db, store.AuditSink{
		Namespace: ns, Name: b.Name, Kind: "webhook", Format: b.Format, URL: b.URL, Enabled: enabled, BatchSize: b.BatchSize,
		SecretCiphertext: ct, SecretNonce: nonce, KeyID: a.auditKeys.KeyID(), CreatedBy: p.Name,
	}, b.FromStart, principalEntry(r, p, "audit.sink.create", "audit_sink", "", store.AuditSuccess,
		map[string]any{"name": b.Name, "url": b.URL, "from_start": b.FromStart}))
	switch {
	case errors.Is(err, store.ErrConflict):
		writeError(w, http.StatusConflict, "sink_exists", "a sink with this name already exists")
		return
	case err != nil && isAuditAppendError(err):
		countAuditFailure(AuditGrant, true)
		a.auditUnavailable(w, r, auditWriteError{err})
		return
	case err != nil:
		a.fail(w, r, "create audit sink", err)
		return
	}
	out := sinkPayload(s)
	// Shown ONCE: stored only encrypted, never returned again.
	out["signing_secret"] = secret
	out["signing_secret_shown_once"] = true
	writeJSON(w, http.StatusCreated, out)
}

func (a *API) updateAuditSink(w http.ResponseWriter, r *http.Request, p authn.Principal) {
	ns, id := r.PathValue("ns"), r.PathValue("id")
	var b struct {
		URL       *string `json:"url"`
		Enabled   *bool   `json:"enabled"`
		BatchSize *int    `json:"batch_size"`
		Revision  int64   `json:"revision"`
	}
	if err := decodeRBACBody(w, r, &b); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if b.URL != nil {
		if err := a.sinkPolicy.CheckURL(*b.URL); err != nil {
			writeError(w, http.StatusUnprocessableEntity, "invalid_sink", err.Error())
			return
		}
	}
	if b.BatchSize != nil && (*b.BatchSize < 1 || *b.BatchSize > 1000) {
		writeError(w, http.StatusUnprocessableEntity, "invalid_sink", "batch_size must be 1..1000")
		return
	}
	a.writeSinkUpdate(w, r, ns, id, store.AuditSinkUpdate{URL: b.URL, Enabled: b.Enabled, BatchSize: b.BatchSize, Revision: b.Revision},
		principalEntry(r, p, "audit.sink.update", "audit_sink", id, store.AuditSuccess,
			map[string]any{"url": b.URL, "enabled": b.Enabled, "batch_size": b.BatchSize}), "")
}

func (a *API) rotateAuditSinkSecret(w http.ResponseWriter, r *http.Request, p authn.Principal) {
	if !a.sinkKeysOrFail(w) {
		return
	}
	ns, id := r.PathValue("ns"), r.PathValue("id")
	var b struct {
		Revision int64 `json:"revision"`
	}
	if err := decodeRBACBody(w, r, &b); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	secret, err := auditexport.NewSecret()
	if err != nil {
		a.fail(w, r, "sink secret", err)
		return
	}
	ct, nonce, err := a.auditKeys.Seal(ns, []byte(secret))
	if err != nil {
		a.fail(w, r, "seal sink secret", err)
		return
	}
	a.writeSinkUpdate(w, r, ns, id, store.AuditSinkUpdate{SecretCiphertext: ct, SecretNonce: nonce,
		KeyID: a.auditKeys.KeyID(), Revision: b.Revision},
		principalEntry(r, p, "audit.sink.rotate_secret", "audit_sink", id, store.AuditSuccess, nil), secret)
}

func (a *API) writeSinkUpdate(w http.ResponseWriter, r *http.Request, ns, id string, u store.AuditSinkUpdate, e store.AuditEntry, secret string) {
	s, err := store.GatewayUpdateAuditSink(r.Context(), a.db, ns, id, u, e)
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "not found")
	case errors.Is(err, store.ErrRevisionConflict):
		writeError(w, http.StatusConflict, "conflict", "the sink changed; reload and retry")
	case err != nil && isAuditAppendError(err):
		countAuditFailure(AuditGrant, true)
		a.auditUnavailable(w, r, auditWriteError{err})
	case err != nil:
		a.fail(w, r, "update audit sink", err)
	default:
		out := sinkPayload(s)
		if secret != "" {
			out["signing_secret"] = secret
			out["signing_secret_shown_once"] = true
		}
		writeJSON(w, http.StatusOK, out)
	}
}

func (a *API) deleteAuditSink(w http.ResponseWriter, r *http.Request, p authn.Principal) {
	ns, id := r.PathValue("ns"), r.PathValue("id")
	err := store.GatewayDeleteAuditSink(r.Context(), a.db, ns, id,
		principalEntry(r, p, "audit.sink.delete", "audit_sink", id, store.AuditSuccess, nil))
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "not found")
	case err != nil && isAuditAppendError(err):
		countAuditFailure(AuditGrant, true)
		a.auditUnavailable(w, r, auditWriteError{err})
	case err != nil:
		a.fail(w, r, "delete audit sink", err)
	default:
		writeJSON(w, http.StatusOK, map[string]any{"deleted": true})
	}
}

// WithKGFContext enables KGF Cloud attribution of audit rows: a request whose
// X-KGF-Context verifies against v has its KGF organization, scope and
// principal recorded. Attribution only -- never authorization.
func (a *API) WithKGFContext(v *kgfctx.Verifier) *API {
	a.kgf = v
	return a
}

// attachKGF puts the verified KGF context (if any) and the request's
// correlation id into r's context, where every audit writer reads them.
// An unverifiable token contributes nothing, and its request_id is not used.
func (a *API) attachKGF(r *http.Request) *http.Request {
	ctx := r.Context()
	reqID := kgfctx.SanitizeRequestID(r.Header.Get(kgfctx.RequestIDHeader))
	if tok := r.Header.Get(kgfctx.Header); tok != "" && a.kgf != nil {
		if c, err := a.kgf.Verify(tok, time.Now()); err == nil {
			ctx = kgfctx.With(ctx, c)
			if id := kgfctx.SanitizeRequestID(c.RequestID); id != "" {
				reqID = id
			}
		} else {
			a.log.WarnContext(ctx, "X-KGF-Context did not verify; the audit row will carry no KGF attribution")
		}
	}
	if reqID != "" {
		ctx = kgfctx.WithRequestID(ctx, reqID)
	}
	return r.WithContext(ctx)
}

// WithAuditExport configures sink secrets and the sink URL policy.
func (a *API) WithAuditExport(keys *auditexport.Keyring, policy auditexport.URLPolicy) *API {
	a.auditKeys, a.sinkPolicy = keys, policy
	return a
}

// registerAuditRoutes adds the audit trail surface (identity must be wired).
func (a *API) registerAuditRoutes(mux *http.ServeMux) {
	mux.Handle("GET /v1/namespaces/{ns}/audit", a.guard(authn.ActionAdminWrite, a.listAudit))
	mux.Handle("GET /v1/namespaces/{ns}/audit/export", a.guard(authn.ActionAdminWrite, a.exportAudit))
	mux.Handle("GET /v1/namespaces/{ns}/audit/policy", a.guard(authn.ActionAdminWrite, a.getAuditPolicy))
	mux.Handle("PUT /v1/namespaces/{ns}/audit/policy", a.guard(authn.ActionAdminWrite, a.putAuditPolicy))
	mux.Handle("POST /v1/namespaces/{ns}/audit/legal-hold", a.guard(authn.ActionAdminWrite, a.setLegalHold))
	mux.Handle("POST /v1/namespaces/{ns}/audit/purge", a.guard(authn.ActionAdminWrite, a.purgeAudit))
	mux.Handle("GET /v1/namespaces/{ns}/audit/sinks", a.guard(authn.ActionAdminWrite, a.listAuditSinks))
	mux.Handle("POST /v1/namespaces/{ns}/audit/sinks", a.guard(authn.ActionAdminWrite, a.createAuditSink))
	mux.Handle("PUT /v1/namespaces/{ns}/audit/sinks/{id}", a.guard(authn.ActionAdminWrite, a.updateAuditSink))
	mux.Handle("POST /v1/namespaces/{ns}/audit/sinks/{id}/rotate-secret", a.guard(authn.ActionAdminWrite, a.rotateAuditSinkSecret))
	mux.Handle("DELETE /v1/namespaces/{ns}/audit/sinks/{id}", a.guard(authn.ActionAdminWrite, a.deleteAuditSink))
}
