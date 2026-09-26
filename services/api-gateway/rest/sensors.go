// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package rest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/khuriwalgroup/kgf-nexus/internal/paramap"
	"github.com/khuriwalgroup/kgf-nexus/internal/store"
	"github.com/khuriwalgroup/kgf-nexus/services/api-gateway/authn"
)

// sensorStatus derives what the console shows. "stale" is computed, not
// stored: a sensor whose lease or poll is long overdue means no workflow-manager
// replica is polling it, which the stored status cannot say.
func sensorStatus(s store.Sensor) string {
	switch {
	case !s.Enabled:
		return "disabled"
	case s.State == nil:
		return "waiting"
	}
	lease := s.State.LeaseUntil
	if time.Since(s.State.NextPollAt) > 5*time.Minute && (lease == nil || time.Now().After(*lease)) {
		return "stale"
	}
	return s.State.Status
}

func sensorPayload(s store.Sensor) map[string]any {
	out := map[string]any{
		"id": s.ID, "namespace": s.Namespace, "name": s.Name, "kind": s.Kind, "workflow": s.SpecName,
		"config": s.Config, "params_mapping": s.ParamsMapping, "poll_interval_seconds": s.PollIntervalSeconds,
		"backfill": s.Backfill, "priority": s.Priority, "enabled": s.Enabled, "created_by": s.CreatedBy,
		"created_at": s.CreatedAt, "updated_at": s.UpdatedAt, "revision": s.Revision, "status": sensorStatus(s),
	}
	if s.SpecVersion != nil {
		out["workflow_version"] = *s.SpecVersion
	}
	if st := s.State; st != nil {
		out["state"] = map[string]any{
			"status": st.Status, "baseline_done": st.BaselineDone, "cursor": st.Cursor, "watermark": st.Watermark,
			"next_poll_at": st.NextPollAt, "lease_owner": st.LeaseOwner, "lease_until": st.LeaseUntil,
			"last_poll_at": st.LastPollAt, "last_success_at": st.LastSuccessAt, "last_error": st.LastError,
			"consecutive_failures": st.ConsecutiveFailures, "observed_total": st.ObservedTotal,
			"triggered_total": st.TriggeredTotal, "pending": st.PendingCount,
			"definition_current": st.DefinitionRevision == s.Revision,
		}
	}
	return out
}

type sensorRequest struct {
	Name                string         `json:"name"`
	Kind                string         `json:"kind"`
	Workflow            string         `json:"workflow"`
	WorkflowVersion     *json.Number   `json:"workflow_version"`
	Config              map[string]any `json:"config"`
	ParamsMapping       map[string]any `json:"params_mapping"`
	PollIntervalSeconds *json.Number   `json:"poll_interval_seconds"`
	Backfill            *bool          `json:"backfill"`
	Priority            *json.Number   `json:"priority"`
	Enabled             *bool          `json:"enabled"`
	Revision            int64          `json:"revision"`
}

// supportedSensorSchemes are the object-store URI schemes a sensor may name.
// s3 is served now; gs and az are reserved for the native listers another
// change adds, and are accepted so a definition can be written ahead of them
// (its status then reports that no lister is configured).
var supportedSensorSchemes = map[string]bool{"s3": true, "gs": true, "az": true}

func validateSensorConfig(kind, workflow string, cfg map[string]any) error {
	allowed := map[string][]string{
		store.SensorObjectStore:  {"uri", "pattern"},
		store.SensorRunSucceeded: {"upstream_workflow"},
	}[kind]
	if allowed == nil {
		return fmt.Errorf("kind must be %s or %s", store.SensorObjectStore, store.SensorRunSucceeded)
	}
	for k := range cfg {
		found := false
		for _, a := range allowed {
			found = found || a == k
		}
		if !found {
			return fmt.Errorf("config.%s is not a setting of a %s sensor (allowed: %v)", k, kind, allowed)
		}
	}
	str := func(k string) string { v, _ := cfg[k].(string); return strings.TrimSpace(v) }
	switch kind {
	case store.SensorObjectStore:
		u, err := url.Parse(str("uri"))
		if err != nil || u.Host == "" || !supportedSensorSchemes[strings.ToLower(u.Scheme)] || u.User != nil || u.RawQuery != "" {
			return errors.New("config.uri must be s3://bucket/prefix (gs:// and az:// are reserved)")
		}
		if _, err := paramap.CompileGlob(str("pattern")); err != nil {
			return fmt.Errorf("config.pattern: %w", err)
		}
	case store.SensorRunSucceeded:
		up := str("upstream_workflow")
		if up == "" {
			return errors.New("config.upstream_workflow is required")
		}
		if up == workflow {
			return errors.New("a workflow cannot be triggered by its own success; that is an unbounded loop")
		}
	}
	return nil
}

func (a *API) buildSensor(ctx context.Context, ns string, req sensorRequest, base store.Sensor) (store.Sensor, int, string, error) {
	s := base
	s.Namespace = ns
	if req.Workflow != "" {
		if req.Workflow != s.SpecName && req.WorkflowVersion == nil {
			s.SpecVersion = nil
		}
		s.SpecName = req.Workflow
	}
	if req.WorkflowVersion != nil {
		v, err := numToInt32(req.WorkflowVersion, 0, 1, 1<<30, "workflow_version")
		if err != nil {
			return s, 400, "invalid", err
		}
		s.SpecVersion = &v
	}
	if s.SpecName == "" {
		return s, 422, "invalid_sensor", errors.New("workflow is required")
	}
	if ok, err := a.specExists(ctx, ns, s.SpecName, s.SpecVersion); err != nil {
		return s, 500, "", err
	} else if !ok {
		return s, 422, "unknown_workflow", fmt.Errorf("workflow %q does not exist in namespace %s", s.SpecName, ns)
	}
	if req.Config != nil {
		s.Config = req.Config
	}
	if err := validateSensorConfig(s.Kind, s.SpecName, s.Config); err != nil {
		return s, 422, "invalid_sensor", err
	}
	if req.ParamsMapping != nil {
		s.ParamsMapping = req.ParamsMapping
	}
	if err := paramap.Validate(s.ParamsMapping); err != nil {
		return s, 422, "invalid_mapping", err
	}
	var err error
	if s.PollIntervalSeconds, err = numToInt32(req.PollIntervalSeconds, orDefault32(s.PollIntervalSeconds, 30), 5, 86400, "poll_interval_seconds"); err != nil {
		return s, 400, "invalid", err
	}
	if s.Priority, err = numToInt32(req.Priority, orDefault32(s.Priority, 100), 0, 1000, "priority"); err != nil {
		return s, 400, "invalid", err
	}
	if req.Backfill != nil {
		s.Backfill = *req.Backfill
	}
	if req.Enabled != nil {
		s.Enabled = *req.Enabled
	}
	return s, 0, "", nil
}

func (a *API) listSensors(w http.ResponseWriter, r *http.Request, _ authn.Principal) {
	if !a.eventsEnabled(w) {
		return
	}
	rows, err := store.SensorList(r.Context(), a.db, r.PathValue("ns"))
	if err != nil {
		a.failConfig(w, r, "list sensors", err)
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, s := range rows {
		out = append(out, sensorPayload(s))
	}
	writeJSON(w, http.StatusOK, map[string]any{"sensors": out})
}

func (a *API) getSensor(w http.ResponseWriter, r *http.Request, _ authn.Principal) {
	if !a.eventsEnabled(w) {
		return
	}
	s, err := store.SensorGet(r.Context(), a.db, r.PathValue("ns"), r.PathValue("name"))
	if err != nil {
		a.failConfig(w, r, "get sensor", err)
		return
	}
	writeJSON(w, http.StatusOK, sensorPayload(s))
}

func (a *API) createSensor(w http.ResponseWriter, r *http.Request, p authn.Principal) {
	if !a.eventsEnabled(w) {
		return
	}
	var req sensorRequest
	if !decodeStrict(w, r, maxConfigBody, &req) || !validName(w, req.Name) {
		return
	}
	ns := r.PathValue("ns")
	s, code, errCode, err := a.buildSensor(r.Context(), ns, req, store.Sensor{Kind: req.Kind, Enabled: true,
		Config: map[string]any{}, ParamsMapping: map[string]any{}})
	if err != nil {
		if code == 500 {
			a.failConfig(w, r, "validate sensor", err)
		} else {
			writeError(w, code, errCode, err.Error())
		}
		return
	}
	s.Name, s.CreatedBy = req.Name, p.Name
	var out store.Sensor
	err = a.withTx(r.Context(), func(tx pgx.Tx) error {
		var err error
		if out, err = store.SensorCreate(r.Context(), tx, s); err != nil {
			return err
		}
		return auditTx(r, tx, p, "sensor.create", "sensor", out.ID,
			map[string]any{"name": out.Name, "kind": out.Kind, "workflow": out.SpecName, "backfill": out.Backfill})
	})
	if err != nil {
		a.failConfig(w, r, "create sensor", err)
		return
	}
	writeJSON(w, http.StatusCreated, sensorPayload(out))
}

func (a *API) updateSensor(w http.ResponseWriter, r *http.Request, p authn.Principal) {
	if !a.eventsEnabled(w) {
		return
	}
	var req sensorRequest
	if !decodeStrict(w, r, maxConfigBody, &req) {
		return
	}
	if !revisionOK(w, req.Revision) {
		return
	}
	ns, name := r.PathValue("ns"), r.PathValue("name")
	cur, err := store.SensorGet(r.Context(), a.db, ns, name)
	if err != nil {
		a.failConfig(w, r, "get sensor", err)
		return
	}
	if (req.Name != "" && req.Name != cur.Name) || (req.Kind != "" && req.Kind != cur.Kind) {
		writeError(w, http.StatusBadRequest, "immutable", "a sensor's name and kind cannot be changed")
		return
	}
	s, code, errCode, err := a.buildSensor(r.Context(), ns, req, cur)
	if err != nil {
		if code == 500 {
			a.failConfig(w, r, "validate sensor", err)
		} else {
			writeError(w, code, errCode, err.Error())
		}
		return
	}
	var out store.Sensor
	err = a.withTx(r.Context(), func(tx pgx.Tx) error {
		var err error
		if out, err = store.SensorUpdate(r.Context(), tx, s, req.Revision); err != nil {
			return err
		}
		return auditTx(r, tx, p, "sensor.update", "sensor", out.ID,
			map[string]any{"name": out.Name, "enabled": out.Enabled, "workflow": out.SpecName})
	})
	if err != nil {
		a.failConfig(w, r, "update sensor", err)
		return
	}
	writeJSON(w, http.StatusOK, sensorPayload(out))
}

func (a *API) deleteSensor(w http.ResponseWriter, r *http.Request, p authn.Principal) {
	if !a.eventsEnabled(w) {
		return
	}
	ns, name := r.PathValue("ns"), r.PathValue("name")
	err := a.withTx(r.Context(), func(tx pgx.Tx) error {
		id, err := store.SensorDelete(r.Context(), tx, ns, name)
		if err != nil {
			return err
		}
		return auditTx(r, tx, p, "sensor.delete", "sensor", id, map[string]any{"name": name})
	})
	if err != nil {
		a.failConfig(w, r, "delete sensor", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) listSensorObservations(w http.ResponseWriter, r *http.Request, _ authn.Principal) {
	if !a.eventsEnabled(w) {
		return
	}
	limit, ok := listLimit(w, r, store.DefaultObservation, store.MaxObservationPage)
	if !ok {
		return
	}
	s, err := store.SensorGet(r.Context(), a.db, r.PathValue("ns"), r.PathValue("name"))
	if err != nil {
		a.failConfig(w, r, "get sensor", err)
		return
	}
	q := r.URL.Query()
	withBaseline, _ := strconv.ParseBool(q.Get("include_baseline"))
	rows, err := store.SensorListObservations(r.Context(), a.db, s.ID, withBaseline, limit)
	if err != nil {
		a.failConfig(w, r, "list observations", err)
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, o := range rows {
		out = append(out, map[string]any{"item_key": o.ItemKey, "item": o.Item, "outcome": o.Outcome, "run_id": o.RunID,
			"attempts": o.Attempts, "last_error": o.LastError, "observed_at": o.ObservedAt, "triggered_at": o.TriggeredAt})
	}
	writeJSON(w, http.StatusOK, map[string]any{"observations": out})
}
