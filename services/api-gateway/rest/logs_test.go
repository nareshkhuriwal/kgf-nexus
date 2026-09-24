// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package rest

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/khuriwalgroup/kgf-nexus/internal/store"
	"github.com/khuriwalgroup/kgf-nexus/services/api-gateway/authn"
	"github.com/khuriwalgroup/kgf-nexus/services/api-gateway/logs"
)

// ── doubles ───────────────────────────────────────────────────────────────

// logsDB adds namespace-scoped attempt lookup to restDB: the query the handler
// runs BEFORE touching any log, which is the tenancy boundary under test.
type logsDB struct {
	*restDB
	mu       sync.Mutex
	attempts map[string]string // "ns\x00id" -> attempt state
}

func (d *logsDB) setAttempt(ns, id, state string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.attempts[ns+"\x00"+id] = state
}

func (d *logsDB) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if strings.Contains(sql, "FROM task_attempts a") {
		d.mu.Lock()
		defer d.mu.Unlock()
		id, ns := args[0].(string), args[1].(string)
		state, ok := d.attempts[ns+"\x00"+id]
		if !ok {
			return errRow{pgx.ErrNoRows}
		}
		return reflectRow{[]any{id, "run-1", "node", int32(1), state, "default",
			(*string)(nil), (*string)(nil), (*string)(nil), []byte(nil), []byte(nil),
			time.Now(), (*time.Time)(nil), (*time.Time)(nil), (*string)(nil)}}
	}
	return d.restDB.QueryRow(ctx, sql, args...)
}

type reflectRow struct{ vals []any }

func (r reflectRow) Scan(dest ...any) error {
	if len(dest) != len(r.vals) {
		return fmt.Errorf("scan wants %d destinations, row has %d", len(dest), len(r.vals))
	}
	for i, d := range dest {
		reflect.ValueOf(d).Elem().Set(reflect.ValueOf(r.vals[i]))
	}
	return nil
}

// fakeStreamer is an in-memory log whose lines and state a test can grow,
// which is what a follow needs to observe.
type fakeStreamer struct {
	mu        sync.Mutex
	found     bool
	state     string
	lines     []logs.Line
	lastQuery logs.SearchQuery
}

func (f *fakeStreamer) add(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.found = true
	for i := 0; i < n; i++ {
		k := int64(len(f.lines) + 1)
		stream := "stdout"
		if k%2 == 0 {
			stream = "stderr"
		}
		f.lines = append(f.lines, logs.Line{N: k, Stream: stream, Message: fmt.Sprintf("line %d", k)})
	}
}

func (f *fakeStreamer) row() store.AttemptLogRow {
	return store.AttemptLogRow{State: f.state, LineCount: int64(len(f.lines))}
}

func (f *fakeStreamer) Summary(_ context.Context, _, attemptState string) (store.AttemptLogRow, bool, logs.Status, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	st := logs.Status{AttemptState: attemptState, Lines: int64(len(f.lines))}
	switch {
	case !f.found:
		st.State = logs.StateNone
	case f.state == store.LogStateComplete:
		st.State = logs.StateComplete
	case logs.TerminalAttempt(attemptState):
		st.State = logs.StatePartial
	default:
		st.State = logs.StateLive
	}
	return f.row(), f.found, st, nil
}

func (f *fakeStreamer) ReadLines(_ context.Context, _ string, _ store.AttemptLogRow, from int64, limit int) (logs.Page, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := logs.Page{NextLine: from, Readable: 1}
	for _, l := range f.lines {
		if l.N >= from && len(p.Lines) < limit {
			p.Lines = append(p.Lines, l)
			p.NextLine = l.N + 1
		}
	}
	return p, nil
}

func (f *fakeStreamer) Search(_ context.Context, _ string, _ store.AttemptLogRow, q logs.SearchQuery) (logs.SearchResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastQuery = q
	if q.Pattern == "(" {
		return logs.SearchResult{}, fmt.Errorf("%w: invalid regular expression", logs.ErrBadQuery)
	}
	res := logs.SearchResult{Matches: []logs.Line{}, Complete: true}
	for _, l := range f.lines {
		if strings.Contains(l.Message, q.Pattern) {
			res.Matches = append(res.Matches, l)
		}
	}
	return res, nil
}

func (f *fakeStreamer) Limits() logs.Limits { return logs.DefaultLimits() }

const workerKey = "nx_worker_key"

func newLogsAPI(t *testing.T, withBackend bool) (http.Handler, *logsDB, *fakeStreamer) {
	t.Helper()
	base := newRestDB()
	base.addKey(adminKey, "team-ml", "ci", "admin")
	base.addKey(readerKey, "team-ml", "dashboard", "reader")
	base.addKey(workerKey, "team-ml", "agent", "worker")
	base.addKey("nx_other_tenant", "team-other", "other", "admin")
	db := &logsDB{restDB: base, attempts: map[string]string{}}
	db.setAttempt("team-ml", "att-1", "RUNNING")
	fs := &fakeStreamer{}
	var ls LogStreamer
	if withBackend {
		ls = fs
	}
	api := New(db, authn.New(db), &fakeWFM{}, fakeRM{}, ls, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return api.Routes(), db, fs
}

func getLogs(t *testing.T, h http.Handler, path, key string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

const logsPath = "/v1/namespaces/team-ml/attempts/att-1/logs"

// ── authorization ─────────────────────────────────────────────────────────

func TestLogsAuthorization(t *testing.T) {
	h, _, fs := newLogsAPI(t, true)
	fs.add(3)
	cases := []struct {
		name, path, key string
		want            int
	}{
		{"no credential", logsPath, "", http.StatusUnauthorized},
		{"worker keys cannot read logs", logsPath, workerKey, http.StatusForbidden},
		{"another tenant's key", logsPath, "nx_other_tenant", http.StatusForbidden},
		{"another tenant's namespace path for our attempt id", "/v1/namespaces/team-other/attempts/att-1/logs", "nx_other_tenant", http.StatusNotFound},
		{"reader", logsPath, readerKey, http.StatusOK},
		{"admin", logsPath, adminKey, http.StatusOK},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if rec := getLogs(t, h, c.path, c.key, nil); rec.Code != c.want {
				t.Fatalf("status %d, want %d: %s", rec.Code, c.want, rec.Body)
			}
		})
	}
}

func TestLogsWithoutBackendAnswer501(t *testing.T) {
	h, _, _ := newLogsAPI(t, false)
	rec := getLogs(t, h, logsPath, readerKey, nil)
	if rec.Code != http.StatusNotImplemented || !strings.Contains(rec.Body.String(), "logs_unavailable") {
		t.Fatalf("status %d body %s", rec.Code, rec.Body)
	}
}

// ── pages, tail, search ───────────────────────────────────────────────────

type pageResp struct {
	Log      logs.Status `json:"log"`
	Lines    []logs.Line `json:"lines"`
	NextLine int64       `json:"next_line"`
	HasMore  bool        `json:"has_more"`
	Search   struct {
		Matches []logs.Line `json:"matches"`
	} `json:"search"`
}

func decodePage(t *testing.T, rec *httptest.ResponseRecorder) pageResp {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var p pageResp
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return p
}

func TestLogsNoneIsAStateNotAnError(t *testing.T) {
	h, _, _ := newLogsAPI(t, true)
	p := decodePage(t, getLogs(t, h, logsPath, readerKey, nil))
	if p.Log.State != logs.StateNone || len(p.Lines) != 0 || p.Lines == nil {
		t.Fatalf("no-log response = %+v; want state none and an empty (not null) lines array", p)
	}
}

func TestLogsTailPageAndStreamFilter(t *testing.T) {
	h, _, fs := newLogsAPI(t, true)
	fs.add(50)

	p := decodePage(t, getLogs(t, h, logsPath+"?tail=5", readerKey, nil))
	if len(p.Lines) != 5 || p.Lines[0].N != 46 || p.NextLine != 51 || p.HasMore || p.Log.State != logs.StateLive {
		t.Fatalf("tail = %+v", p)
	}
	p = decodePage(t, getLogs(t, h, logsPath+"?from_line=10&limit=5", readerKey, nil))
	if len(p.Lines) != 5 || p.Lines[0].N != 10 || p.NextLine != 15 || !p.HasMore {
		t.Fatalf("page = %+v", p)
	}
	p = decodePage(t, getLogs(t, h, logsPath+"?from_line=1&limit=10&stream_name=stderr", readerKey, nil))
	for _, l := range p.Lines {
		if l.Stream != "stderr" {
			t.Fatalf("stream filter leaked %+v", l)
		}
	}
	if rec := getLogs(t, h, logsPath+"?stream_name=bogus", readerKey, nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad stream_name: %d", rec.Code)
	}
}

func TestLogsSearch(t *testing.T) {
	h, _, fs := newLogsAPI(t, true)
	fs.add(30)
	p := decodePage(t, getLogs(t, h, logsPath+"?q=line+2&case=insensitive&regex=true&from_line=3&limit=7", readerKey, nil))
	if len(p.Search.Matches) != 11 { // "line 2", "line 20".."line 29"
		t.Fatalf("matches = %d", len(p.Search.Matches))
	}
	q := fs.lastQuery
	if !q.Regex || !q.CaseInsensitive || q.FromLine != 3 || q.MaxMatches != 7 {
		t.Fatalf("query parameters were not passed through: %+v", q)
	}
	if rec := getLogs(t, h, logsPath+"?q=(&regex=true", readerKey, nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("an invalid pattern must be a 400, got %d", rec.Code)
	}
}

// ── text and follow ───────────────────────────────────────────────────────

func TestLogsPlainTextIsWhatTheCLIReads(t *testing.T) {
	h, db, fs := newLogsAPI(t, true)
	fs.add(3)
	fs.state = store.LogStateComplete
	db.setAttempt("team-ml", "att-1", "SUCCEEDED")
	rec := getLogs(t, h, logsPath, readerKey, map[string]string{"Accept": "text/plain"})
	if rec.Code != http.StatusOK || rec.Body.String() != "line 1\nline 2\nline 3\n" {
		t.Fatalf("status %d body %q", rec.Code, rec.Body)
	}
	if rec.Header().Get("X-Nexus-Log-State") != logs.StateComplete {
		t.Fatalf("X-Nexus-Log-State = %q", rec.Header().Get("X-Nexus-Log-State"))
	}
}

// sseEvent is one parsed frame.
type sseEvent struct{ id, event, data string }

func readSSE(t *testing.T, r *bufio.Reader, until func(sseEvent) bool) []sseEvent {
	t.Helper()
	var out []sseEvent
	var cur sseEvent
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			t.Fatalf("stream ended early after %d events: %v", len(out), err)
		}
		line = strings.TrimRight(line, "\n")
		switch {
		case line == "":
			if cur.event != "" || cur.data != "" {
				out = append(out, cur)
				if until(cur) {
					return out
				}
			}
			cur = sseEvent{}
		case strings.HasPrefix(line, "id: "):
			cur.id = line[4:]
		case strings.HasPrefix(line, "event: "):
			cur.event = line[7:]
		case strings.HasPrefix(line, "data: "):
			cur.data = line[6:]
		}
	}
}

// TestLogsFollowStreamsNewLinesAndEndsWithTheLog: a follow delivers lines as
// they are indexed, resumes from Last-Event-ID without repeating, and ends
// with an end event once the attempt is terminal and its log complete.
func TestLogsFollowStreamsNewLinesAndEndsWithTheLog(t *testing.T) {
	h, db, fs := newLogsAPI(t, true)
	fs.add(2)
	srv := httptest.NewServer(h)
	defer srv.Close()

	open := func(lastID string) (*http.Response, *bufio.Reader) {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+logsPath+"?stream=true", nil)
		req.Header.Set("Authorization", "Bearer "+readerKey)
		if lastID != "" {
			req.Header.Set("Last-Event-ID", lastID)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
			t.Fatalf("status %d type %q", resp.StatusCode, resp.Header.Get("Content-Type"))
		}
		return resp, bufio.NewReader(resp.Body)
	}

	resp, r := open("")
	evs := readSSE(t, r, func(e sseEvent) bool { return e.event == "line" && e.id == "2" })
	fs.add(2) // lines 3 and 4 arrive while the client is attached
	evs = append(evs, readSSE(t, r, func(e sseEvent) bool { return e.event == "line" && e.id == "4" })...)
	resp.Body.Close()
	var ids []string
	for _, e := range evs {
		if e.event == "line" {
			ids = append(ids, e.id)
		}
	}
	if strings.Join(ids, ",") != "1,2,3,4" {
		t.Fatalf("line ids = %v", ids)
	}

	// Reconnect after line 3: only line 4 onwards, then the end once the
	// attempt is terminal and the log complete.
	fs.add(1)
	fs.mu.Lock()
	fs.state = store.LogStateComplete
	fs.mu.Unlock()
	db.setAttempt("team-ml", "att-1", "SUCCEEDED")
	resp, r = open("3")
	defer resp.Body.Close()
	evs = readSSE(t, r, func(e sseEvent) bool { return e.event == "end" })
	ids = nil
	for _, e := range evs {
		if e.event == "line" {
			ids = append(ids, e.id)
		}
	}
	if strings.Join(ids, ",") != "4,5" {
		t.Fatalf("resumed ids = %v; Last-Event-ID must resume without gaps or repeats", ids)
	}
	if !strings.Contains(evs[len(evs)-1].data, `"state":"complete"`) {
		t.Fatalf("end event = %+v", evs[len(evs)-1])
	}
}
