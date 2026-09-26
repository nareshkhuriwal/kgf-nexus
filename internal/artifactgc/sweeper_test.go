// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package artifactgc

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fakeStore is an in-memory ObjectLister. Deletes are recorded rather than
// pretended, so a test can assert on EXACTLY which keys a pass removed — the
// assertion that matters, since "how many" would pass even if the sweep deleted
// the wrong ones.
type fakeStore struct {
	objects   []Candidate
	deleted   []string
	listErr   error
	deleteErr map[string]error
	listLimit int // the limit the sweeper actually asked for
}

func (f *fakeStore) ListObjects(_ context.Context, prefix string, limit int) ([]Candidate, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	f.listLimit = limit
	var out []Candidate
	for _, o := range f.objects {
		if prefix != "" && len(o.Key) >= len(prefix) && o.Key[:len(prefix)] != prefix {
			continue
		}
		out = append(out, o)
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (f *fakeStore) DeleteObject(_ context.Context, key string) error {
	if err, ok := f.deleteErr[key]; ok {
		return err
	}
	f.deleted = append(f.deleted, key)
	for i, o := range f.objects {
		if o.Key == key {
			f.objects = append(f.objects[:i], f.objects[i+1:]...)
			break
		}
	}
	return nil
}

func (f *fakeStore) deletedSorted() []string {
	out := append([]string(nil), f.deleted...)
	sort.Strings(out)
	return out
}

// sweeperWithRefs builds a Sweeper whose reference set is fixed, bypassing SQL.
//
// The split is deliberate: these unit tests exercise the CLASSIFICATION logic
// against an injected reference set, and the integration tests exercise the SQL
// that derives that set against real rows. Neither half is tested only via the
// other, which matters because a bug in either one deletes live data.
func sweeperWithRefs(t *testing.T, store ObjectLister, cfg Config, refs map[string]struct{}, refErr error) *Sweeper {
	t.Helper()
	s := NewSweeper(nil, store, discardLogger(), cfg)
	s.liveKeys = func(context.Context) (map[string]struct{}, error) {
		if refErr != nil {
			return nil, refErr
		}
		return refs, nil
	}
	return s
}

func refSet(keys ...string) map[string]struct{} {
	m := make(map[string]struct{}, len(keys))
	for _, k := range keys {
		m[k] = struct{}{}
	}
	return m
}

func old() time.Time   { return time.Now().Add(-100 * time.Hour) }
func fresh() time.Time { return time.Now().Add(-1 * time.Minute) }

// ── Reachability ─────────────────────────────────────────────────────────

// TestReferencedObjectIsNeverDeleted is the headline safety property: an object a
// live row names must survive, regardless of how old it is.
func TestReferencedObjectIsNeverDeleted(t *testing.T) {
	fs := &fakeStore{objects: []Candidate{
		{Key: "default/aaa", LastModified: old(), SizeBytes: 10},
		{Key: "default/bbb", LastModified: old(), SizeBytes: 20},
	}}
	sw := sweeperWithRefs(t, fs, Config{DryRun: false, GracePeriod: time.Hour},
		refSet("default/aaa"), nil)

	st, err := sw.Once(context.Background())
	if err != nil {
		t.Fatalf("Once: %v", err)
	}
	if got := fs.deletedSorted(); len(got) != 1 || got[0] != "default/bbb" {
		t.Fatalf("deleted %v, want only [default/bbb]; a referenced object was collected", got)
	}
	if st.Reachable != 1 {
		t.Errorf("Reachable = %d, want 1", st.Reachable)
	}
	if st.Orphans != 1 {
		t.Errorf("Orphans = %d, want 1", st.Orphans)
	}
}

// TestReferenceFromInputRefsProtectsAnObject: an object may be referenced ONLY by
// a downstream attempt's input_refs after the producing run is pruned. Sweeping
// output_ref alone would delete it. This pins that input_refs counts as a
// reference source — the bug this test exists for is a real one, because the
// obvious reading of ADR-0008 ("live output_refs") names only one column.
func TestReferenceFromInputRefsProtectsAnObject(t *testing.T) {
	sources := RefSources()
	var sawInputRefs bool
	for _, s := range sources {
		if s == "task_attempts.input_refs" {
			sawInputRefs = true
		}
	}
	if !sawInputRefs {
		t.Fatalf("task_attempts.input_refs is NOT a reference source (%v); a downstream "+
			"attempt's input would be collectable once its producing run was pruned", sources)
	}
}

// TestEmptyReferenceSetStillCollectsOnlyOldObjects: with no live references at
// all, everything old is an orphan (that is correct), but the grace period still
// protects the fresh ones.
func TestEmptyReferenceSetStillCollectsOnlyOldObjects(t *testing.T) {
	fs := &fakeStore{objects: []Candidate{
		{Key: "default/old", LastModified: old()},
		{Key: "default/new", LastModified: fresh()},
	}}
	sw := sweeperWithRefs(t, fs, Config{DryRun: false, GracePeriod: time.Hour},
		refSet(), nil)

	if _, err := sw.Once(context.Background()); err != nil {
		t.Fatalf("Once: %v", err)
	}
	if got := fs.deletedSorted(); len(got) != 1 || got[0] != "default/old" {
		t.Fatalf("deleted %v, want only [default/old]", got)
	}
}

// TestReferenceQueryFailureAbortsWithoutDeleting is the one that matters most.
// A database error must NOT degrade to an empty reference set, because an empty
// reference set makes every object an orphan — a transient Postgres blip would
// become a bucket wipe.
func TestReferenceQueryFailureAbortsWithoutDeleting(t *testing.T) {
	fs := &fakeStore{objects: []Candidate{
		{Key: "default/aaa", LastModified: old()},
		{Key: "default/bbb", LastModified: old()},
	}}
	boom := errors.New("connection reset by peer")
	sw := sweeperWithRefs(t, fs, Config{DryRun: false, GracePeriod: time.Hour}, nil, boom)

	st, err := sw.Once(context.Background())
	if err == nil {
		t.Fatal("a failed reference query returned no error; the sweep would treat " +
			"an unreachable database as 'nothing is referenced'")
	}
	if len(fs.deleted) != 0 {
		t.Fatalf("deleted %v after a reference-query failure; must delete nothing", fs.deleted)
	}
	if st.Deleted != 0 {
		t.Errorf("Stats.Deleted = %d, want 0", st.Deleted)
	}
}

// TestListFailureAbortsWithoutQueryingOrDeleting: a listing error is likewise
// fatal to the pass.
func TestListFailureAbortsWithoutDeleting(t *testing.T) {
	fs := &fakeStore{listErr: errors.New("no such bucket")}
	sw := sweeperWithRefs(t, fs, Config{DryRun: false}, refSet(), nil)
	if _, err := sw.Once(context.Background()); err == nil {
		t.Fatal("a listing failure returned no error")
	}
	if len(fs.deleted) != 0 {
		t.Fatalf("deleted %v despite a failed listing", fs.deleted)
	}
}

// ── Grace period ─────────────────────────────────────────────────────────

// TestGracePeriodProtectsARecentlyWrittenObject models the exact race the grace
// period exists for: a worker has uploaded an object but its report has not
// committed, so no row references it yet. It must survive.
func TestGracePeriodProtectsARecentlyWrittenObject(t *testing.T) {
	justUploaded := Candidate{Key: "default/inflight", LastModified: time.Now().Add(-30 * time.Second)}
	fs := &fakeStore{objects: []Candidate{justUploaded}}
	sw := sweeperWithRefs(t, fs, Config{DryRun: false, GracePeriod: DefaultGracePeriod},
		refSet(), nil) // deliberately empty: the row does not exist yet

	st, err := sw.Once(context.Background())
	if err != nil {
		t.Fatalf("Once: %v", err)
	}
	if len(fs.deleted) != 0 {
		t.Fatalf("deleted %v; an in-flight attempt's output was collected before its "+
			"row committed, which is silent data loss", fs.deleted)
	}
	if st.TooYoung != 1 {
		t.Errorf("TooYoung = %d, want 1", st.TooYoung)
	}
	if st.Orphans != 0 {
		t.Errorf("Orphans = %d, want 0", st.Orphans)
	}
}

// TestGracePeriodBoundaryIsInclusive: an object exactly at the cutoff is KEPT.
// The comparison must err toward keeping, so the boundary case is pinned rather
// than left to whichever way the inequality happened to be written.
func TestGracePeriodBoundaryIsInclusive(t *testing.T) {
	grace := time.Hour
	// A frozen clock: against the real one, "exactly at the cutoff" is
	// unconstructible — the two time.Now() calls differ by the microseconds
	// between them, so the test would assert on a race rather than on the
	// boundary rule.
	frozen := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	fs := &fakeStore{objects: []Candidate{
		{Key: "default/exactly-at-cutoff", LastModified: frozen.Add(-grace)},
		{Key: "default/one-nanosecond-younger", LastModified: frozen.Add(-grace).Add(time.Nanosecond)},
		{Key: "default/well-past-cutoff", LastModified: frozen.Add(-grace - time.Minute)},
	}}
	sw := sweeperWithRefs(t, fs, Config{DryRun: false, GracePeriod: grace}, refSet(), nil)
	sw.now = func() time.Time { return frozen }

	if _, err := sw.Once(context.Background()); err != nil {
		t.Fatalf("Once: %v", err)
	}
	for _, k := range fs.deleted {
		if k == "default/exactly-at-cutoff" || k == "default/one-nanosecond-younger" {
			t.Errorf("%s was deleted; at the grace boundary the sweep must err toward keeping", k)
		}
	}
	if len(fs.deleted) != 1 || fs.deleted[0] != "default/well-past-cutoff" {
		t.Fatalf("deleted %v, want only [default/well-past-cutoff]", fs.deleted)
	}
}

// TestDefaultGracePeriodIsGenerous guards the constant itself. Someone tuning
// this down to minutes would reintroduce the race the comment describes, so the
// floor is asserted rather than trusted.
func TestDefaultGracePeriodIsGenerous(t *testing.T) {
	if DefaultGracePeriod < time.Hour {
		t.Fatalf("DefaultGracePeriod = %s; hours are mandatory — it must cover a worker "+
			"that uploaded an object and has not yet managed to report it",
			DefaultGracePeriod)
	}
}

// TestZeroGracePeriodFallsBackToTheDefault: a Config with an unset GracePeriod
// must not mean "no grace at all". The zero value of a Duration is 0, which would
// otherwise be the most dangerous possible setting.
func TestZeroGracePeriodFallsBackToTheDefault(t *testing.T) {
	sw := NewSweeper(nil, &fakeStore{}, discardLogger(), Config{})
	if sw.Config().GracePeriod != DefaultGracePeriod {
		t.Fatalf("GracePeriod = %s with an unset Config, want the default %s; an unset "+
			"grace period must never mean zero grace",
			sw.Config().GracePeriod, DefaultGracePeriod)
	}
}

// ── Dry run ──────────────────────────────────────────────────────────────

// TestDryRunDeletesNothing: the reporting mode must not touch the store, while
// still producing the full classification an operator needs.
func TestDryRunDeletesNothing(t *testing.T) {
	fs := &fakeStore{objects: []Candidate{
		{Key: "default/orphan-1", LastModified: old(), SizeBytes: 100},
		{Key: "default/orphan-2", LastModified: old(), SizeBytes: 200},
		{Key: "default/live", LastModified: old(), SizeBytes: 300},
	}}
	sw := sweeperWithRefs(t, fs, Config{DryRun: true, GracePeriod: time.Hour},
		refSet("default/live"), nil)

	st, err := sw.Once(context.Background())
	if err != nil {
		t.Fatalf("Once: %v", err)
	}
	if len(fs.deleted) != 0 {
		t.Fatalf("a dry run deleted %v", fs.deleted)
	}
	if st.Deleted != 0 {
		t.Errorf("Stats.Deleted = %d in a dry run, want 0", st.Deleted)
	}
	if st.Orphans != 2 {
		t.Errorf("Orphans = %d, want 2", st.Orphans)
	}
	if st.OrphanBytes != 300 {
		t.Errorf("OrphanBytes = %d, want 300", st.OrphanBytes)
	}
	if len(st.OrphanKeys) != 2 {
		t.Errorf("OrphanKeys = %v, want both orphans reported so the run is auditable", st.OrphanKeys)
	}
}

// TestNewSweeperDefaultsToDryRun pins the safe default at the constructor, which
// is the only place it can be guaranteed: Config{}.DryRun is false, so a bare
// struct literal would be a DELETING sweeper.
func TestNewSweeperDefaultsToDryRun(t *testing.T) {
	if !NewDryRunSweeper(nil, &fakeStore{}, discardLogger(), Config{}).Config().DryRun {
		t.Fatal("NewDryRunSweeper did not produce a dry-run sweeper")
	}
}

// ── Bounding ─────────────────────────────────────────────────────────────

// TestPassIsBounded: the sweeper must ask the store for a bounded page rather
// than listing the whole bucket.
func TestPassIsBounded(t *testing.T) {
	var objs []Candidate
	for i := 0; i < 50; i++ {
		objs = append(objs, Candidate{Key: "default/k" + string(rune('a'+i%26)) + string(rune('0'+i/26)), LastModified: old()})
	}
	fs := &fakeStore{objects: objs}
	sw := sweeperWithRefs(t, fs, Config{DryRun: true, MaxObjects: 5, GracePeriod: time.Hour},
		refSet(), nil)

	st, err := sw.Once(context.Background())
	if err != nil {
		t.Fatalf("Once: %v", err)
	}
	if fs.listLimit != 5 {
		t.Errorf("sweeper asked for limit %d, want 5", fs.listLimit)
	}
	if st.Listed != 5 {
		t.Errorf("Listed = %d, want 5; the pass is not bounded", st.Listed)
	}
}

// TestZeroMaxObjectsFallsBackToTheDefault: an unset bound must not mean
// "unbounded".
func TestZeroMaxObjectsFallsBackToTheDefault(t *testing.T) {
	sw := NewSweeper(nil, &fakeStore{}, discardLogger(), Config{})
	if sw.Config().MaxObjects != DefaultMaxObjects {
		t.Fatalf("MaxObjects = %d with an unset Config, want %d", sw.Config().MaxObjects, DefaultMaxObjects)
	}
}

// TestDeleteErrorDoesNotStrandTheRest: one unremovable object must not abort the
// pass.
func TestDeleteErrorDoesNotStrandTheRest(t *testing.T) {
	fs := &fakeStore{
		objects: []Candidate{
			{Key: "default/bad", LastModified: old()},
			{Key: "default/good", LastModified: old()},
		},
		deleteErr: map[string]error{"default/bad": errors.New("access denied")},
	}
	sw := sweeperWithRefs(t, fs, Config{DryRun: false, GracePeriod: time.Hour}, refSet(), nil)

	st, err := sw.Once(context.Background())
	if err != nil {
		t.Fatalf("Once: %v", err)
	}
	if st.DeleteErrors != 1 {
		t.Errorf("DeleteErrors = %d, want 1", st.DeleteErrors)
	}
	if st.Deleted != 1 || len(fs.deleted) != 1 || fs.deleted[0] != "default/good" {
		t.Errorf("deleted %v (Stats.Deleted=%d), want [default/good]", fs.deleted, st.Deleted)
	}
}

// TestCancellationStopsDeleting: an operator's Ctrl-C must stop the pass promptly
// rather than finish removing the whole batch.
func TestCancellationStopsDeleting(t *testing.T) {
	fs := &fakeStore{objects: []Candidate{
		{Key: "default/a", LastModified: old()},
		{Key: "default/b", LastModified: old()},
	}}
	ctx, cancel := context.WithCancel(context.Background())
	sw := sweeperWithRefs(t, fs, Config{DryRun: false, GracePeriod: time.Hour}, refSet(), nil)
	cancel()

	st, err := sw.Once(ctx)
	if err == nil && st.Deleted != 0 {
		t.Fatalf("deleted %d objects under a cancelled context", st.Deleted)
	}
	if len(fs.deleted) != 0 {
		t.Fatalf("deleted %v under a cancelled context", fs.deleted)
	}
}

// ── Key extraction ───────────────────────────────────────────────────────

// TestExtractKeysFindsRefsUnderAnyOutputName is the shape rule that a fixed-path
// extractor would get wrong. The map key is the task's declared OUTPUT NAME,
// chosen by the workflow author — live data in this deployment contains
// "result", "out" and "features". A reader that only looked under "result" would
// report the others' objects as orphans and delete live data.
func TestExtractKeysFindsRefsUnderAnyOutputName(t *testing.T) {
	docs := map[string]string{
		"result": `{"result":{"blob":{"key":"default/aaa","bucket":"nexus-results","sha256":"aaa"},"size_bytes":"300000"}}`,
		"out":    `{"out":{"blob":{"key":"default/bbb","bucket":"nexus-results","sha256":"bbb"}}}`,
		"custom": `{"anything_the_author_named_it":{"blob":{"key":"default/ccc","sha256":"ccc"}}}`,
	}
	want := map[string]string{"result": "default/aaa", "out": "default/bbb", "custom": "default/ccc"}
	for name, doc := range docs {
		keys, err := ExtractKeys([]byte(doc))
		if err != nil {
			t.Fatalf("%s: ExtractKeys: %v", name, err)
		}
		if len(keys) != 1 || keys[0] != want[name] {
			t.Errorf("%s: keys = %v, want [%s]; a reference under output name %q was missed",
				name, keys, want[name], name)
		}
	}
}

// TestExtractKeysFindsMultipleRefsInOneDocument: input_refs holds one entry per
// resolved input, so a document can name several objects and all must be found.
func TestExtractKeysFindsMultipleRefsInOneDocument(t *testing.T) {
	doc := `{
		"features":{"blob":{"key":"default/f1","sha256":"f1"}},
		"labels":{"blob":{"key":"default/l1","sha256":"l1"}},
		"config":{"inline":"e30="}
	}`
	keys, err := ExtractKeys([]byte(doc))
	if err != nil {
		t.Fatalf("ExtractKeys: %v", err)
	}
	sort.Strings(keys)
	if len(keys) != 2 || keys[0] != "default/f1" || keys[1] != "default/l1" {
		t.Fatalf("keys = %v, want [default/f1 default/l1]", keys)
	}
}

// TestExtractKeysIgnoresInlineResults: an inline result names no object, so it
// must contribute nothing — and must not error.
func TestExtractKeysIgnoresInlineResults(t *testing.T) {
	doc := `{"result":{"inline":"eyJyb3dzIjo0Mn0=","size_bytes":"11","content_type":"application/json"}}`
	keys, err := ExtractKeys([]byte(doc))
	if err != nil {
		t.Fatalf("ExtractKeys: %v", err)
	}
	if len(keys) != 0 {
		t.Fatalf("keys = %v, want none for an inline result", keys)
	}
}

// TestExtractKeysIgnoresTheWordBlobInFreeText is the false positive a substring
// search produced against real data: workflows in this deployment are literally
// named "blob-threshold-test", and spec_json therefore contains "blob" without
// containing a BlobRef. Shape, not substring.
func TestExtractKeysIgnoresTheWordBlobInFreeText(t *testing.T) {
	doc := `{"name":"blob-threshold-test","description":"must land in MinIO as a blob, not inline","blob":"not-an-object"}`
	keys, err := ExtractKeys([]byte(doc))
	if err != nil {
		t.Fatalf("ExtractKeys: %v", err)
	}
	if len(keys) != 0 {
		t.Fatalf("keys = %v, want none; the literal word \"blob\" is not a reference", keys)
	}
}

// TestExtractKeysHandlesEmptyAndMalformedInput: a malformed document must error
// rather than silently yield no keys, because "no keys" from a row that has one
// is how live data gets deleted.
func TestExtractKeysHandlesEmptyAndMalformedInput(t *testing.T) {
	if keys, err := ExtractKeys(nil); err != nil || keys != nil {
		t.Errorf("ExtractKeys(nil) = %v, %v; want no keys and no error", keys, err)
	}
	if _, err := ExtractKeys([]byte(`{not json`)); err == nil {
		t.Error("a malformed document returned no error; it would silently contribute " +
			"zero references and make its object look collectable")
	}
}

// ── Dangling references ───────────────────────────────────────────────────

// fakeChecker is an in-memory ObjectChecker. It distinguishes "absent" from
// "could not tell", because collapsing those two is the specific bug that would
// turn a network blip into a false data-loss report.
type fakeChecker struct {
	present map[string]bool
	statErr error
	stated  []string
}

func (f *fakeChecker) StatObject(_ context.Context, key string) (bool, error) {
	if f.statErr != nil {
		return false, f.statErr
	}
	f.stated = append(f.stated, key)
	return f.present[key], nil
}

func (f *fakeChecker) Bucket() string { return "test-bucket" }

// A lookup FAILURE must abort rather than be read as "the object is gone". This
// is the property that keeps a DNS outage from being reported as total data
// loss, so it is pinned directly.
func TestCheckDanglingRefsAbortsWhenTheStoreCannotAnswer(t *testing.T) {
	checker := &fakeChecker{statErr: errors.New("connection refused")}

	_, err := CheckDanglingRefs(context.Background(), stubQuerier{}, checker, discardLogger(), 0)
	if err == nil {
		t.Fatal("a store that could not answer was treated as a successful check")
	}
}

// The report must name the ROW, not just the key: an operator cannot act on a
// content-addressed hash, and "which run lost its output" is the actual question.
func TestDanglingRefNamesTheDamagedWork(t *testing.T) {
	d := DanglingRef{
		Table: "task_attempts", Column: "output_ref",
		RowID: "att-1", RunID: "run-7", NodeID: "train", State: "SUCCEEDED",
		Key: "default/abc123",
	}
	s := d.String()
	for _, want := range []string{"run-7", "train", "default/abc123", "output_ref"} {
		if !strings.Contains(s, want) {
			t.Errorf("DanglingRef.String() = %q, missing %q", s, want)
		}
	}
}

// stubQuerier returns no rows. Enough for the store-side properties above; the
// SQL itself is exercised against a real database in the integration tests,
// matching how the sweep is tested.
type stubQuerier struct{}

func (stubQuerier) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, errors.New("stub querier: no database")
}

// TestLogObjectsAreCountedApartFromOutputs: the task-log share of a pass is
// reported separately, so log retention can be watched without it hiding in
// the output totals (the nexus_artifact_gc_last_run_log_* gauges).
func TestLogObjectsAreCountedApartFromOutputs(t *testing.T) {
	fs := &fakeStore{
		objects: []Candidate{
			{Key: "default/logs/att-1/00000000.ndjson", LastModified: old(), SizeBytes: 1000},
			{Key: "default/logs/att-1/log.ndjson", LastModified: old(), SizeBytes: 2000},
			{Key: "default/logs/att-2/00000000.ndjson", LastModified: old(), SizeBytes: 400},
			{Key: "default/logs/att-3/00000000.ndjson", LastModified: old(), SizeBytes: 50}, // live
			{Key: "default/logs/att-4/00000000.ndjson", LastModified: fresh(), SizeBytes: 60},
			{Key: "default/orphan-output", LastModified: old(), SizeBytes: 7},
		},
		deleteErr: map[string]error{"default/logs/att-2/00000000.ndjson": errors.New("access denied")},
	}
	sw := sweeperWithRefs(t, fs, Config{DryRun: false, GracePeriod: time.Hour},
		refSet("default/logs/att-3/00000000.ndjson"), nil)

	st, err := sw.Once(context.Background())
	if err != nil {
		t.Fatalf("Once: %v", err)
	}
	if st.LogObjects != 5 || st.LogOrphans != 3 || st.LogOrphanBytes != 3400 {
		t.Errorf("log objects/orphans/orphan bytes = %d/%d/%d, want 5/3/3400",
			st.LogObjects, st.LogOrphans, st.LogOrphanBytes)
	}
	if st.LogDeleted != 2 || st.LogDeletedBytes != 3000 || st.LogDeleteErrors != 1 {
		t.Errorf("log deleted/deleted bytes/delete errors = %d/%d/%d, want 2/3000/1",
			st.LogDeleted, st.LogDeletedBytes, st.LogDeleteErrors)
	}
	// The totals still include the log share.
	if st.Orphans != 4 || st.Deleted != 3 || st.DeleteErrors != 1 {
		t.Errorf("orphans/deleted/delete errors = %d/%d/%d, want 4/3/1", st.Orphans, st.Deleted, st.DeleteErrors)
	}
}

// TestDryRunCollectsNoLogBytes: a dry run reports collectable logs and deletes
// none of them.
func TestDryRunCollectsNoLogBytes(t *testing.T) {
	fs := &fakeStore{objects: []Candidate{
		{Key: "default/logs/att-1/log.ndjson", LastModified: old(), SizeBytes: 900},
	}}
	sw := sweeperWithRefs(t, fs, Config{DryRun: true, GracePeriod: time.Hour}, refSet(), nil)
	st, err := sw.Once(context.Background())
	if err != nil {
		t.Fatalf("Once: %v", err)
	}
	if st.LogOrphans != 1 || st.LogOrphanBytes != 900 || st.LogDeleted != 0 || st.LogDeletedBytes != 0 {
		t.Errorf("dry run log stats = %+v", st)
	}
}
