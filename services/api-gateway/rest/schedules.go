// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package rest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/khuriwalgroup/kgf-nexus/internal/cron"
	"github.com/khuriwalgroup/kgf-nexus/internal/store"
	"github.com/khuriwalgroup/kgf-nexus/services/api-gateway/authn"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// The schedule surface. Reads are served locally, because ADR-0011 permits the
// gateway to read every table and a schedule listing is a projection like any other.
// Mutations go through workflow-manager, which owns the table.

// ScheduleManager is the schedule-mutation surface the gateway needs.
//
// Separate from WorkflowManager rather than added to it, so a deployment running a
// workflow-manager that predates schedules still satisfies the interface it
// implements. The gateway then reports the feature as unavailable rather than
// failing to start, which matters during a rolling upgrade (ADR-0011 rule 5).
type ScheduleManager interface {
	CreateSchedule(ctx context.Context, s store.Schedule, nextFireAt time.Time) (store.Schedule, error)
	SetScheduleEnabled(ctx context.Context, namespace, name string, enabled bool, nextFireAt *time.Time) (store.Schedule, error)
	DeleteSchedule(ctx context.Context, namespace, name string) error
}

// maxPreviewFires bounds the next-run preview.
//
// A preview is the only way an author can check that a cron expression means what
// they think before it starts creating runs, so it is worth serving generously --
// but each entry costs a grid walk, and an unbounded count would let one request
// spend arbitrary CPU inside the gateway.
const maxPreviewFires = 20

// schedulePayload renders a schedule for the wire.
//
// next_fire_at is reported as stored, INCLUDING null. A null means the schedule is
// not currently scheduled -- disabled, or claimed by a replica that is firing it
// right now, or left unarmed by one that died mid-fire. Hiding that behind a
// computed "next time it would fire" would make the third case invisible, and that
// case is the one an operator needs to see.
func schedulePayload(s store.Schedule) map[string]any {
	out := map[string]any{
		"id":        s.ID,
		"namespace": s.Namespace,
		"name":      s.Name,
		"spec_name": s.SpecName,
		"cron":      s.CronExpr,
		// Reported so a caller is never in doubt about which zone the cron was
		// evaluated in. It is always UTC in this release, and saying so beats
		// omitting the field and letting a client assume local time.
		"timezone":       s.Timezone,
		"params":         s.Params,
		"priority":       s.Priority,
		"overlap_policy": s.OverlapPolicy,
		"catchup_limit":  s.CatchupLimit,
		"enabled":        s.Enabled,
		"next_fire_at":   s.NextFireAt,
		"last_fire_at":   s.LastFireAt,
		"created_at":     s.CreatedAt,
		"updated_at":     s.UpdatedAt,
		"created_by":     s.CreatedBy,
	}
	if s.SpecVersion != nil {
		out["spec_version"] = *s.SpecVersion
	}
	return out
}

// createScheduleRequest is the submission body.
type createScheduleRequest struct {
	Name          string         `json:"name"`
	SpecName      string         `json:"spec_name"`
	SpecVersion   *int32         `json:"spec_version"`
	Cron          string         `json:"cron"`
	Timezone      string         `json:"timezone"`
	Params        map[string]any `json:"params"`
	Priority      int32          `json:"priority"`
	OverlapPolicy string         `json:"overlap_policy"`
	CatchupLimit  int32          `json:"catchup_limit"`
	// Enabled defaults to TRUE when absent, because a schedule submitted by an
	// author who did not mention it is one they want running. A pointer
	// distinguishes "absent" from an explicit false.
	Enabled *bool `json:"enabled"`
}

func (a *API) createSchedule(w http.ResponseWriter, r *http.Request, p authn.Principal) {
	if a.schedules == nil {
		scheduleUnavailable(w)
		return
	}
	body, err := readBody(r, maxSpecBytes)
	if err != nil {
		writeError(w, http.StatusBadRequest, "unreadable_body", err.Error())
		return
	}
	var req createScheduleRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "malformed_json", "request body is not valid JSON")
		return
	}

	if req.Name == "" || req.SpecName == "" || req.Cron == "" {
		writeError(w, http.StatusBadRequest, "missing_field",
			"name, spec_name and cron are all required")
		return
	}

	// Parse the cron HERE, at submit time, and reject a bad one.
	//
	// The alternative is storing it and discovering the problem in the fire loop,
	// where the only possible outcome is a schedule that looks active and never
	// fires -- the failure an operator cannot diagnose. The error names the field
	// and the token, because this is something a person has to fix.
	// The timezone is resolved BEFORE the cron, because the expression's meaning
	// depends on it: "0 2 * * *" is a different set of instants in London than in UTC.
	// An unknown zone is rejected here rather than stored, since a schedule that
	// cannot be compiled is one that silently never fires.
	loc, err := cron.LoadLocation(req.Timezone)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_timezone", err.Error())
		return
	}
	expr, err := cron.ParseIn(req.Cron, loc)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_cron", err.Error())
		return
	}
	switch req.OverlapPolicy {
	case "", store.OverlapSkip, store.OverlapAllow, store.OverlapQueueOne:
	default:
		writeError(w, http.StatusBadRequest, "invalid_overlap_policy",
			fmt.Sprintf("overlap_policy must be one of %q, %q, %q",
				store.OverlapSkip, store.OverlapAllow, store.OverlapQueueOne))
		return
	}
	if req.CatchupLimit < 0 {
		writeError(w, http.StatusBadRequest, "invalid_catchup_limit",
			"catchup_limit must be zero or positive; zero means fire only the tick that just came due")
		return
	}

	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}

	// The first fire time is computed from now, so a schedule created at 10:07 with
	// "0 * * * *" first fires at 11:00 rather than immediately.
	next, err := expr.Next(a.clock().UTC())
	if err != nil {
		writeError(w, http.StatusBadRequest, "unreachable_cron", err.Error())
		return
	}

	created, err := a.schedules.CreateSchedule(r.Context(), store.Schedule{
		Namespace:     r.PathValue("ns"),
		Name:          req.Name,
		SpecName:      req.SpecName,
		SpecVersion:   req.SpecVersion,
		CronExpr:      req.Cron,
		Timezone:      loc.String(),
		Params:        req.Params,
		Priority:      req.Priority,
		OverlapPolicy: req.OverlapPolicy,
		CatchupLimit:  req.CatchupLimit,
		Enabled:       enabled,
		CreatedBy:     p.Name,
	}, next)
	if err != nil {
		a.failSchedule(w, r, "create schedule", err)
		return
	}

	out := schedulePayload(created)
	// The preview comes back on CREATE as well as on read, so an author sees
	// immediately whether the expression they wrote means what they intended --
	// before it has created anything.
	out["next_fires"] = previewFires(expr, a.clock().UTC(), 5)
	writeJSON(w, http.StatusCreated, out)
}

func (a *API) listSchedules(w http.ResponseWriter, r *http.Request, _ authn.Principal) {
	rows, err := store.WFMListSchedules(r.Context(), a.db, r.PathValue("ns"), intParam(r, "limit", 100))
	if err != nil {
		a.failSchedule(w, r, "list schedules", err)
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, s := range rows {
		out = append(out, schedulePayload(s))
	}
	writeJSON(w, http.StatusOK, map[string]any{"schedules": out})
}

func (a *API) getSchedule(w http.ResponseWriter, r *http.Request, _ authn.Principal) {
	s, err := store.WFMGetSchedule(r.Context(), a.db, r.PathValue("ns"), r.PathValue("name"))
	if err != nil {
		a.failSchedule(w, r, "get schedule", err)
		return
	}
	out := schedulePayload(s)
	if expr, perr := parseScheduleCron(s); perr == nil {
		out["next_fires"] = previewFires(expr, a.clock().UTC(), previewCount(r))
	} else {
		// A stored schedule whose cron no longer parses cannot fire at all, and the
		// read is where an operator will look when it stops. Reporting the reason
		// beats omitting the field and leaving them to guess.
		out["cron_error"] = perr.Error()
	}
	writeJSON(w, http.StatusOK, out)
}

// setScheduleEnabled handles both enable and disable.
func (a *API) setScheduleEnabled(enabled bool) func(http.ResponseWriter, *http.Request, authn.Principal) {
	return func(w http.ResponseWriter, r *http.Request, _ authn.Principal) {
		if a.schedules == nil {
			scheduleUnavailable(w)
			return
		}
		ns, name := r.PathValue("ns"), r.PathValue("name")

		var next *time.Time
		if enabled {
			// Re-enabling must recompute the fire time rather than restore the old
			// one: a schedule disabled for a week has a next_fire_at deep in the
			// past, and restoring it would fire a burst of catch-up the operator
			// did not ask for by pressing "enable".
			s, err := store.WFMGetSchedule(r.Context(), a.db, ns, name)
			if err != nil {
				a.failSchedule(w, r, "get schedule", err)
				return
			}
			expr, perr := parseScheduleCron(s)
			if perr != nil {
				writeError(w, http.StatusBadRequest, "invalid_cron",
					"this schedule cannot be compiled and so cannot be enabled: "+perr.Error())
				return
			}
			t, nerr := expr.Next(a.clock().UTC())
			if nerr != nil {
				writeError(w, http.StatusBadRequest, "unreachable_cron", nerr.Error())
				return
			}
			next = &t
		}

		s, err := a.schedules.SetScheduleEnabled(r.Context(), ns, name, enabled, next)
		if err != nil {
			a.failSchedule(w, r, "set schedule enabled", err)
			return
		}
		writeJSON(w, http.StatusOK, schedulePayload(s))
	}
}

func (a *API) deleteSchedule(w http.ResponseWriter, r *http.Request, _ authn.Principal) {
	if a.schedules == nil {
		scheduleUnavailable(w)
		return
	}
	if err := a.schedules.DeleteSchedule(r.Context(), r.PathValue("ns"), r.PathValue("name")); err != nil {
		a.failSchedule(w, r, "delete schedule", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// previewSchedule evaluates a cron expression WITHOUT creating anything.
//
// Exists so an author can check an expression before committing to it. Validating by
// creating a disabled schedule and reading it back would leave a row behind on every
// typo, and the whole point is to catch the typo first.
func (a *API) previewSchedule(w http.ResponseWriter, r *http.Request, _ authn.Principal) {
	expr := r.URL.Query().Get("cron")
	if expr == "" {
		writeError(w, http.StatusBadRequest, "missing_cron", "the cron query parameter is required")
		return
	}
	// The zone is part of the question: the same expression previews different
	// instants in different zones, and an author checking a London schedule needs to
	// see London instants.
	tz := r.URL.Query().Get("timezone")
	loc, lerr := cron.LoadLocation(tz)
	if lerr != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"cron":     expr,
			"timezone": tz,
			"valid":    false,
			"error":    lerr.Error(),
		})
		return
	}
	parsed, err := cron.ParseIn(expr, loc)
	if err != nil {
		// 200 with a valid:false body, not 400. This endpoint's job is to ANSWER
		// "is this expression good", so a bad expression is a successful answer --
		// a 400 would make a UI treat it as a request failure.
		writeJSON(w, http.StatusOK, map[string]any{
			"cron":     expr,
			"timezone": loc.String(),
			"valid":    false,
			"error":    err.Error(),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"cron":       parsed.Expr(),
		"valid":      true,
		"timezone":   loc.String(),
		"next_fires": previewFires(parsed, a.clock().UTC(), previewCount(r)),
	})
}

// parseScheduleCron compiles a stored schedule's expression in its own timezone.
//
// Mirrors the workflow-manager's parseIn: the gateway must agree with the owner about
// what a schedule means, and two places computing fire times from different zones is
// the sort of disagreement that shows up as "the console says 02:00 and it ran at
// 03:00".
func parseScheduleCron(s store.Schedule) (*cron.Schedule, error) {
	loc, err := cron.LoadLocation(s.Timezone)
	if err != nil {
		return nil, err
	}
	return cron.ParseIn(s.CronExpr, loc)
}

// previewFires renders the next n fire times.
//
// Formatted in the SCHEDULE's location, so the offset in each timestamp shows what the
// author's wall clock will read. An author previewing a London schedule that sees
// "02:00Z" has learned nothing about whether it runs at 02:00 for them.
func previewFires(expr *cron.Schedule, from time.Time, n int) []string {
	times, _ := expr.NextN(from, n)
	out := make([]string, 0, len(times))
	for _, t := range times {
		out = append(out, t.In(expr.Location()).Format(time.RFC3339))
	}
	return out
}

func previewCount(r *http.Request) int {
	n := intParam(r, "count", 5)
	if n < 1 {
		n = 1
	}
	if n > maxPreviewFires {
		n = maxPreviewFires
	}
	return n
}

// failSchedule maps schedule errors, including the not-found the shared handler
// does not know about.
func (a *API) failSchedule(w http.ResponseWriter, r *http.Request, op string, err error) {
	if errors.Is(err, store.ErrScheduleNotFound) {
		writeError(w, http.StatusNotFound, "schedule_not_found", "no such schedule")
		return
	}
	// The owner classifies a duplicate name as AlreadyExists, because a
	// pgconn.PgError does not survive a gRPC hop as a typed error. Mapping the
	// STATUS CODE here keeps the gateway out of the business of matching Postgres
	// message text, which is not a stable contract.
	if status.Code(err) == codes.AlreadyExists {
		writeError(w, http.StatusConflict, "schedule_exists",
			"a schedule with this name already exists in this namespace")
		return
	}
	if status.Code(err) == codes.NotFound {
		writeError(w, http.StatusNotFound, "schedule_not_found", "no such schedule")
		return
	}
	if status.Code(err) == codes.InvalidArgument {
		writeError(w, http.StatusBadRequest, "invalid_schedule", status.Convert(err).Message())
		return
	}
	a.fail(w, r, op, err)
}

// scheduleUnavailable reports that this gateway has no schedule upstream.
//
// 501 rather than 500: the request was fine and the server simply cannot serve it,
// which is what a client needs to distinguish a misconfiguration from an outage.
func scheduleUnavailable(w http.ResponseWriter) {
	writeError(w, http.StatusNotImplemented, "schedules_unavailable",
		"this gateway has no schedule service configured")
}

// ── backfills ───────────────────────────────────────────────────────────────

// BackfillManager is the backfill-mutation surface the gateway needs.
//
// Separate from ScheduleManager for the same reason ScheduleManager is separate from
// WorkflowManager: a gateway pointed at an older workflow-manager satisfies the
// interfaces that exist and reports the rest unavailable, rather than failing to start.
type BackfillManager interface {
	StartBackfill(ctx context.Context, req BackfillStartRequest) (store.Backfill, error)
	PauseBackfill(ctx context.Context, namespace, id string) (store.Backfill, error)
	ResumeBackfill(ctx context.Context, namespace, id string) (store.Backfill, error)
	CancelBackfill(ctx context.Context, namespace, id string) (store.Backfill, error)
}

// BackfillStartRequest is the gateway's view of a backfill request.
type BackfillStartRequest struct {
	Namespace  string
	ScheduleID string
	From       time.Time
	To         time.Time
	MaxActive  int32
	MaxRuns    int32
	CreatedBy  string
}

// backfillPayload renders one backfill.
//
// scheduleName is optional and rendered only when known. A backfill is addressed
// by its own id for its whole life (it outlives the interval that named it), so
// schedule_id alone leaves a client holding a uuid it cannot turn into a link
// without listing every schedule and scanning for a match. The name is supplied
// where it is already cheap to resolve and omitted elsewhere rather than making
// every list query pay for a join it does not need.
func backfillPayload(b store.Backfill, scheduleName ...string) map[string]any {
	out := map[string]any{
		"id":          b.ID,
		"schedule_id": b.ScheduleID,
		"namespace":   b.Namespace,
		"from":        b.FromTime,
		"to":          b.ToTime,
		// The next LOGICAL tick to generate, or null when the interval is exhausted.
		// Reported because it is the only way to see how far a long backfill has got.
		"cursor_at":    b.CursorAt,
		"max_active":   b.MaxActive,
		"max_runs":     b.MaxRuns,
		"runs_created": b.RunsCreated,
		"state":        b.State,
		"detail":       b.Detail,
		"created_at":   b.CreatedAt,
		"updated_at":   b.UpdatedAt,
		"ended_at":     b.EndedAt,
		"created_by":   b.CreatedBy,
	}
	if len(scheduleName) > 0 && scheduleName[0] != "" {
		out["schedule_name"] = scheduleName[0]
	}
	return out
}

type startBackfillRequest struct {
	From      string `json:"from"`
	To        string `json:"to"`
	MaxActive int32  `json:"max_active"`
	MaxRuns   int32  `json:"max_runs"`
}

func (a *API) startBackfill(w http.ResponseWriter, r *http.Request, p authn.Principal) {
	if a.backfills == nil {
		scheduleUnavailable(w)
		return
	}
	ns, name := r.PathValue("ns"), r.PathValue("name")
	sch, err := store.WFMGetSchedule(r.Context(), a.db, ns, name)
	if err != nil {
		a.failSchedule(w, r, "get schedule", err)
		return
	}

	body, err := readBody(r, maxSpecBytes)
	if err != nil {
		writeError(w, http.StatusBadRequest, "unreadable_body", err.Error())
		return
	}
	var req startBackfillRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "malformed_json", "request body is not valid JSON")
		return
	}

	from, err := time.Parse(time.RFC3339, req.From)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_from", "from must be an RFC3339 timestamp")
		return
	}
	to, err := time.Parse(time.RFC3339, req.To)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_to", "to must be an RFC3339 timestamp")
		return
	}

	bf, err := a.backfills.StartBackfill(r.Context(), BackfillStartRequest{
		Namespace: ns, ScheduleID: sch.ID, From: from.UTC(), To: to.UTC(),
		MaxActive: req.MaxActive, MaxRuns: req.MaxRuns, CreatedBy: p.Name,
	})
	if err != nil {
		a.failSchedule(w, r, "start backfill", err)
		return
	}
	writeJSON(w, http.StatusCreated, backfillPayload(bf, sch.Name))
}

func (a *API) listBackfills(w http.ResponseWriter, r *http.Request, _ authn.Principal) {
	ns, name := r.PathValue("ns"), r.PathValue("name")
	sch, err := store.WFMGetSchedule(r.Context(), a.db, ns, name)
	if err != nil {
		a.failSchedule(w, r, "get schedule", err)
		return
	}
	rows, err := store.WFMListBackfills(r.Context(), a.db, ns, sch.ID, intParam(r, "limit", 50))
	if err != nil {
		a.failSchedule(w, r, "list backfills", err)
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, b := range rows {
		out = append(out, backfillPayload(b, sch.Name))
	}
	writeJSON(w, http.StatusOK, map[string]any{"backfills": out})
}

func (a *API) getBackfill(w http.ResponseWriter, r *http.Request, _ authn.Principal) {
	b, err := store.WFMGetBackfill(r.Context(), a.db, r.PathValue("ns"), r.PathValue("id"))
	if err != nil {
		a.failBackfill(w, r, "get backfill", err)
		return
	}
	// Resolve the schedule's NAME here specifically: this route is reached by
	// backfill id alone, so it is the one place a client has no other way to know
	// which schedule it is looking at. A failure is not fatal -- the backfill is
	// what was asked for, and answering without the link is better than a 500.
	var name string
	if sch, serr := store.WFMGetScheduleByID(r.Context(), a.db, b.ScheduleID); serr == nil {
		name = sch.Name
	}
	writeJSON(w, http.StatusOK, backfillPayload(b, name))
}

// backfillAction handles pause, resume and cancel.
func (a *API) backfillAction(action string) func(http.ResponseWriter, *http.Request, authn.Principal) {
	return func(w http.ResponseWriter, r *http.Request, _ authn.Principal) {
		if a.backfills == nil {
			scheduleUnavailable(w)
			return
		}
		ns, id := r.PathValue("ns"), r.PathValue("id")
		var (
			b   store.Backfill
			err error
		)
		switch action {
		case "pause":
			b, err = a.backfills.PauseBackfill(r.Context(), ns, id)
		case "resume":
			b, err = a.backfills.ResumeBackfill(r.Context(), ns, id)
		default:
			b, err = a.backfills.CancelBackfill(r.Context(), ns, id)
		}
		if err != nil {
			a.failBackfill(w, r, action+" backfill", err)
			return
		}
		writeJSON(w, http.StatusOK, backfillPayload(b))
	}
}

func (a *API) failBackfill(w http.ResponseWriter, r *http.Request, op string, err error) {
	if errors.Is(err, store.ErrBackfillNotFound) {
		writeError(w, http.StatusNotFound, "backfill_not_found", "no such backfill")
		return
	}
	a.failSchedule(w, r, op, err)
}
