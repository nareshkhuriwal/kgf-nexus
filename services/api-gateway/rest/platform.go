// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0
package rest

import (
	"bytes"
	"encoding/json"
	"errors"
	"github.com/khuriwalgroup/kgf-nexus/internal/platform"
	"github.com/khuriwalgroup/kgf-nexus/internal/store"
	"github.com/khuriwalgroup/kgf-nexus/services/api-gateway/authn"
	"io"
	"net/http"
)

func (a *API) platformGet(w http.ResponseWriter, r *http.Request, p authn.Principal) {
	c, err := store.GatewayPlatformGet(r.Context(), a.db, p.Namespace)
	if err != nil {
		a.fail(w, r, "load platform config", err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}
func (a *API) platformPut(w http.ResponseWriter, r *http.Request, p authn.Principal) {
	body, err := readBody(r, 128<<10)
	if err != nil {
		writeError(w, 413, "body_too_large", err.Error())
		return
	}
	var c platform.Config
	d := json.NewDecoder(bytes.NewReader(body))
	d.DisallowUnknownFields()
	if err = d.Decode(&c); err == nil {
		var extra any
		if d.Decode(&extra) != io.EOF {
			err = errors.New("expected one JSON document")
		}
	}
	if err == nil {
		// A client that predates the field (the console before ADR-0017, or a
		// scripted PUT) omits it and decodes to 0. That means "leave the
		// default", not "take this namespace out of service", so it is
		// normalised rather than rejected.
		if c.Scheduler.FairShareWeight == 0 {
			c.Scheduler.FairShareWeight = platform.DefaultFairShareWeight
		}
		err = c.Validate()
	}
	if err != nil {
		writeError(w, 400, "invalid_policy", err.Error())
		return
	}
	// The weight is the one field in this document that is NOT only the
	// namespace's own business: raising it takes dispatch share from every
	// other namespace on the queue. A namespace admin may save the rest of the
	// policy freely but only a platform operator may change the weight
	// (ADR-0017).
	cur, err := store.GatewayPlatformGet(r.Context(), a.db, p.Namespace)
	if err != nil {
		a.fail(w, r, "load platform config", err)
		return
	}
	if cur.Scheduler.FairShareWeight != c.Scheduler.FairShareWeight && !a.platformOperator(w, r, p) {
		return
	}
	// Quotas ride in this document but have their own route and permission
	// (ADR-0021). A client that omits them -- every console before quotas, a
	// scripted PUT -- keeps the stored ones; one that sends them must send them
	// unchanged, so infrastructure.configure can never edit a quota.
	switch {
	case c.Quotas == nil:
		c.Quotas = cur.Quotas
	case !c.Quotas.Equal(quotasOrZero(cur.Quotas)) && c.Revision == cur.Revision:
		writeError(w, 400, "invalid_policy", "quotas cannot be changed here; use PUT /v1/namespaces/{ns}/quotas")
		return
	case !c.Quotas.Equal(quotasOrZero(cur.Quotas)):
		// A stale document -- quotas were changed since the client read it.
		// Carry the stored ones; the revision check answers 409, which is
		// the true reason, instead of a misleading "cannot change quotas".
		c.Quotas = cur.Quotas
	}
	c, err = store.GatewayPlatformPut(r.Context(), a.db, p.Namespace, c, store.AuditEntry{ActorKind: string(p.Kind), ActorLabel: p.Name, ClientIP: clientIP(r)})
	if errors.Is(err, store.ErrPlatformConflict) {
		writeError(w, 409, "revision_conflict", err.Error())
		return
	}
	if err != nil {
		a.fail(w, r, "save platform config", err)
		return
	}
	writeJSON(w, 200, c)
}
func (a *API) platformPlan(w http.ResponseWriter, r *http.Request, p authn.Principal) {
	c, err := store.GatewayPlatformGet(r.Context(), a.db, p.Namespace)
	if err != nil {
		a.fail(w, r, "load platform config", err)
		return
	}
	plans := []platform.Decision{}
	templates := map[string]any{}
	profiles := map[string]platform.PodProfile{}
	for _, profile := range c.Profiles {
		profiles[profile.Name] = profile
		templates[profile.Name] = platform.JobTemplate(profile)
	}
	for _, pool := range c.Pools {
		profile := profiles[pool.Profile]
		d, err := store.GatewayPlatformDemand(r.Context(), a.db, p.Namespace, pool, profile)
		if err != nil {
			a.fail(w, r, "observe scaling demand", err)
			return
		}
		plans = append(plans, platform.Plan(pool, profile, d))
	}
	// Say honestly whether anything acts on this plan. The preview itself is
	// always computed here and never applied; a registered cluster controller
	// computes and (only in apply mode) acts on its own copy of the decision.
	message := "Policies are saved. No cluster controller is registered for this namespace; this is a demand preview, not applied infrastructure."
	clusters, err := store.GatewayClusterList(r.Context(), a.db, p.Namespace)
	if err != nil {
		a.fail(w, r, "load cluster registry", err)
		return
	}
	controllers := []map[string]any{}
	for _, cl := range clusters {
		entry := map[string]any{"cluster": cl.Name, "registry_mode": cl.Mode, "reporting": cl.Status != nil}
		if cl.Status != nil {
			entry["effective_mode"] = cl.Status.EffectiveMode
			entry["last_report_at"] = cl.Status.LastReportAt
		}
		controllers = append(controllers, entry)
	}
	if len(clusters) > 0 {
		message = "This preview is computed by the gateway and is not applied. Registered cluster controllers report their own decisions and applied state under Live cluster."
	}
	writeJSON(w, 200, map[string]any{"revision": c.Revision, "mode": "preview", "decisions": plans, "task_job_templates": templates, "controllers": controllers, "message": message})
}

func quotasOrZero(q *platform.Quotas) platform.Quotas {
	if q == nil {
		return platform.Quotas{}
	}
	return *q
}
