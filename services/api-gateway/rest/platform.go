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
		err = c.Validate()
	}
	if err != nil {
		writeError(w, 400, "invalid_policy", err.Error())
		return
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
	writeJSON(w, 200, map[string]any{"revision": c.Revision, "mode": "preview", "decisions": plans, "task_job_templates": templates, "message": "Policies are saved. No cloud controller is connected; this is a demand preview, not applied infrastructure."})
}
