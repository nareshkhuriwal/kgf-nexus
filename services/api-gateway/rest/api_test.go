// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package rest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/khuriwalgroup/kgf-nexus/internal/store"
	"github.com/khuriwalgroup/kgf-nexus/services/api-gateway/authn"
)

// These tests drive the HTTP surface, because the Idempotency-Key contract is a
// property of the HTTP layer: which STATUS CODE a client sees is the whole point
// (200 replay vs 201 created vs 409 conflict), and asserting it at the store level
// would leave the mapping untested.

// ── doubles ───────────────────────────────────────────────────────────────

// restDB models api_keys and idempotency_keys, the two tables the gateway owns and
// the only ones these tests touch -- plus the EXISTENCE of workflow names, which
// createSchedule now reads directly (ADR-0011 lets the gateway read any table).
type restDB struct {
	mu sync.Mutex
	// keyHash -> principal fields
	keys map[string][4]string
	idem map[string]*idemRow
	// "ns\x00name" for every workflow name that exists. A set rather than a
	// version list because the query under test is EXISTS: a schedule needs the
	// name to be inhabited and has no interest in which versions inhabit it.
	specs map[string]bool
	// runs and nodes back the run read and node listing the retry handler makes
	// before it asks the owner; keyed "ns\x00id" and by run id respectively. A
	// run absent here reads as not found in that namespace.
	runs  map[string]store.RunRow
	nodes map[string][]store.NodeRow
	// runListLimits records the LIMIT of every run listing, in order.
	runListLimits []int
}

// addRun seeds a run and its nodes (node id -> state).
func (d *restDB) addRun(ns, id, state string, nodes map[string]string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.runs[ns+"\x00"+id] = store.RunRow{ID: id, Namespace: ns, State: state, CreatedAt: time.Now()}
	var rows []store.NodeRow
	for n, st := range nodes {
		rows = append(rows, store.NodeRow{NodeID: n, State: st})
	}
	d.nodes[id] = rows
}

type idemRow struct {
	requestHash string
	response    string
}

func newRestDB() *restDB {
	return &restDB{keys: map[string][4]string{}, idem: map[string]*idemRow{}, specs: map[string]bool{},
		runs: map[string]store.RunRow{}, nodes: map[string][]store.NodeRow{}}
}

// addSpec declares that a workflow name exists in a namespace.
//
// Every test that creates a schedule must call this for the name it uses, which is
// the point: the gateway refuses an unknown spec_name, so a test fixture that did
// not declare the workflow would be asserting against a namespace where the
// workflow genuinely is not there.
func (d *restDB) addSpec(ns, name string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.specs[ns+"\x00"+name] = true
}

func (d *restDB) addKey(plaintext, ns, name, role string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.keys[store.HashAPIKey(plaintext)] = [4]string{"key-" + name, ns, name, role}
}

func (d *restDB) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	switch {
	case strings.Contains(sql, "INSERT INTO idempotency_keys"):
		k := args[0].(string) + "\x00" + args[1].(string)
		if _, exists := d.idem[k]; exists {
			return pgconn.NewCommandTag("INSERT 0 0"), nil
		}
		d.idem[k] = &idemRow{requestHash: args[2].(string), response: "null"}
		return pgconn.NewCommandTag("INSERT 0 1"), nil
	case strings.Contains(sql, "UPDATE idempotency_keys"):
		k := args[0].(string) + "\x00" + args[1].(string)
		if row, ok := d.idem[k]; ok {
			row.response = args[2].(string)
		}
		return pgconn.NewCommandTag("UPDATE 1"), nil
	case strings.Contains(sql, "DELETE FROM idempotency_keys"):
		k := args[0].(string) + "\x00" + args[1].(string)
		if row, ok := d.idem[k]; ok && row.response == "null" {
			delete(d.idem, k)
			return pgconn.NewCommandTag("DELETE 1"), nil
		}
		return pgconn.NewCommandTag("DELETE 0"), nil
	}
	return pgconn.NewCommandTag(""), fmt.Errorf("restDB: unhandled Exec: %s", sql)
}

func (d *restDB) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	d.mu.Lock()
	defer d.mu.Unlock()
	switch {
	case strings.Contains(sql, "FROM api_keys"):
		p, ok := d.keys[args[0].(string)]
		if !ok {
			return errRow{pgx.ErrNoRows}
		}
		return valueRow{[]any{p[0], p[1], p[2], p[3]}}
	case strings.Contains(sql, "FROM idempotency_keys"):
		k := args[0].(string) + "\x00" + args[1].(string)
		row, ok := d.idem[k]
		if !ok {
			return errRow{pgx.ErrNoRows}
		}
		return valueRow{[]any{row.requestHash, []byte(row.response)}}
	// The schedule create path's existence check. Matched on the FROM clause
	// rather than on "EXISTS", so it keeps working if the query is reformatted.
	// GatewayGetRun: the run select joined to its spec, scoped by namespace.
	// Checked before the spec lookup below, whose FROM clause it also contains.
	// GatewayTenant: no namespace in these tests has a tenant row.
	case strings.Contains(sql, "FROM tenants WHERE namespace"):
		return errRow{pgx.ErrNoRows}
	// GatewayCountRuns first: it reads the same table with a count.
	case strings.Contains(sql, "SELECT count(*) FROM workflow_runs r"):
		return valueRow{[]any{int64(0)}}
	case strings.Contains(sql, "FROM workflow_runs r"):
		run, ok := d.runs[args[0].(string)+"\x00"+args[1].(string)]
		if !ok {
			return errRow{pgx.ErrNoRows}
		}
		return runScanRow{run}
	case strings.Contains(sql, "FROM workflow_specs"):
		return valueRow{[]any{d.specs[args[0].(string)+"\x00"+args[1].(string)]}}
	}
	return errRow{fmt.Errorf("restDB: unhandled QueryRow: %s", sql)}
}

func (d *restDB) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if strings.Contains(sql, "FROM task_nodes") {
		return &nodeScanRows{rows: d.nodes[args[0].(string)], i: -1}, nil
	}
	// GatewayClusterList: a namespace with no clusters.
	if strings.Contains(sql, "FROM clusters WHERE namespace") {
		return &nodeScanRows{i: -1}, nil
	}
	// GatewayListRuns: an empty page, recording the LIMIT the store asked for
	// (always its last argument) so a test can pin how the handler capped it.
	if strings.Contains(sql, "FROM workflow_runs r") && strings.Contains(sql, "ORDER BY r.created_at DESC LIMIT") {
		d.runListLimits = append(d.runListLimits, args[len(args)-1].(int))
		return &nodeScanRows{i: -1}, nil
	}
	return nil, fmt.Errorf("restDB: unhandled Query: %s", sql)
}

// lastRunListLimit is the LIMIT of the most recent run listing, or 0.
func (d *restDB) lastRunListLimit() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.runListLimits) == 0 {
		return 0
	}
	return d.runListLimits[len(d.runListLimits)-1]
}

// runScanRow scans a store.RunRow in the column order of store's runSelect.
type runScanRow struct{ r store.RunRow }

func (r runScanRow) Scan(dest ...any) error {
	if len(dest) != 14 {
		return fmt.Errorf("run scan wants 14 destinations, got %d", len(dest))
	}
	*dest[0].(*string), *dest[1].(*string), *dest[2].(*string) = r.r.ID, r.r.SpecID, r.r.SpecName
	*dest[3].(*int32), *dest[4].(*string), *dest[5].(*string) = r.r.SpecVersion, r.r.Namespace, r.r.State
	*dest[6].(*int32), *dest[7].(*string), *dest[8].(*[]byte) = r.r.Priority, r.r.TriggerKind, r.r.Params
	*dest[9].(**time.Time), *dest[10].(*time.Time) = r.r.DeadlineAt, r.r.CreatedAt
	*dest[11].(**time.Time), *dest[12].(**time.Time), *dest[13].(*int64) = r.r.StartedAt, r.r.EndedAt, r.r.StateVersion
	return nil
}

// nodeScanRows serves store.GatewayListRunNodes. Only the methods that
// function uses do anything.
type nodeScanRows struct {
	pgx.Rows
	rows []store.NodeRow
	i    int
}

func (n *nodeScanRows) Next() bool { n.i++; return n.i < len(n.rows) }
func (n *nodeScanRows) Err() error { return nil }
func (n *nodeScanRows) Close()     {}
func (n *nodeScanRows) Scan(dest ...any) error {
	row := n.rows[n.i]
	*dest[0].(*string), *dest[1].(*string), *dest[2].(*string) = row.NodeID, row.TaskRef, row.State
	*dest[3].(*string), *dest[4].(*int32), *dest[5].(*int32) = row.TriggerRule, row.MaxAttempts, row.AttemptsUsed
	*dest[6].(**int32), *dest[7].(**string) = row.MapIndex, row.ParentNodeID
	return nil
}

func (d *restDB) Begin(ctx context.Context) (pgx.Tx, error) {
	return nil, fmt.Errorf("restDB: transactions not modelled")
}

type errRow struct{ err error }

func (r errRow) Scan(dest ...any) error { return r.err }

type valueRow struct{ vals []any }

func (r valueRow) Scan(dest ...any) error {
	if len(dest) != len(r.vals) {
		return fmt.Errorf("scan wants %d destinations, row has %d", len(dest), len(r.vals))
	}
	for i, d := range dest {
		switch p := d.(type) {
		case *string:
			*p = r.vals[i].(string)
		case *[]byte:
			*p = r.vals[i].([]byte)
		// The existence check scans a bool; without this the schedule create path
		// fails with "unsupported destination" rather than an answer.
		case *bool:
			*p = r.vals[i].(bool)
		case *int64:
			*p = r.vals[i].(int64)
		default:
			return fmt.Errorf("unsupported destination %T", d)
		}
	}
	return nil
}

// fakeWFM counts CreateRun calls, which is how "the work happened only once" is
// asserted: a replay that reaches the workflow-manager would create a second run.
type fakeWFM struct {
	mu       sync.Mutex
	creates  int
	nextID   int
	createFn func() (store.RunRow, bool, error)
	// deleteSpecFn lets a test choose the owner's answer to a spec delete --
	// success, the referenced refusal, or not-found -- so the handler's mapping of
	// each gRPC code to an HTTP status is pinned without a database.
	deleteSpecFn func(ns, name string) (int, error)
	// specDeletes records what reached the owner, so a test can prove a refusal
	// was produced by the owner rather than short-circuited in the gateway.
	specDeletes []string
	// retries records every retry that reached the owner, and retryFn chooses
	// its answer; nil answers the run-state conflict a non-retryable run gets.
	retries []retryCall
	retryFn func(nodes []string) (store.RunRow, error)
	// lifecycles records every cancel/pause/resume that reached the owner, as
	// "op ns/id", so a test can prove a refused request never got there.
	lifecycles []string
	// specPuts counts specs that reached the owner's store, so a dry run can be
	// proven to have stored nothing.
	specPuts int
}

type retryCall struct {
	ns, id, actor string
	nodes         []string
}

func (f *fakeWFM) DeleteSpec(_ context.Context, ns, name string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.specDeletes = append(f.specDeletes, ns+"/"+name)
	if f.deleteSpecFn != nil {
		return f.deleteSpecFn(ns, name)
	}
	return 3, nil
}

func (f *fakeWFM) PutSpec(ctx context.Context, namespace, name, hash string, canonicalJSON []byte, createdBy string) (store.SpecRow, bool, error) {
	f.mu.Lock()
	f.specPuts++
	f.mu.Unlock()
	return store.SpecRow{
		ID: "spec-1", Namespace: namespace, Name: name, Version: 1, SpecHash: hash,
		CreatedBy: createdBy, CreatedAt: time.Now(),
	}, true, nil
}

func (f *fakeWFM) CreateRun(ctx context.Context, req CreateRunRequest) (store.RunRow, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.creates++
	if f.createFn != nil {
		return f.createFn()
	}
	f.nextID++
	return store.RunRow{
		ID: fmt.Sprintf("run-%d", f.nextID), Namespace: req.Namespace,
		SpecID: "spec-1", SpecName: "train", SpecVersion: 1,
		State: "PENDING", TriggerKind: req.TriggerKind, CreatedAt: time.Now(),
	}, true, nil
}

func (f *fakeWFM) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.creates
}

func (f *fakeWFM) recordLifecycle(op, ns, id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lifecycles = append(f.lifecycles, op+" "+ns+"/"+id)
}

func (f *fakeWFM) CancelRun(ctx context.Context, ns, id string) (store.RunRow, error) {
	f.recordLifecycle("cancel", ns, id)
	return store.RunRow{ID: id, State: "CANCELLING"}, nil
}
func (f *fakeWFM) PauseRun(ctx context.Context, ns, id string) (store.RunRow, error) {
	f.recordLifecycle("pause", ns, id)
	return store.RunRow{ID: id, State: "PAUSING"}, nil
}
func (f *fakeWFM) ResumeRun(ctx context.Context, ns, id string) (store.RunRow, error) {
	f.recordLifecycle("resume", ns, id)
	return store.RunRow{ID: id, State: "RUNNING"}, nil
}
func (f *fakeWFM) RetryRun(ctx context.Context, ns, id string, nodes []string, actor string) (store.RunRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.retries = append(f.retries, retryCall{ns: ns, id: id, nodes: nodes, actor: actor})
	if f.retryFn != nil {
		return f.retryFn(nodes)
	}
	return store.RunRow{}, store.ErrRunStateConflict
}

type fakeRM struct{}

func (fakeRM) DrainWorker(ctx context.Context, ns, id string) error { return nil }
func (fakeRM) Capacity(ctx context.Context, ns string) (CapacityReport, error) {
	return CapacityReport{Namespace: ns, Workers: 2}, nil
}

const adminKey = "nx_admin_key"
const readerKey = "nx_reader_key"

func newTestAPI(t *testing.T) (http.Handler, *restDB, *fakeWFM) {
	t.Helper()
	db := newRestDB()
	db.addKey(adminKey, "team-ml", "ci", "admin")
	db.addKey(readerKey, "team-ml", "dashboard", "reader")
	wfm := &fakeWFM{}
	api := New(db, authn.New(db), wfm, fakeRM{}, nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	return api.Routes(), db, wfm
}

func post(t *testing.T, h http.Handler, path, key, idemKey, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	if idemKey != "" {
		req.Header.Set("Idempotency-Key", idemKey)
	}
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// ── idempotency over HTTP ─────────────────────────────────────────────────

// TestCreateRunIdempotentReplay: the same key with the same body must return the
// CACHED response and must NOT create a second run.
func TestCreateRunIdempotentReplay(t *testing.T) {
	h, _, wfm := newTestAPI(t)
	body := `{"spec_name":"train","params":{"epochs":10}}`

	first := post(t, h, "/v1/namespaces/team-ml/runs", adminKey, "client-key-1", body)
	if first.Code != http.StatusCreated {
		t.Fatalf("first create: status %d, want 201. body=%s", first.Code, first.Body)
	}

	second := post(t, h, "/v1/namespaces/team-ml/runs", adminKey, "client-key-1", body)
	if second.Code != http.StatusOK {
		t.Fatalf("replay: status %d, want 200. body=%s", second.Code, second.Body)
	}
	if second.Header().Get("Idempotency-Replayed") != "true" {
		t.Error("a replayed response was not marked with Idempotency-Replayed")
	}
	if n := wfm.callCount(); n != 1 {
		t.Errorf("CreateRun was called %d times for one idempotency key; the work was done twice", n)
	}

	// The replay must be the SAME run, byte-for-byte: a client treating the retry as
	// the original call needs the original answer.
	var a, b map[string]any
	if err := json.Unmarshal(first.Body.Bytes(), &a); err != nil {
		t.Fatalf("decode first: %v", err)
	}
	if err := json.Unmarshal(second.Body.Bytes(), &b); err != nil {
		t.Fatalf("decode replay: %v", err)
	}
	if a["id"] != b["id"] {
		t.Errorf("replay returned run %v, want the original %v", b["id"], a["id"])
	}
}

// TestCreateRunIdempotencyConflict is the 409 the task calls out: the same key with a
// DIFFERENT body catches a client bug instead of silently doing the wrong thing.
func TestCreateRunIdempotencyConflict(t *testing.T) {
	h, _, wfm := newTestAPI(t)

	first := post(t, h, "/v1/namespaces/team-ml/runs", adminKey, "client-key-2",
		`{"spec_name":"train","params":{"epochs":10}}`)
	if first.Code != http.StatusCreated {
		t.Fatalf("first create: status %d, want 201. body=%s", first.Code, first.Body)
	}

	// Same key, different params — a retry loop whose body changed underneath it.
	conflict := post(t, h, "/v1/namespaces/team-ml/runs", adminKey, "client-key-2",
		`{"spec_name":"train","params":{"epochs":500}}`)
	if conflict.Code != http.StatusConflict {
		t.Fatalf("same key + different body: status %d, want 409. body=%s", conflict.Code, conflict.Body)
	}
	if !strings.Contains(conflict.Body.String(), "idempotency_key_reused") {
		t.Errorf("the 409 does not name the cause: %s", conflict.Body)
	}
	if n := wfm.callCount(); n != 1 {
		t.Errorf("CreateRun was called %d times; a conflicting request must not create anything", n)
	}
}

// TestCreateRunIdempotencyIgnoresFormatting: a client that re-serializes its request
// with different key order or whitespace is NOT making a different request, and
// 409-ing it would be a false positive that breaks well-behaved clients.
func TestCreateRunIdempotencyIgnoresFormatting(t *testing.T) {
	h, _, wfm := newTestAPI(t)

	if rec := post(t, h, "/v1/namespaces/team-ml/runs", adminKey, "client-key-3",
		`{"spec_name":"train","params":{"epochs":10}}`); rec.Code != http.StatusCreated {
		t.Fatalf("first create: status %d, want 201", rec.Code)
	}
	// Same content, reordered keys and extra whitespace.
	replay := post(t, h, "/v1/namespaces/team-ml/runs", adminKey, "client-key-3",
		"{\n  \"params\" : { \"epochs\" : 10 },\n  \"spec_name\" : \"train\"\n}")
	if replay.Code != http.StatusOK {
		t.Errorf("a semantically identical body returned %d, want 200 replay. body=%s", replay.Code, replay.Body)
	}
	if n := wfm.callCount(); n != 1 {
		t.Errorf("CreateRun called %d times, want 1", n)
	}
}

// TestCreateRunWithoutIdempotencyKey: the header is optional, and two unkeyed
// requests are two genuine runs.
func TestCreateRunWithoutIdempotencyKey(t *testing.T) {
	h, _, wfm := newTestAPI(t)
	body := `{"spec_name":"train"}`

	for i := 0; i < 2; i++ {
		if rec := post(t, h, "/v1/namespaces/team-ml/runs", adminKey, "", body); rec.Code != http.StatusCreated {
			t.Fatalf("create %d: status %d, want 201. body=%s", i, rec.Code, rec.Body)
		}
	}
	if n := wfm.callCount(); n != 2 {
		t.Errorf("CreateRun called %d times, want 2: unkeyed requests must not dedup", n)
	}
}

// TestFailedCreateReleasesIdempotencyClaim: without release, a transient failure would
// poison the key permanently and the client's correct retry could never succeed.
func TestFailedCreateReleasesIdempotencyClaim(t *testing.T) {
	h, db, wfm := newTestAPI(t)

	wfm.createFn = func() (store.RunRow, bool, error) {
		return store.RunRow{}, false, errors.New("database briefly unavailable")
	}
	if rec := post(t, h, "/v1/namespaces/team-ml/runs", adminKey, "client-key-4",
		`{"spec_name":"train"}`); rec.Code != http.StatusInternalServerError {
		t.Fatalf("a failed creation returned %d, want 500", rec.Code)
	}

	db.mu.Lock()
	_, stillClaimed := db.idem["team-ml\x00client-key-4"]
	db.mu.Unlock()
	if stillClaimed {
		t.Error("the idempotency claim survived a failed creation; the key is poisoned and the client can never retry")
	}

	// The retry, once the dependency recovers, must succeed.
	wfm.createFn = nil
	if rec := post(t, h, "/v1/namespaces/team-ml/runs", adminKey, "client-key-4",
		`{"spec_name":"train"}`); rec.Code != http.StatusCreated {
		t.Errorf("the retry after recovery returned %d, want 201. body=%s", rec.Code, rec.Body)
	}
}

func TestCreateRunRequiresSpec(t *testing.T) {
	h, _, _ := newTestAPI(t)
	rec := post(t, h, "/v1/namespaces/team-ml/runs", adminKey, "", `{}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("a run with neither spec_id nor spec_name returned %d, want 400", rec.Code)
	}
}

func TestCreateRunRejectsMalformedJSON(t *testing.T) {
	h, _, _ := newTestAPI(t)
	rec := post(t, h, "/v1/namespaces/team-ml/runs", adminKey, "", `{"spec_name":`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("malformed JSON returned %d, want 400", rec.Code)
	}
}

// ── spec submission ───────────────────────────────────────────────────────

func TestSubmitSpecAcceptsJSONAndYAML(t *testing.T) {
	h, _, _ := newTestAPI(t)

	jsonSpec := `{"api_version":"nexus.dev/v1","namespace":"team-ml","name":"train",
	  "tasks":[{"id":"a","task_ref":"etl.extract:v1"}]}`
	rec := post(t, h, "/v1/namespaces/team-ml/specs", adminKey, "", jsonSpec)
	if rec.Code != http.StatusCreated {
		t.Fatalf("JSON spec: status %d, want 201. body=%s", rec.Code, rec.Body)
	}

	yamlSpec := "api_version: nexus.dev/v1\nnamespace: team-ml\nname: train\ntasks:\n  - id: a\n    task_ref: etl.extract:v1\n"
	req := httptest.NewRequest(http.MethodPost, "/v1/namespaces/team-ml/specs", strings.NewReader(yamlSpec))
	req.Header.Set("Authorization", "Bearer "+adminKey)
	req.Header.Set("Content-Type", "application/yaml")
	yrec := httptest.NewRecorder()
	h.ServeHTTP(yrec, req)
	if yrec.Code != http.StatusCreated {
		t.Fatalf("YAML spec: status %d, want 201. body=%s", yrec.Code, yrec.Body)
	}

	// The two surfaces must produce the SAME content address: ADR-0004's "neither
	// surface is second-class", visible at the API boundary.
	var a, b map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &a)
	_ = json.Unmarshal(yrec.Body.Bytes(), &b)
	if a["spec_hash"] != b["spec_hash"] {
		t.Errorf("JSON hash %v != YAML hash %v for the same workflow", a["spec_hash"], b["spec_hash"])
	}
}

// TestSubmitSpecDryRunValidatesWithoutStoring pins CONTRACT-FINDINGS F118: a dry
// run answers with the content address a real submit would register, stores
// nothing, and still rejects an invalid spec exactly as a real submit does.
func TestSubmitSpecDryRunValidatesWithoutStoring(t *testing.T) {
	h, _, wfm := newTestAPI(t)
	good := `{"api_version":"nexus.dev/v1","namespace":"team-ml","name":"train",
	  "tasks":[{"id":"a","task_ref":"etl.extract:v1"}]}`

	rec := post(t, h, "/v1/namespaces/team-ml/specs?dry_run=true", adminKey, "", good)
	if rec.Code != http.StatusOK {
		t.Fatalf("dry run: status %d, want 200. body=%s", rec.Code, rec.Body)
	}
	var dry map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &dry)
	if dry["valid"] != true || dry["dry_run"] != true || dry["spec_hash"] == "" {
		t.Errorf("dry run answer is incomplete: %s", rec.Body)
	}
	if _, has := dry["version"]; has {
		t.Errorf("a dry run must not report a version it did not create: %s", rec.Body)
	}
	if wfm.specPuts != 0 {
		t.Fatalf("a dry run stored %d spec(s)", wfm.specPuts)
	}

	// The hash is the one a real submit then registers.
	reg := post(t, h, "/v1/namespaces/team-ml/specs", adminKey, "", good)
	var stored map[string]any
	_ = json.Unmarshal(reg.Body.Bytes(), &stored)
	if stored["spec_hash"] != dry["spec_hash"] || wfm.specPuts != 1 {
		t.Errorf("dry-run hash %v, stored hash %v, puts %d", dry["spec_hash"], stored["spec_hash"], wfm.specPuts)
	}

	bad := post(t, h, "/v1/namespaces/team-ml/specs?dry_run=true", adminKey, "",
		`{"api_version":"nexus.dev/v1","namespace":"team-ml","name":"train","tasks":[]}`)
	if bad.Code != http.StatusUnprocessableEntity {
		t.Errorf("an invalid spec must fail a dry run with 422, got %d", bad.Code)
	}
	mismatch := post(t, h, "/v1/namespaces/team-ml/specs?dry_run=true", adminKey, "",
		strings.Replace(good, `"namespace":"team-ml"`, `"namespace":"other"`, 1))
	if mismatch.Code != http.StatusUnprocessableEntity {
		t.Errorf("a namespace mismatch must fail a dry run too, got %d", mismatch.Code)
	}
}

// TestSubmitInvalidSpecReturns422WithProblems: 422 rather than 400 because the request
// was well-formed and the DOCUMENT is unacceptable, and the problems list is what makes
// the error actionable rather than just a rejection.
func TestSubmitInvalidSpecReturns422WithProblems(t *testing.T) {
	h, _, _ := newTestAPI(t)
	rec := post(t, h, "/v1/namespaces/team-ml/specs", adminKey, "",
		`{"api_version":"nexus.dev/v1","namespace":"team-ml","name":"train","tasks":[]}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status %d, want 422. body=%s", rec.Code, rec.Body)
	}
	var body struct {
		Error    string `json:"error"`
		Problems []struct {
			Path    string `json:"path"`
			Message string `json:"message"`
			Stage   string `json:"stage"`
		} `json:"problems"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Error != "spec_invalid" || len(body.Problems) == 0 {
		t.Errorf("the rejection carries no actionable problems: %s", rec.Body)
	}
}

// TestSubmitSpecRejectsNamespaceMismatch: silently preferring one would let a caller
// authenticated for namespace A store a spec claiming namespace B.
func TestSubmitSpecRejectsNamespaceMismatch(t *testing.T) {
	h, _, _ := newTestAPI(t)
	rec := post(t, h, "/v1/namespaces/team-ml/specs", adminKey, "",
		`{"api_version":"nexus.dev/v1","namespace":"other-team","name":"train",
		  "tasks":[{"id":"a","task_ref":"x"}]}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status %d, want 422", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "namespace_mismatch") {
		t.Errorf("the rejection does not name the cause: %s", rec.Body)
	}
}

// ── authn / authz over HTTP ───────────────────────────────────────────────

func TestUnauthenticatedRequestsRejected(t *testing.T) {
	h, _, _ := newTestAPI(t)

	if rec := post(t, h, "/v1/namespaces/team-ml/runs", "", "", `{"spec_name":"t"}`); rec.Code != http.StatusUnauthorized {
		t.Errorf("no credentials: status %d, want 401", rec.Code)
	}
	if rec := post(t, h, "/v1/namespaces/team-ml/runs", "nx_not_a_real_key", "", `{"spec_name":"t"}`); rec.Code != http.StatusUnauthorized {
		t.Errorf("a bad key: status %d, want 401", rec.Code)
	}
}

// TestUnauthorizedResponsesLeakNothing: distinguishing "no such key" from "revoked"
// would make this endpoint an oracle for probing valid credentials.
func TestUnauthorizedResponsesLeakNothing(t *testing.T) {
	h, _, _ := newTestAPI(t)
	rec := post(t, h, "/v1/namespaces/team-ml/runs", "nx_wrong", "", `{"spec_name":"t"}`)
	body := rec.Body.String()
	if strings.Contains(body, "nx_wrong") {
		t.Error("the response echoed the presented key material back to the caller")
	}
	for _, leak := range []string{"revoked", "no such", "not found", "hash"} {
		if strings.Contains(strings.ToLower(body), leak) {
			t.Errorf("the 401 distinguishes the failure mode (%q), making it an oracle: %s", leak, body)
		}
	}
}

// TestReaderCannotMutate: a read-only credential must not be able to create runs or
// submit specs.
func TestReaderCannotMutate(t *testing.T) {
	h, _, wfm := newTestAPI(t)

	if rec := post(t, h, "/v1/namespaces/team-ml/runs", readerKey, "", `{"spec_name":"t"}`); rec.Code != http.StatusForbidden {
		t.Errorf("reader creating a run: status %d, want 403", rec.Code)
	}
	if rec := post(t, h, "/v1/namespaces/team-ml/specs", readerKey, "", `{}`); rec.Code != http.StatusForbidden {
		t.Errorf("reader submitting a spec: status %d, want 403", rec.Code)
	}
	if n := wfm.callCount(); n != 0 {
		t.Errorf("a forbidden request reached the workflow-manager %d times", n)
	}
}

// TestCrossNamespaceAccessForbidden: namespaces are the tenancy boundary, and a
// boundary that is not checked on every request is not a boundary.
func TestCrossNamespaceAccessForbidden(t *testing.T) {
	h, _, _ := newTestAPI(t)
	rec := post(t, h, "/v1/namespaces/other-team/runs", adminKey, "", `{"spec_name":"t"}`)
	if rec.Code != http.StatusForbidden {
		t.Errorf("a team-ml key acting on other-team: status %d, want 403", rec.Code)
	}
}

// TestAdminOnlyDrain: draining a worker is destructive to in-flight capacity planning,
// so it requires admin rather than any writer.
func TestAdminOnlyDrain(t *testing.T) {
	h, _, _ := newTestAPI(t)
	rec := post(t, h, "/v1/namespaces/team-ml/workers/w1/drain", readerKey, "", "")
	if rec.Code != http.StatusForbidden {
		t.Errorf("reader draining a worker: status %d, want 403", rec.Code)
	}
}

// ── lifecycle status mapping ──────────────────────────────────────────────

// TestLifecycleEndpointsMapStates checks that pause reports PAUSING rather than
// PAUSED: the run has not finished draining, and reporting PAUSED early would tell an
// operator that in-flight work had stopped when it has not.
func TestLifecycleEndpointsMapStates(t *testing.T) {
	h, db, _ := newTestAPI(t)
	db.addRun("team-ml", "run-1", "RUNNING", map[string]string{"a": "RUNNING"})
	cases := []struct{ path, wantState string }{
		{"/v1/namespaces/team-ml/runs/run-1/pause", "PAUSING"},
		{"/v1/namespaces/team-ml/runs/run-1/cancel", "CANCELLING"},
		{"/v1/namespaces/team-ml/runs/run-1/resume", "RUNNING"},
	}
	for _, c := range cases {
		rec := post(t, h, c.path, adminKey, "", "")
		if rec.Code != http.StatusOK {
			t.Errorf("%s: status %d, want 200. body=%s", c.path, rec.Code, rec.Body)
			continue
		}
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Errorf("%s: decode: %v", c.path, err)
			continue
		}
		if body["state"] != c.wantState {
			t.Errorf("%s: state %v, want %s", c.path, body["state"], c.wantState)
		}
	}
}

// TestLifecycleConflictIs409: an operator who retries a run that is not retryable must
// be told it did not apply, not given a silent success.
func TestLifecycleConflictIs409(t *testing.T) {
	h, db, _ := newTestAPI(t)
	db.addRun("team-ml", "run-1", "SUCCEEDED", map[string]string{"a": "SUCCEEDED"})
	rec := post(t, h, "/v1/namespaces/team-ml/runs/run-1/retry", adminKey, "", "")
	if rec.Code != http.StatusConflict {
		t.Errorf("retrying a non-retryable run: status %d, want 409. body=%s", rec.Code, rec.Body)
	}
}

// ── schema endpoint ───────────────────────────────────────────────────────

// TestServeSchema: publishing the grammar the gateway actually enforces, so authors
// validate locally against the same rules rather than a documented copy.
func TestServeSchema(t *testing.T) {
	h, _, _ := newTestAPI(t)
	req := httptest.NewRequest(http.MethodGet, "/v1/schemas/workflow-spec-v1.json", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/schema+json" {
		t.Errorf("Content-Type = %q, want application/schema+json", ct)
	}
	var doc map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("the served schema is not valid JSON: %v", err)
	}
	if doc["$schema"] != "https://json-schema.org/draft/2020-12/schema" {
		t.Errorf("$schema = %v, want draft 2020-12", doc["$schema"])
	}
}

// TestBodySizeLimit: bounded by MaxBytesReader rather than a Content-Length check,
// which a client controls and can lie about.
func TestBodySizeLimit(t *testing.T) {
	h, _, _ := newTestAPI(t)
	huge := strings.Repeat("x", maxRunBytes+1024)
	rec := post(t, h, "/v1/namespaces/team-ml/runs", adminKey, "", `{"pad":"`+huge+`"}`)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("an oversized body returned %d, want 413", rec.Code)
	}
}

// fakeFleet is a fixed upstream report, so the handler's behaviour is pinned
// without dialling anything.
type fakeFleet struct{ ups []UpstreamHealth }

func (f fakeFleet) UpstreamHealth() []UpstreamHealth { return f.ups }

// A fleet where every configured upstream is connected is "ok".
func TestFleetHealthReportsOKWhenEverythingIsReady(t *testing.T) {
	rec := get(t, buildFleetAPI(t, fakeFleet{ups: []UpstreamHealth{
		{Name: "scheduler", Addr: "scheduler:9090", State: "READY", Configured: true},
		{Name: "dag-engine", Addr: "dag:9090", State: "READY", Configured: true},
	}}), "/v1/health/fleet", adminKey)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var body struct {
		Status    string `json:"status"`
		Upstreams []struct {
			Name, State string
		} `json:"upstreams"`
		Measures string `json:"measures"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Status != "ok" {
		t.Errorf("status = %q, want ok", body.Status)
	}
	if len(body.Upstreams) != 2 {
		t.Errorf("reported %d upstreams, want 2", len(body.Upstreams))
	}
	// The payload must say what it measured, or a client will treat connectivity
	// as readiness and call a failing service healthy.
	if !strings.Contains(body.Measures, "connectivity") {
		t.Errorf("payload does not state what it measures: %q", body.Measures)
	}
}

// A failed upstream degrades the fleet; an IDLE one does NOT, because idle means
// "not dialled yet", which is normal before first use rather than a fault.
func TestFleetHealthDistinguishesFailureFromIdle(t *testing.T) {
	degraded := get(t, buildFleetAPI(t, fakeFleet{ups: []UpstreamHealth{
		{Name: "scheduler", State: "TRANSIENT_FAILURE", Configured: true},
	}}), "/v1/health/fleet", adminKey)
	if got := statusField(t, degraded); got != "degraded" {
		t.Errorf("a failed upstream gave status %q, want degraded", got)
	}

	idle := get(t, buildFleetAPI(t, fakeFleet{ups: []UpstreamHealth{
		{Name: "scheduler", State: "IDLE", Configured: true},
	}}), "/v1/health/fleet", adminKey)
	if got := statusField(t, idle); got != "ok" {
		t.Errorf("an IDLE upstream gave status %q, want ok (idle is not a fault)", got)
	}
}

// An upstream nobody configured is not a fault either: reporting it as down
// sends an operator chasing a service this deployment does not run.
func TestFleetHealthIgnoresUnconfiguredUpstreams(t *testing.T) {
	rec := get(t, buildFleetAPI(t, fakeFleet{ups: []UpstreamHealth{
		{Name: "dag-engine", State: "UNCONFIGURED", Configured: false},
	}}), "/v1/health/fleet", adminKey)
	if got := statusField(t, rec); got != "ok" {
		t.Errorf("an unconfigured upstream gave status %q, want ok", got)
	}
}

// A gateway assembled without a reporter answers 501, not 500: the feature is
// absent, not broken.
func TestFleetHealthIsNotImplementedWhenUnwired(t *testing.T) {
	h, _, _ := newTestAPI(t)
	rec := get(t, h, "/v1/health/fleet", adminKey)
	if rec.Code != http.StatusNotImplemented {
		t.Errorf("status = %d, want 501 when no reporter is wired", rec.Code)
	}
}

func statusField(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return body.Status
}

// buildFleetAPI is newTestAPI plus a wired health reporter.
func buildFleetAPI(t *testing.T, f FleetHealth) http.Handler {
	t.Helper()
	db := newRestDB()
	db.addKey(adminKey, "team-ml", "ci", "admin")
	api := New(db, authn.New(db), &fakeWFM{}, fakeRM{}, nil,
		slog.New(slog.NewTextHandler(io.Discard, nil))).WithFleetHealth(f)
	return api.Routes()
}

// A malformed spec_id is the CALLER's error. Letting it reach Postgres and
// surface as a 500 tells an operator the server broke when the request was
// simply unusable, sending them to read gateway logs for a fault that is not
// there. A well-formed id naming no spec is a different answer (404).
func TestCreateRunRejectsAMalformedSpecID(t *testing.T) {
	h, _, _ := newTestAPI(t)

	rec := post(t, h, "/v1/namespaces/team-ml/runs", adminKey, "",
		`{"spec_id":"not-a-uuid","params":{}}`)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for a malformed spec_id", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "invalid_spec_id") {
		t.Errorf("body does not name the offending field: %s", rec.Body.String())
	}
}

// Creating by NAME must still work, and must not be caught by the uuid check.
func TestCreateRunByNameIsNotTreatedAsAMalformedID(t *testing.T) {
	h, _, _ := newTestAPI(t)

	rec := post(t, h, "/v1/namespaces/team-ml/runs", adminKey, "",
		`{"spec_name":"nightly","params":{}}`)

	if rec.Code == http.StatusBadRequest && strings.Contains(rec.Body.String(), "invalid_spec_id") {
		t.Errorf("a by-name creation was rejected as a malformed uuid: %s", rec.Body.String())
	}
}

// ── worker drill-down authorisation ───────────────────────────────────────

// TestWorkerDetailIsAReadNotAnAdminAction pins the permission choice on the new
// drill-down: a reader must be able to open it. (`get` is the shared helper from
// schedules_test.go.)
//
// The pairing is the assertion. "What is this worker running" is the question the
// fleet table raises and cannot answer, and it exposes nothing /workers and
// /runs/{id} do not already expose between them — so gating it on admin would
// lock out exactly the dashboard-key operators who look at the fleet during an
// incident. The drain beside it stays admin (TestAdminOnlyDrain), because that
// one reshapes the fleet.
//
// The reader's request is asserted as "not 403" rather than 200 deliberately:
// restDB models api_keys and idempotency_keys only, so the read reaches an
// unmodelled table and 500s. That is the authorisation boundary under test here;
// the SQL itself is covered against real Postgres in the store integration suite,
// which is the only place it can be.
func TestWorkerDetailIsAReadNotAnAdminAction(t *testing.T) {
	h, _, _ := newTestAPI(t)

	rec := get(t, h, "/v1/namespaces/team-ml/workers/w1", readerKey)
	if rec.Code == http.StatusForbidden {
		t.Errorf("a reader opening a worker's detail: status 403, want the route to admit a read")
	}
	if rec.Code == http.StatusNotFound {
		t.Errorf("GET /workers/{id}: 404 — the route is not registered")
	}

	// Same route, another tenant's namespace: the boundary still holds.
	if cross := get(t, h, "/v1/namespaces/other-team/workers/w1", adminKey); cross.Code != http.StatusForbidden {
		t.Errorf("a team-ml key reading other-team's worker: status %d, want 403", cross.Code)
	}
}

// TestLifecycleOfAnotherNamespacesRunIsNotFound pins F101: cancel, pause and
// resume used to reach the owner -- which acts on a run id alone -- before
// anything tied the run to the caller's namespace, and only the read-back
// afterwards was scoped. Now the run is looked up in the path's namespace
// first, and a run that lives elsewhere is a 404 that changes nothing.
func TestLifecycleOfAnotherNamespacesRunIsNotFound(t *testing.T) {
	for _, op := range []string{"cancel", "pause", "resume", "retry"} {
		t.Run(op, func(t *testing.T) {
			h, db, wfm := newTestAPI(t)
			// The run exists -- in a namespace the caller has no key for.
			db.addRun("other-team", "run-1", "FAILED", map[string]string{"a": "FAILED"})
			wfm.retryFn = okRetry

			rec := post(t, h, "/v1/namespaces/team-ml/runs/run-1/"+op, adminKey, "", "")
			if rec.Code != http.StatusNotFound {
				t.Fatalf("%s of another namespace's run: status %d, want 404. body=%s", op, rec.Code, rec.Body)
			}
			if len(wfm.lifecycles) != 0 || len(wfm.retries) != 0 {
				t.Errorf("%s reached the owner before the namespace check: %v %v", op, wfm.lifecycles, wfm.retries)
			}
		})
	}
}

// And a run in the caller's own namespace still reaches the owner, with the
// path's namespace and id.
func TestLifecycleOfOwnRunReachesTheOwner(t *testing.T) {
	h, db, wfm := newTestAPI(t)
	db.addRun("team-ml", "run-1", "RUNNING", map[string]string{"a": "RUNNING"})
	for _, op := range []string{"pause", "resume", "cancel"} {
		if rec := post(t, h, "/v1/namespaces/team-ml/runs/run-1/"+op, adminKey, "", ""); rec.Code != http.StatusOK {
			t.Fatalf("%s: status %d. body=%s", op, rec.Code, rec.Body)
		}
	}
	want := []string{"pause team-ml/run-1", "resume team-ml/run-1", "cancel team-ml/run-1"}
	if strings.Join(wfm.lifecycles, ",") != strings.Join(want, ",") {
		t.Errorf("owner saw %v, want %v", wfm.lifecycles, want)
	}
}
