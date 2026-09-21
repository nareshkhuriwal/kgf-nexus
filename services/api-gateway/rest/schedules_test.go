// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package rest

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/khuriwalgroup/kgf-nexus/internal/store"
	"github.com/khuriwalgroup/kgf-nexus/services/api-gateway/authn"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// fakeSchedules records what the gateway asked the owner to do, so the tests assert
// on the REST layer's decisions rather than on a database.
type fakeSchedules struct {
	created    []store.Schedule
	nextFireAt time.Time
	enabled    []bool
	deleted    []string
	err        error
}

func (f *fakeSchedules) CreateSchedule(_ context.Context, s store.Schedule, next time.Time) (store.Schedule, error) {
	if f.err != nil {
		return store.Schedule{}, f.err
	}
	f.created = append(f.created, s)
	f.nextFireAt = next
	s.ID = "sched-1"
	s.NextFireAt = &next
	s.CreatedAt, s.UpdatedAt = next, next
	return s, nil
}

func (f *fakeSchedules) SetScheduleEnabled(_ context.Context, ns, name string, enabled bool, next *time.Time) (store.Schedule, error) {
	if f.err != nil {
		return store.Schedule{}, f.err
	}
	f.enabled = append(f.enabled, enabled)
	return store.Schedule{ID: "sched-1", Namespace: ns, Name: name, Enabled: enabled, NextFireAt: next}, nil
}

func (f *fakeSchedules) DeleteSchedule(_ context.Context, ns, name string) error {
	if f.err != nil {
		return f.err
	}
	f.deleted = append(f.deleted, ns+"/"+name)
	return nil
}

// get issues an authenticated GET. The shared harness only has a post helper.
func get(t *testing.T, h http.Handler, path, key string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func newScheduleAPI(t *testing.T) (http.Handler, *fakeSchedules) {
	t.Helper()
	db := newRestDB()
	db.addKey(adminKey, "team-ml", "ci", "admin")
	db.addKey(readerKey, "team-ml", "dashboard", "reader")
	// The workflow every creation test schedules. It must exist, because
	// createSchedule now refuses a spec_name that names nothing -- so a fixture
	// without it would be testing a namespace where the workflow really is absent,
	// and every one of those tests would be asserting the wrong thing.
	db.addSpec("team-ml", "train")
	fs := &fakeSchedules{}
	api := New(db, authn.New(db), &fakeWFM{}, fakeRM{}, nil,
		slog.New(slog.NewTextHandler(io.Discard, nil))).WithSchedules(fs)
	return api.Routes(), fs
}

func TestCreateScheduleValidatesAndPreviews(t *testing.T) {
	h, fs := newScheduleAPI(t)

	body := `{"name":"nightly","spec_name":"train","cron":"0 2 * * *","catchup_limit":3}`
	rec := post(t, h, "/v1/namespaces/team-ml/schedules", adminKey, "", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status %d, want 201. body=%s", rec.Code, rec.Body)
	}

	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// The preview comes back on CREATE, so an author sees whether the expression
	// means what they intended before it has created anything.
	fires, ok := got["next_fires"].([]any)
	if !ok || len(fires) == 0 {
		t.Fatalf("response carried no next_fires preview: %v", got)
	}
	if !strings.HasSuffix(fires[0].(string), "T02:00:00Z") {
		t.Errorf("first preview fire = %v, want 02:00 UTC", fires[0])
	}
	// The owner must receive a computed fire time; a zero would make the schedule
	// due at the Unix epoch and fire immediately.
	if fs.nextFireAt.IsZero() {
		t.Error("the owner was sent a zero next_fire_at")
	}
	if len(fs.created) != 1 || fs.created[0].CatchupLimit != 3 {
		t.Errorf("owner received %+v", fs.created)
	}
	// Absent `enabled` means enabled: a schedule submitted without mentioning it is
	// one the author wants running.
	if !fs.created[0].Enabled {
		t.Error("a schedule created without an explicit enabled flag was disabled")
	}
}

// TestCreateScheduleRejectsBadCron is the important one. Storing an unparseable cron
// produces a schedule that looks active and never fires, which is the failure an
// operator cannot diagnose -- so it has to be rejected at submit time.
func TestCreateScheduleRejectsBadCron(t *testing.T) {
	h, fs := newScheduleAPI(t)

	for _, bad := range []string{"not a cron", "0 2 * *", "99 * * * *", "@reboot", ""} {
		body := `{"name":"x","spec_name":"train","cron":"` + bad + `"}`
		rec := post(t, h, "/v1/namespaces/team-ml/schedules", adminKey, "", body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("cron %q: status %d, want 400. body=%s", bad, rec.Code, rec.Body)
		}
	}
	if len(fs.created) != 0 {
		t.Errorf("%d schedules reached the owner despite an invalid cron", len(fs.created))
	}
}

// TestCreateScheduleAcceptsAnIANATimezone covers what replaced the old
// refuse-everything-but-UTC behaviour.
//
// Refusing non-UTC was the honest choice while the column was stored and unused --
// accepting "Europe/London" and evaluating in UTC would have told the author their job
// runs at 02:00 local when it ran at 02:00 UTC, wrong by an hour for half the year with
// nothing reporting it. Now the zone is actually honoured, so it is accepted, and the
// preview comes back with the author's own offset so they can see that it is.
func TestCreateScheduleAcceptsAnIANATimezone(t *testing.T) {
	h, fs := newScheduleAPI(t)

	body := `{"name":"london","spec_name":"train","cron":"0 2 * * *","timezone":"Europe/London"}`
	rec := post(t, h, "/v1/namespaces/team-ml/schedules", adminKey, "", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status %d, want 201. body=%s", rec.Code, rec.Body)
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["timezone"] != "Europe/London" {
		t.Errorf("timezone = %v, want Europe/London stored verbatim", got["timezone"])
	}
	if len(fs.created) != 1 || fs.created[0].Timezone != "Europe/London" {
		t.Errorf("owner received timezone %q, want Europe/London", fs.created[0].Timezone)
	}
	// The preview must read in the AUTHOR's zone. A London schedule previewing "02:00Z"
	// tells them nothing about whether it runs at 02:00 for them.
	fires, _ := got["next_fires"].([]any)
	if len(fires) == 0 {
		t.Fatal("no preview returned")
	}
	first, _ := fires[0].(string)
	if !strings.Contains(first, "T02:00:00") {
		t.Errorf("first preview fire = %q, want a 02:00 local wall reading", first)
	}
	if strings.HasSuffix(first, "Z") {
		t.Errorf("first preview fire = %q is rendered in UTC; it must carry the schedule's own offset", first)
	}
}

// TestCreateScheduleRejectsUnknownTimezone: an unloadable zone must fail at submit
// time. Storing it would produce a schedule that cannot be compiled and therefore
// silently never fires, which is the failure mode an operator cannot diagnose.
func TestCreateScheduleRejectsUnknownTimezone(t *testing.T) {
	h, fs := newScheduleAPI(t)

	for _, tz := range []string{"Mars/Olympus_Mons", "Not/AZone", "GMT+5"} {
		body := `{"name":"x","spec_name":"train","cron":"0 2 * * *","timezone":"` + tz + `"}`
		rec := post(t, h, "/v1/namespaces/team-ml/schedules", adminKey, "", body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("timezone %q: status %d, want 400. body=%s", tz, rec.Code, rec.Body)
		}
	}
	if len(fs.created) != 0 {
		t.Errorf("%d schedules reached the owner with an unknown timezone", len(fs.created))
	}

	// Omitted and explicit UTC both mean UTC, so a client that never sets it is safe.
	cases := map[string]string{
		"omitted":      `{"name":"tz-omitted","spec_name":"train","cron":"0 2 * * *"}`,
		"explicit UTC": `{"name":"tz-utc","spec_name":"train","cron":"0 2 * * *","timezone":"UTC"}`,
	}
	for name, body := range cases {
		rec := post(t, h, "/v1/namespaces/team-ml/schedules", adminKey, "", body)
		if rec.Code != http.StatusCreated {
			t.Errorf("%s: status %d, want 201. body=%s", name, rec.Code, rec.Body)
		}
	}
}

func TestCreateScheduleValidatesOverlapPolicy(t *testing.T) {
	h, _ := newScheduleAPI(t)

	rec := post(t, h, "/v1/namespaces/team-ml/schedules", adminKey, "",
		`{"name":"x","spec_name":"train","cron":"* * * * *","overlap_policy":"whenever"}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status %d, want 400 for an unknown overlap_policy. body=%s", rec.Code, rec.Body)
	}
	for _, p := range []string{"skip", "allow", "queue_one"} {
		rec := post(t, h, "/v1/namespaces/team-ml/schedules", adminKey, "",
			`{"name":"`+p+`","spec_name":"train","cron":"* * * * *","overlap_policy":"`+p+`"}`)
		if rec.Code != http.StatusCreated {
			t.Errorf("overlap_policy %q: status %d, want 201. body=%s", p, rec.Code, rec.Body)
		}
	}
}

func TestCreateScheduleRequiresFields(t *testing.T) {
	h, _ := newScheduleAPI(t)
	for _, body := range []string{
		`{"spec_name":"train","cron":"* * * * *"}`, // no name
		`{"name":"x","cron":"* * * * *"}`,          // no spec_name
		`{"name":"x","spec_name":"train"}`,         // no cron
	} {
		rec := post(t, h, "/v1/namespaces/team-ml/schedules", adminKey, "", body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("body %s: status %d, want 400", body, rec.Code)
		}
	}
}

// TestPreviewAnswersRatherThanFailing pins why an invalid expression is 200 with
// valid:false. This endpoint's job is to ANSWER "is this good", so a bad expression
// is a successful answer -- a 400 would make a UI treat it as a request failure.
func TestPreviewAnswersRatherThanFailing(t *testing.T) {
	h, _ := newScheduleAPI(t)

	rec := get(t, h, "/v1/namespaces/team-ml/schedules/preview?cron=0+2+*+*+*&count=3", adminKey)
	if rec.Code != http.StatusOK {
		t.Fatalf("valid cron: status %d, want 200. body=%s", rec.Code, rec.Body)
	}
	var ok map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &ok)
	if ok["valid"] != true {
		t.Errorf("valid cron reported valid=%v", ok["valid"])
	}
	if fires, _ := ok["next_fires"].([]any); len(fires) != 3 {
		t.Errorf("count=3 returned %d fires", len(fires))
	}

	rec = get(t, h, "/v1/namespaces/team-ml/schedules/preview?cron=nonsense", adminKey)
	if rec.Code != http.StatusOK {
		t.Fatalf("invalid cron: status %d, want 200 with valid:false. body=%s", rec.Code, rec.Body)
	}
	var bad map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &bad)
	if bad["valid"] != false {
		t.Errorf("invalid cron reported valid=%v", bad["valid"])
	}
	if bad["error"] == nil {
		t.Error("invalid cron carried no error explaining why")
	}

	// A missing cron IS a malformed request, unlike a bad one.
	if rec := get(t, h, "/v1/namespaces/team-ml/schedules/preview", adminKey); rec.Code != http.StatusBadRequest {
		t.Errorf("missing cron: status %d, want 400", rec.Code)
	}
}

func TestPreviewCountIsBounded(t *testing.T) {
	h, _ := newScheduleAPI(t)
	// An unbounded count would let one request spend arbitrary CPU walking the grid
	// inside the gateway.
	rec := get(t, h, "/v1/namespaces/team-ml/schedules/preview?cron=*+*+*+*+*&count=9999", adminKey)
	var got map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	fires, _ := got["next_fires"].([]any)
	if len(fires) > maxPreviewFires {
		t.Errorf("count=9999 returned %d fires, want at most %d", len(fires), maxPreviewFires)
	}
}

// TestScheduleWritesRequireWriteScope keeps a dashboard key from creating work. The
// reader role exists precisely so a read-only console cannot mutate anything.
func TestScheduleWritesRequireWriteScope(t *testing.T) {
	h, fs := newScheduleAPI(t)

	rec := post(t, h, "/v1/namespaces/team-ml/schedules", readerKey, "",
		`{"name":"x","spec_name":"train","cron":"* * * * *"}`)
	if rec.Code != http.StatusForbidden {
		t.Errorf("create with a reader key: status %d, want 403. body=%s", rec.Code, rec.Body)
	}
	if len(fs.created) != 0 {
		t.Error("a reader key created a schedule")
	}

	// Reads are allowed for a reader, which is the point of the role.
	if rec := get(t, h, "/v1/namespaces/team-ml/schedules/preview?cron=*+*+*+*+*", readerKey); rec.Code != http.StatusOK {
		t.Errorf("preview with a reader key: status %d, want 200", rec.Code)
	}
}

// TestScheduleRoutesAreCrossNamespaceSafe: a key scoped to one namespace must not
// reach another's schedules, which is the tenancy boundary authn enforces.
func TestScheduleRoutesAreCrossNamespaceSafe(t *testing.T) {
	h, fs := newScheduleAPI(t)
	rec := post(t, h, "/v1/namespaces/other-team/schedules", adminKey, "",
		`{"name":"x","spec_name":"train","cron":"* * * * *"}`)
	if rec.Code != http.StatusForbidden {
		t.Errorf("cross-namespace create: status %d, want 403. body=%s", rec.Code, rec.Body)
	}
	if len(fs.created) != 0 {
		t.Error("a cross-namespace create reached the owner")
	}
}

// TestSchedulesUnavailableWhenUnconfigured covers the rolling-upgrade case: a gateway
// pointed at a workflow-manager that predates schedules must report the feature
// missing rather than refuse to start or return a confusing 500.
func TestSchedulesUnavailableWhenUnconfigured(t *testing.T) {
	db := newRestDB()
	db.addKey(adminKey, "team-ml", "ci", "admin")
	api := New(db, authn.New(db), &fakeWFM{}, fakeRM{}, nil,
		slog.New(slog.NewTextHandler(io.Discard, nil))) // no WithSchedules
	h := api.Routes()

	rec := post(t, h, "/v1/namespaces/team-ml/schedules", adminKey, "",
		`{"name":"x","spec_name":"train","cron":"* * * * *"}`)
	if rec.Code != http.StatusNotImplemented {
		t.Errorf("create with no schedule upstream: status %d, want 501. body=%s", rec.Code, rec.Body)
	}

	req := httptest.NewRequest(http.MethodDelete, "/v1/namespaces/team-ml/schedules/x", nil)
	req.Header.Set("Authorization", "Bearer "+adminKey)
	del := httptest.NewRecorder()
	h.ServeHTTP(del, req)
	if del.Code != http.StatusNotImplemented {
		t.Errorf("delete with no schedule upstream: status %d, want 501", del.Code)
	}
}

// TestEnableRecomputesTheFireTime pins why enable does not restore the stored time. A
// schedule disabled for a week has a next_fire_at deep in the past, and restoring it
// would fire a burst of catch-up nobody asked for by pressing "enable".
func TestEnableRecomputesTheFireTime(t *testing.T) {
	h, fs := newScheduleAPI(t)
	rec := post(t, h, "/v1/namespaces/team-ml/schedules/nightly/disable", adminKey, "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("disable: status %d, want 200. body=%s", rec.Code, rec.Body)
	}
	if len(fs.enabled) != 1 || fs.enabled[0] {
		t.Errorf("owner received enabled=%v, want false", fs.enabled)
	}
}

// TestScheduleErrorsMapFromStatusCodes pins the classification boundary.
//
// A pgconn.PgError does not survive a gRPC hop as a typed error, only as text, so
// the OWNER classifies a duplicate name as AlreadyExists and the gateway maps the
// code. Matching Postgres message text here instead would mean a locale or wording
// change silently turns 409 back into 500 -- which is exactly what happened before
// this was moved.
func TestScheduleErrorsMapFromStatusCodes(t *testing.T) {
	cases := []struct {
		err  error
		want int
	}{
		{status.Error(codes.AlreadyExists, "duplicate"), http.StatusConflict},
		{status.Error(codes.NotFound, "gone"), http.StatusNotFound},
		{status.Error(codes.InvalidArgument, "bad cron"), http.StatusBadRequest},
		// Anything unclassified stays a 500: inventing a 4xx for an unknown failure
		// would tell a client to stop retrying something that might succeed.
		{status.Error(codes.Internal, "boom"), http.StatusInternalServerError},
		{errors.New("plain"), http.StatusInternalServerError},
	}
	for _, c := range cases {
		db := newRestDB()
		db.addKey(adminKey, "team-ml", "ci", "admin")
		// The spec must exist so each request reaches the OWNER, which is where the
		// error under test comes from. Without it every case would be short-circuited
		// by the spec check into a 400 and the mapping would go untested.
		db.addSpec("team-ml", "train")
		api := New(db, authn.New(db), &fakeWFM{}, fakeRM{}, nil,
			slog.New(slog.NewTextHandler(io.Discard, nil))).
			WithSchedules(&fakeSchedules{err: c.err})
		rec := post(t, api.Routes(), "/v1/namespaces/team-ml/schedules", adminKey, "",
			`{"name":"x","spec_name":"train","cron":"* * * * *"}`)
		if rec.Code != c.want {
			t.Errorf("%v -> status %d, want %d. body=%s", c.err, rec.Code, c.want, rec.Body)
		}
	}
}

// TestPreviewHonoursTheTimezone: the same expression previews different instants in
// different zones, and an author checking a London schedule needs London instants.
func TestPreviewHonoursTheTimezone(t *testing.T) {
	h, _ := newScheduleAPI(t)

	rec := get(t, h, "/v1/namespaces/team-ml/schedules/preview?cron=0+2+*+*+*&timezone=Europe/London&count=2", adminKey)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200. body=%s", rec.Code, rec.Body)
	}
	var got map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got["valid"] != true {
		t.Fatalf("valid = %v, body=%s", got["valid"], rec.Body)
	}
	if got["timezone"] != "Europe/London" {
		t.Errorf("timezone = %v, want Europe/London echoed back", got["timezone"])
	}
	fires, _ := got["next_fires"].([]any)
	if len(fires) != 2 {
		t.Fatalf("got %d fires, want 2", len(fires))
	}
	for _, f := range fires {
		str, _ := f.(string)
		if !strings.Contains(str, "T02:00:00") {
			t.Errorf("preview fire %q is not 02:00 local", str)
		}
	}

	// An unknown zone is answered, not errored, for the same reason a bad cron is: this
	// endpoint's job is to say whether the input is usable.
	bad := get(t, h, "/v1/namespaces/team-ml/schedules/preview?cron=0+2+*+*+*&timezone=Mars/Olympus", adminKey)
	if bad.Code != http.StatusOK {
		t.Errorf("unknown zone: status %d, want 200 with valid:false", bad.Code)
	}
	var badBody map[string]any
	_ = json.Unmarshal(bad.Body.Bytes(), &badBody)
	if badBody["valid"] != false || badBody["error"] == nil {
		t.Errorf("unknown zone body = %v, want valid:false with an error", badBody)
	}
}

// A backfill is addressed by its own id for its whole life, so a client that
// followed a backfill link holds a schedule UUID and nothing a person can read.
// The name is rendered where it is known and omitted where it is not, rather
// than every list query paying for a join to carry it.
func TestBackfillPayloadCarriesTheScheduleNameWhenKnown(t *testing.T) {
	b := store.Backfill{ID: "bf-1", ScheduleID: "sch-1", Namespace: "team-ml"}

	with := backfillPayload(b, "nightly-retrain")
	if got := with["schedule_name"]; got != "nightly-retrain" {
		t.Errorf("schedule_name = %v, want nightly-retrain", got)
	}
	// schedule_id stays regardless: the name is a convenience, the id is the
	// identifier every other route is keyed on.
	if got := with["schedule_id"]; got != "sch-1" {
		t.Errorf("schedule_id = %v, want sch-1", got)
	}

	// Absent rather than present-and-empty, so a client can tell "not supplied"
	// from "named the empty string" without a special case.
	for _, p := range []map[string]any{backfillPayload(b), backfillPayload(b, "")} {
		if _, ok := p["schedule_name"]; ok {
			t.Errorf("schedule_name was rendered when no name was known: %v", p["schedule_name"])
		}
	}
}

// TestCreateScheduleRejectsUnknownSpecName is the counterpart to the cron check.
//
// schedules.spec_name has NO foreign key and cannot have one -- workflow_specs is
// versioned and unique only on (namespace, name, version), so there is no
// (namespace, name) key to reference. Without a submit-time check the database
// accepts any string, the create returns 201 with next_fire_at armed, and the
// operator gets a schedule that lists as enabled and healthy, fires on time, and
// creates nothing. That is the same failure mode as a bad cron, so it gets the
// same treatment: rejected here, with the field and the value named.
func TestCreateScheduleRejectsUnknownSpecName(t *testing.T) {
	h, fs := newScheduleAPI(t)

	rec := post(t, h, "/v1/namespaces/team-ml/schedules", adminKey, "",
		`{"name":"nightly","spec_name":"trian","cron":"0 2 * * *"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400. body=%s", rec.Code, rec.Body)
	}

	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["error"] != "unknown_spec_name" {
		t.Errorf("error code = %v, want unknown_spec_name. body=%s", got["error"], rec.Body)
	}
	// The message must quote the VALUE back. Whoever hits this is looking at a
	// string they believed was right, and "unknown workflow" without the name they
	// typed sends them to diff it by eye -- which is the whole reason the cron
	// rejection names its offending token.
	msg, _ := got["message"].(string)
	for _, want := range []string{`"trian"`, "spec_name", "team-ml"} {
		if !strings.Contains(msg, want) {
			t.Errorf("rejection %q does not name %q", msg, want)
		}
	}

	// And nothing was created. A 400 that still reached the owner would leave the
	// broken row behind, which is precisely what this exists to prevent.
	if len(fs.created) != 0 {
		t.Errorf("the owner was asked to create %+v despite the rejection", fs.created)
	}
}

// TestCreateScheduleChecksTheSpecInTheREQUESTNamespace pins that the check is
// namespaced.
//
// A workflow name is unique only within a namespace, so "train exists" is not a
// fact about the fleet. Checking without the namespace would let a schedule in
// team-ops be created for a workflow that only team-ml has -- a schedule that
// passes validation and still resolves nothing at fire time, which is the exact
// failure this is meant to stop, reintroduced one namespace over.
func TestCreateScheduleChecksTheSpecInTheRequestNamespace(t *testing.T) {
	db := newRestDB()
	db.addKey(adminKey, "team-ops", "ci", "admin")
	// The workflow exists -- but somewhere else.
	db.addSpec("team-ml", "train")
	api := New(db, authn.New(db), &fakeWFM{}, fakeRM{}, nil,
		slog.New(slog.NewTextHandler(io.Discard, nil))).WithSchedules(&fakeSchedules{})

	rec := post(t, api.Routes(), "/v1/namespaces/team-ops/schedules", adminKey, "",
		`{"name":"nightly","spec_name":"train","cron":"0 2 * * *"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400. body=%s", rec.Code, rec.Body)
	}
}

// TestCreateScheduleFailsLoudlyWhenTheCatalogCannotBeRead separates "it does not
// exist" from "I could not check".
//
// If the existence read itself fails, the gateway has established NOTHING about
// whether the workflow is there. Reporting that as a 400 would tell the operator
// their workflow does not exist -- a falsehood they would act on, by going to
// re-create a workflow that was fine. It is an outage and must read as one, so
// the client retries instead of "fixing" a correct name.
func TestCreateScheduleFailsLoudlyWhenTheCatalogCannotBeRead(t *testing.T) {
	db := newRestDB()
	db.addKey(adminKey, "team-ml", "ci", "admin")
	api := New(&specReadFailsDB{restDB: db}, authn.New(db), &fakeWFM{}, fakeRM{}, nil,
		slog.New(slog.NewTextHandler(io.Discard, nil))).WithSchedules(&fakeSchedules{})

	rec := post(t, api.Routes(), "/v1/namespaces/team-ml/schedules", adminKey, "",
		`{"name":"nightly","spec_name":"train","cron":"0 2 * * *"}`)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status %d, want 500 -- an unreadable catalog is an outage, not a bad request. body=%s",
			rec.Code, rec.Body)
	}
}

// specReadFailsDB answers every query normally except the existence check, which
// fails. Wrapping rather than reimplementing keeps authentication working, so the
// request under test reaches the handler rather than dying at the door.
type specReadFailsDB struct{ *restDB }

func (d *specReadFailsDB) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if strings.Contains(sql, "FROM workflow_specs") {
		return errRow{errors.New("connection refused")}
	}
	return d.restDB.QueryRow(ctx, sql, args...)
}

// TestScheduleCreationAndSpecDeletionAgreeOnExistence is the two-halves test.
//
// Creation refuses a spec_name that names no workflow; deletion refuses a workflow
// a schedule still names. Those are two ends of one invariant, and if they used
// different notions of "the same name" a workflow could be simultaneously
// unschedulable (creation says it does not exist) and undeletable (deletion says a
// schedule references it) -- a state an operator cannot escape from either end.
//
// Both sides read workflow name and schedule spec_name with the SAME exact,
// unfolded, untrimmed predicate: store.WFMSpecNameExists against workflow_specs,
// and the schedules half of store.WFMCountSpecReferences. This pins that a name
// creation ACCEPTS is exactly a name deletion reports as blocked, and that a name
// creation REJECTS blocks nothing.
func TestScheduleCreationAndSpecDeletionAgreeOnExistence(t *testing.T) {
	h, fs := newScheduleAPI(t) // "train" exists in team-ml

	// Half one: the name that exists is accepted, and the schedule is created.
	rec := post(t, h, "/v1/namespaces/team-ml/schedules", adminKey, "",
		`{"name":"nightly","spec_name":"train","cron":"0 2 * * *"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("creating a schedule for an existing workflow: status %d, want 201. body=%s", rec.Code, rec.Body)
	}
	if len(fs.created) != 1 || fs.created[0].SpecName != "train" {
		t.Fatalf("owner received %+v, want one schedule for train", fs.created)
	}

	// Half two: that same name, now referenced by a schedule, is refused by the
	// delete path with a 409 that NAMES the schedule blocking it. The gateway asks
	// its owner, which classifies the refusal as FailedPrecondition.
	dh, wfm, _ := newSpecDeleteAPI(t)
	wfm.deleteSpecFn = func(ns, name string) (int, error) {
		return 0, status.Errorf(codes.FailedPrecondition,
			"workflow %s/%s is still referenced by 0 run(s) and 1 schedule(s)", ns, name)
	}
	drec := doJSON(t, dh, http.MethodDelete, "/v1/namespaces/team-ml/specs/train", adminKey, "")
	if drec.Code != http.StatusConflict {
		t.Fatalf("deleting a workflow a schedule names: status %d, want 409. body=%s", drec.Code, drec.Body)
	}

	// Half three, the one that makes this more than two assertions side by side:
	// the name creation REJECTED must not be one deletion treats as referenced.
	// A create that was refused leaves nothing behind to block a delete.
	rec = post(t, h, "/v1/namespaces/team-ml/schedules", adminKey, "",
		`{"name":"typo","spec_name":"trian","cron":"0 2 * * *"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("creating a schedule for an absent workflow: status %d, want 400. body=%s", rec.Code, rec.Body)
	}
	if len(fs.created) != 1 {
		t.Errorf("the refused create still reached the owner: %+v", fs.created)
	}
}

func (f *fakeSchedules) UpdateSchedule(ctx context.Context, s store.Schedule, next time.Time) (store.Schedule, error) {
	return f.CreateSchedule(ctx, s, next)
}

func TestUpdateSchedule(t *testing.T) {
	for _, tc := range []struct {
		name, body, key string
		want            int
	}{
		{"valid", `{"name":"nightly","spec_name":"train","cron":"0 4 * * *","enabled":false}`, adminKey, 200},
		{"rename", `{"name":"other","spec_name":"train","cron":"0 4 * * *","enabled":false}`, adminKey, 400},
		{"bad cron", `{"spec_name":"train","cron":"bad","enabled":false}`, adminKey, 400},
		{"missing enabled", `{"spec_name":"train","cron":"0 4 * * *"}`, adminKey, 400},
		{"unknown workflow", `{"spec_name":"missing","cron":"0 4 * * *","enabled":false}`, adminKey, 400},
		{"reader", `{"spec_name":"train","cron":"0 4 * * *","enabled":false}`, readerKey, 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, fs := newScheduleAPI(t)
			req := httptest.NewRequest(http.MethodPut, "/v1/namespaces/team-ml/schedules/nightly", strings.NewReader(tc.body))
			req.Header.Set("Authorization", "Bearer "+tc.key)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
			}
			if tc.want == 200 && (len(fs.created) != 1 || fs.created[0].Name != "nightly" || fs.created[0].Enabled) {
				t.Fatalf("incorrect update: %+v", fs.created)
			}
			if tc.want != 200 && len(fs.created) != 0 {
				t.Fatal("invalid update reached owner")
			}
		})
	}
}
