// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package rest

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/khuriwalgroup/kgf-nexus/internal/platform"
	"github.com/khuriwalgroup/kgf-nexus/internal/store"
	"github.com/khuriwalgroup/kgf-nexus/services/api-gateway/authn"
)

// Namespace quotas and usage (ADR-0021).
//
// Every namespaced route here is authorized by guard() against the namespace in
// the PATH before the handler runs (F101), and every query below is keyed by
// the principal's namespace, which guard has just proven equal to the path's.
// No handler takes a namespace, tenant or run id from the request body or query
// string, so there is nothing a caller could substitute to read or change
// another tenant's quotas or usage.
func (a *API) registerQuotaRoutes(mux *http.ServeMux) {
	mux.Handle("GET /v1/namespaces/{ns}/quotas", a.guard(authn.ActionRead, a.quotasGet))
	mux.Handle("PUT /v1/namespaces/{ns}/quotas", a.guard(authn.ActionAdminWrite, a.quotasPut))
	mux.Handle("GET /v1/namespaces/{ns}/usage", a.guard(authn.ActionRead, a.usageGet))
	// Cross-tenant, so not namespaced: a platform operator only (checked in
	// the handler, like /v1/platform/tenants).
	mux.Handle("GET /v1/platform/usage", a.guard(authn.ActionRead, a.platformUsageGet))
}

// quotaDimension is one row of the quotas page: a limit and what is using it.
type quotaDimension struct {
	Name string `json:"name"`
	// Kind: concurrent (enforced in the claim), run (enforced at run
	// creation) or storage (soft, reported only).
	Kind  string `json:"kind"`
	Unit  string `json:"unit"`
	Used  int64  `json:"used"`
	Limit int64  `json:"limit"` // 0 = unlimited
	// Enforced is false for the soft storage limit.
	Enforced bool `json:"enforced"`
}

func quotaDimensions(q platform.Quotas, u store.NamespaceUsageSnapshot) []quotaDimension {
	dims := []quotaDimension{
		{platform.DimInflightAttempts, "concurrent", "attempts", u.Inflight.InflightAttempts, q.MaxInflightAttempts, true},
		{platform.DimCPUMillis, "concurrent", "millicores", u.Inflight.CPUMillis, q.MaxCPUMillis, true},
		{platform.DimMemoryBytes, "concurrent", "bytes", u.Inflight.MemoryBytes, q.MaxMemoryBytes, true},
		{platform.DimGPUCount, "concurrent", "gpus", u.Inflight.GPUCount, q.MaxGPUCount, true},
	}
	// Every custom resource that has a limit OR is in use, so an operator sees
	// consumption of a resource before deciding to cap it.
	seen := map[string]bool{}
	for name, limit := range q.MaxCustom {
		seen[name] = true
		dims = append(dims, quotaDimension{platform.DimCustomPrefix + name, "concurrent", "units",
			u.Inflight.Custom[name], limit, true})
	}
	for name, used := range u.Inflight.Custom {
		if !seen[name] {
			dims = append(dims, quotaDimension{platform.DimCustomPrefix + name, "concurrent", "units", used, 0, true})
		}
	}
	return append(dims,
		quotaDimension{platform.DimActiveRuns, "run", "runs", u.ActiveRuns, q.MaxActiveRuns, true},
		quotaDimension{platform.DimRunsPerHour, "run", "runs/hour", u.RunsLastHour, q.MaxRunsPerHour, true},
		quotaDimension{"storage_bytes", "storage", "bytes", u.StorageBytes, q.StorageSoftLimitBytes, false},
	)
}

func (a *API) quotasGet(w http.ResponseWriter, r *http.Request, p authn.Principal) {
	rev, q, err := store.GatewayQuotasGet(r.Context(), a.db, p.Namespace)
	if err != nil {
		a.fail(w, r, "load quotas", err)
		return
	}
	usage, err := store.GatewayNamespaceUsage(r.Context(), a.db, p.Namespace)
	if err != nil {
		a.fail(w, r, "read namespace usage", err)
		return
	}
	if q.MaxCustom == nil {
		q.MaxCustom = map[string]int64{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"namespace":  p.Namespace,
		"revision":   rev,
		"quotas":     q,
		"dimensions": quotaDimensions(q, usage),
		"storage": map[string]any{
			"source": usage.StorageSource,
			"note":   "Measured from durable task-log bytes. Result-store artifacts are not metered yet, so this undercounts total storage.",
		},
	})
}

// quotasPutRequest is the PUT body: the whole quota document and the policy
// revision it was read at.
type quotasPutRequest struct {
	Revision *int64           `json:"revision"`
	Quotas   *platform.Quotas `json:"quotas"`
}

func (a *API) quotasPut(w http.ResponseWriter, r *http.Request, p authn.Principal) {
	body, err := readBody(r, 64<<10)
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "body_too_large", err.Error())
		return
	}
	var req quotasPutRequest
	d := json.NewDecoder(bytes.NewReader(body))
	d.DisallowUnknownFields()
	if err = d.Decode(&req); err == nil {
		var extra any
		if d.Decode(&extra) != io.EOF {
			err = errors.New("expected one JSON document")
		}
	}
	if err == nil && (req.Revision == nil || req.Quotas == nil) {
		// The revision is required, not defaulted: an editor that does not
		// send the one it read would overwrite whatever another admin saved.
		err = errors.New("revision and quotas are required")
	}
	if err == nil {
		err = req.Quotas.Validate()
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_quotas", err.Error())
		return
	}
	// rate_limits (ADR-0032) postdates the console's quota editor. A client
	// that omits the key keeps the stored limits instead of erasing them --
	// the rule ADR-0021 set for quotas under the platform-config route. The
	// revision check in the write still refuses a stale save, so the copy
	// below cannot resurrect limits another admin removed since.
	if !quotasBodyHas(body, "rate_limits") {
		_, stored, gerr := store.GatewayQuotasGet(r.Context(), a.db, p.Namespace)
		if gerr != nil && !errors.Is(gerr, store.ErrNotFound) {
			a.fail(w, r, "load stored rate limits", gerr)
			return
		}
		req.Quotas.RateLimits = stored.RateLimits
	}
	operator, err := a.isPlatformOperator(r, p)
	if err != nil {
		a.fail(w, r, "check platform operator", err)
		return
	}
	rev, err := store.GatewayQuotasPut(r.Context(), a.db, p.Namespace, *req.Revision, *req.Quotas, operator,
		store.AuditEntry{ActorKind: string(p.Kind), ActorLabel: p.Name, ClientIP: clientIP(r)})
	var locked *store.QuotaLockedError
	switch {
	case errors.As(err, &locked):
		writeJSON(w, http.StatusForbidden, map[string]any{
			"error": "quotas_locked", "message": err.Error(), "loosened": locked.Loosened,
		})
		return
	case errors.Is(err, store.ErrPlatformConflict):
		writeError(w, http.StatusConflict, "revision_conflict", err.Error())
		return
	case errors.Is(err, store.ErrInvalidRequest):
		writeError(w, http.StatusBadRequest, "invalid_quotas", err.Error())
		return
	case err != nil:
		a.fail(w, r, "save quotas", err)
		return
	}
	q := *req.Quotas
	if q.MaxCustom == nil {
		q.MaxCustom = map[string]int64{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"namespace": p.Namespace, "revision": rev, "quotas": q})
}

// quotasBodyHas reports whether the PUT body's quotas object names field. The
// body has already been decoded strictly, so a parse failure here means "no".
func quotasBodyHas(body []byte, field string) bool {
	var raw struct {
		Quotas map[string]json.RawMessage `json:"quotas"`
	}
	if json.Unmarshal(body, &raw) != nil {
		return false
	}
	_, ok := raw.Quotas[field]
	return ok
}

// isPlatformOperator is platformOperator without the 403: the quotas PUT asks
// the question to decide what a caller may do, not whether it may call.
func (a *API) isPlatformOperator(r *http.Request, p authn.Principal) (bool, error) {
	if p.Kind != authn.KindSession || p.Role != authn.RoleAdmin || p.Grants != nil {
		return false, nil
	}
	return store.GatewayPlatformOperator(r.Context(), a.db, p.UserID)
}

// parseUsageQuery reads from, to, group_by, cursor and limit. from/to accept
// RFC 3339 or a UTC date; the default range is the 30 days ending now.
func parseUsageQuery(r *http.Request) (store.UsageQuery, error) {
	v := r.URL.Query()
	parse := func(s string) (time.Time, error) {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			return t, nil
		}
		return time.Parse("2006-01-02", s)
	}
	now := time.Now().UTC()
	q := store.UsageQuery{From: now.Add(-30 * 24 * time.Hour), To: now, GroupBy: v.Get("group_by"), Cursor: v.Get("cursor")}
	if q.GroupBy == "" {
		q.GroupBy = "day"
	}
	var err error
	if s := v.Get("from"); s != "" {
		if q.From, err = parse(s); err != nil {
			return q, errors.New("from must be RFC 3339 or YYYY-MM-DD")
		}
	}
	if s := v.Get("to"); s != "" {
		if q.To, err = parse(s); err != nil {
			return q, errors.New("to must be RFC 3339 or YYYY-MM-DD")
		}
	}
	if s := v.Get("limit"); s != "" {
		if q.Limit, err = strconv.Atoi(s); err != nil || q.Limit < 1 {
			return q, errors.New("limit must be a positive integer")
		}
	}
	if len(q.Cursor) > 512 {
		return q, errors.New("cursor is too long")
	}
	return q, nil
}

func (a *API) usageGet(w http.ResponseWriter, r *http.Request, p authn.Principal) {
	q, err := parseUsageQuery(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_query", err.Error())
		return
	}
	page, err := store.GatewayUsageRollup(r.Context(), a.db, p.Namespace, q)
	a.writeUsagePage(w, r, p.Namespace, q, page, err)
}

func (a *API) platformUsageGet(w http.ResponseWriter, r *http.Request, p authn.Principal) {
	if !a.platformOperator(w, r, p) {
		return
	}
	q, err := parseUsageQuery(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_query", err.Error())
		return
	}
	q.GroupBy = "namespace"
	page, err := store.GatewayPlatformUsageRollup(r.Context(), a.db, q)
	a.writeUsagePage(w, r, "", q, page, err)
}

func (a *API) writeUsagePage(w http.ResponseWriter, r *http.Request, ns string, q store.UsageQuery, page store.UsagePage, err error) {
	if errors.Is(err, store.ErrInvalidRequest) {
		writeError(w, http.StatusBadRequest, "invalid_query", strings.TrimPrefix(err.Error(), store.ErrInvalidRequest.Error()+": "))
		return
	}
	if err != nil {
		a.fail(w, r, "read usage", err)
		return
	}
	out := map[string]any{
		"from": q.From.UTC().Format(time.RFC3339), "to": q.To.UTC().Format(time.RFC3339),
		"group_by": q.GroupBy, "buckets": page.Buckets, "next_cursor": page.NextCursor,
		"attribution": "attempts are counted in the UTC day they ended",
	}
	if ns != "" {
		out["namespace"] = ns
	}
	writeJSON(w, http.StatusOK, out)
}

// writeRunQuotaExceeded answers a run creation refused by a namespace quota:
// 429 with the dimension and numbers, and Retry-After for the hourly rate.
func writeRunQuotaExceeded(w http.ResponseWriter, q *store.RunQuotaError) {
	body := map[string]any{
		"error": "namespace_quota_exceeded", "message": q.Error(),
		"dimension": q.Dimension, "used": q.Used, "limit": q.Limit,
	}
	if q.RetryAfter > 0 {
		secs := int64(q.RetryAfter / time.Second)
		if secs < 1 {
			secs = 1
		}
		w.Header().Set("Retry-After", strconv.FormatInt(secs, 10))
		body["retry_after_seconds"] = secs
	}
	writeJSON(w, http.StatusTooManyRequests, body)
}
