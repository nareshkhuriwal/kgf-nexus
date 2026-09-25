// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package rest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/khuriwalgroup/kgf-nexus/internal/store"
	"github.com/khuriwalgroup/kgf-nexus/services/api-gateway/authn"
	"github.com/khuriwalgroup/kgf-nexus/services/api-gateway/logs"
)

// GET /v1/namespaces/{ns}/attempts/{id}/logs (ADR-0020).
//
// One route, four representations, chosen by the request:
//
//   - ?q=...                         bounded server-side search (JSON)
//   - Accept: text/event-stream or ?stream=true
//     live follow over SSE; id: is the line number, so Last-Event-ID resumes
//     exactly where a dropped connection stopped
//   - Accept: text/plain or ?format=text
//     raw lines, streamed; with follow=true it keeps going until the log ends
//     (this is what `nexusctl logs -f` and the SDK's get_logs read)
//   - otherwise                      a JSON page: ?tail=N (default 500) or
//     ?from_line=K&limit=L
//
// ?stream_name=stdout|stderr filters every representation.

const (
	defaultLogTail = 500
	// logFollowPoll is how often a follow re-reads the index. The agent seals
	// chunks every ~2 s, so polling faster buys nothing.
	logFollowPoll = time.Second
	// logQuietAfterEnd ends a follow on a terminal attempt whose log never
	// finalized (a crashed agent): after this long with no new lines, nothing
	// more is coming.
	logQuietAfterEnd = 30 * time.Second
	logFollowBatch   = 1000
)

// logsPermission is the route's permission, re-checked on every poll of a
// follow so a revoked credential stops a stream that is already open.
const logsPermission = "tasks.logs.view"

func (a *API) attemptLogs(w http.ResponseWriter, r *http.Request, p authn.Principal) {
	ns, id := r.PathValue("ns"), r.PathValue("id")
	// Namespace-scoped existence check BEFORE anything is read, so logs are not
	// readable across tenants by attempt UUID.
	att, err := store.GatewayGetAttempt(r.Context(), a.db, ns, id)
	if err != nil {
		a.fail(w, r, "get attempt", err)
		return
	}
	if a.logs == nil {
		writeError(w, http.StatusNotImplemented, "logs_unavailable",
			"this gateway has no log backend configured (it needs result-store read credentials); see OPERATIONS, durable task logs")
		return
	}
	q := r.URL.Query()
	streamFilter := q.Get("stream_name")
	if streamFilter != "" && streamFilter != "stdout" && streamFilter != "stderr" && streamFilter != "combined" && streamFilter != "nexus" {
		writeError(w, http.StatusBadRequest, "invalid_argument", "stream_name must be stdout, stderr, combined or nexus")
		return
	}

	if pattern := q.Get("q"); pattern != "" {
		a.searchLogs(w, r, att, pattern, streamFilter)
		return
	}
	switch {
	case wantsSSE(r):
		a.followLogsSSE(w, r, ns, att, streamFilter)
	case q.Get("format") == "text" || acceptsTextPlain(r):
		a.streamLogsText(w, r, ns, att, streamFilter, q.Get("follow") == "true")
	default:
		a.logPage(w, r, att, streamFilter)
	}
}

func acceptsTextPlain(r *http.Request) bool {
	accept := strings.ToLower(r.Header.Get("Accept"))
	return strings.Contains(accept, "text/plain") && !strings.Contains(accept, "application/json")
}

func int64Param(r *http.Request, name string, def int64) int64 {
	v := r.URL.Query().Get(name)
	if v == "" {
		return def
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 {
		return def
	}
	return n
}

func filterStream(lines []logs.Line, stream string) []logs.Line {
	out := make([]logs.Line, 0, len(lines))
	for _, l := range lines {
		// Gap lines always pass: hiding "lines 40-80 are unavailable" behind a
		// filter would make a filtered view look complete when it is not.
		if stream == "" || l.Stream == stream || l.Kind == "gap" {
			out = append(out, l)
		}
	}
	return out
}

// expired upgrades a status when every chunk the index names is gone.
func expired(st logs.Status, page logs.Page) logs.Status {
	if page.Unreadable > 0 && page.Readable == 0 && st.State != logs.StateNone {
		st.State = logs.StateExpired
		st.Detail = "The log's objects are no longer in the result store (removed by log retention or deleted); the index still records its size."
	}
	return st
}

func (a *API) logPage(w http.ResponseWriter, r *http.Request, att store.AttemptRow, streamFilter string) {
	ctx := r.Context()
	row, found, st, err := a.logs.Summary(ctx, att.ID, att.State)
	if err != nil {
		a.fail(w, r, "read log summary", err)
		return
	}
	resp := map[string]any{"attempt_id": att.ID, "log": st, "lines": []logs.Line{}, "next_line": int64(1), "has_more": false}
	if !found {
		writeJSON(w, http.StatusOK, resp)
		return
	}

	var from int64
	var limit int
	if r.URL.Query().Has("from_line") {
		from = max(int64Param(r, "from_line", 1), 1)
		limit = intParam(r, "limit", defaultLogTail)
	} else {
		limit = intParam(r, "tail", defaultLogTail)
		if limit <= 0 {
			limit = defaultLogTail
		}
		from = logs.TailStart(row, min(limit, a.logs.Limits().MaxPageLines))
	}
	page, err := a.logs.ReadLines(ctx, att.ID, row, from, limit)
	if err != nil {
		a.fail(w, r, "read log lines", err)
		return
	}
	resp["log"] = expired(st, page)
	resp["lines"] = filterStream(page.Lines, streamFilter)
	resp["first_line"] = from
	resp["next_line"] = page.NextLine
	resp["has_more"] = page.NextLine <= row.LineCount
	if page.Unreadable > 0 {
		resp["unreadable_chunks"] = page.Unreadable
	}
	writeJSON(w, http.StatusOK, resp)
}

func (a *API) searchLogs(w http.ResponseWriter, r *http.Request, att store.AttemptRow, pattern, streamFilter string) {
	ctx := r.Context()
	row, found, st, err := a.logs.Summary(ctx, att.ID, att.State)
	if err != nil {
		a.fail(w, r, "read log summary", err)
		return
	}
	if !found {
		writeJSON(w, http.StatusOK, map[string]any{"attempt_id": att.ID, "log": st,
			"search": logs.SearchResult{Matches: []logs.Line{}, NextLine: 1, Complete: true}})
		return
	}
	qp := r.URL.Query()
	res, err := a.logs.Search(ctx, att.ID, row, logs.SearchQuery{
		Pattern:         pattern,
		Regex:           qp.Get("regex") == "true",
		CaseInsensitive: qp.Get("case") == "insensitive",
		Stream:          streamFilter,
		FromLine:        int64Param(r, "from_line", 1),
		MaxMatches:      intParam(r, "limit", 0),
		ScanBytes:       int64Param(r, "scan_bytes", 0),
	})
	if errors.Is(err, logs.ErrBadQuery) {
		writeError(w, http.StatusBadRequest, "invalid_argument", err.Error())
		return
	}
	if err != nil {
		a.fail(w, r, "search log", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"attempt_id": att.ID, "log": st, "search": res})
}

// logFollower is the shared loop behind SSE and plain-text follows: read what
// is new past the cursor, hand it to emit, decide whether the log has ended.
type logFollower struct {
	a      *API
	r      *http.Request
	ns     string
	attID  string
	cursor int64
	filter string
}

// step reads everything new. done=true when nothing more can arrive.
func (f *logFollower) step(ctx context.Context, lastProgress *time.Time, emit func(logs.Line) error) (st logs.Status, done bool, err error) {
	att, err := store.GatewayGetAttempt(ctx, f.a.db, f.ns, f.attID)
	if err != nil {
		return st, true, err
	}
	row, found, st, err := f.a.logs.Summary(ctx, att.ID, att.State)
	if err != nil {
		return st, false, err
	}
	terminal := logs.TerminalAttempt(att.State)
	if !found {
		// Nothing shipped. On a terminal attempt that will not change unless an
		// agent is mid-finalize, so give it the same quiet window.
		return st, terminal && time.Since(*lastProgress) > logQuietAfterEnd, nil
	}
	for f.cursor <= row.LineCount {
		page, err := f.a.logs.ReadLines(ctx, att.ID, row, f.cursor, logFollowBatch)
		if err != nil {
			return st, false, err
		}
		if len(page.Lines) == 0 {
			break
		}
		for _, l := range filterStream(page.Lines, f.filter) {
			if err := emit(l); err != nil {
				return st, true, err
			}
		}
		f.cursor = page.NextLine
		*lastProgress = time.Now()
		st = expired(st, page)
	}
	switch {
	case row.State == store.LogStateComplete && f.cursor > row.LineCount:
		return st, true, nil
	case terminal && time.Since(*lastProgress) > logQuietAfterEnd:
		return st, true, nil
	}
	return st, false, nil
}

// stillAllowed re-authenticates a long-lived stream, exactly as the run event
// stream does: a session revoked mid-tail must stop receiving lines.
func (a *API) stillAllowed(r *http.Request, ns string) bool {
	p, err := a.auth.AuthenticateCredential(r.Context(), authn.ExtractBearer(r.Header.Get("Authorization")))
	if err != nil || p.Authorize(ns, authn.ActionRead) != nil || !p.Allows(logsPermission) {
		return false
	}
	return true
}

func (a *API) followStart(r *http.Request, att store.AttemptRow) int64 {
	if lastID := r.Header.Get("Last-Event-ID"); lastID != "" {
		if n, err := strconv.ParseInt(lastID, 10, 64); err == nil && n >= 0 {
			return n + 1
		}
	}
	if r.URL.Query().Has("from_line") {
		return max(int64Param(r, "from_line", 1), 1)
	}
	if r.URL.Query().Has("tail") {
		row, found, _, err := a.logs.Summary(r.Context(), att.ID, att.State)
		if err == nil && found {
			return logs.TailStart(row, intParam(r, "tail", defaultLogTail))
		}
	}
	return 1
}

func (a *API) followLogsSSE(w http.ResponseWriter, r *http.Request, ns string, att store.AttemptRow, filter string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming_unsupported", "this server cannot stream")
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ctx := r.Context()
	f := &logFollower{a: a, r: r, ns: ns, attID: att.ID, cursor: a.followStart(r, att), filter: filter}
	lastProgress := time.Now()
	lastState := ""
	keepalive := time.NewTicker(sseKeepalive)
	defer keepalive.Stop()
	ticker := time.NewTicker(logFollowPoll)
	defer ticker.Stop()
	for {
		if !a.stillAllowed(r, ns) {
			fmt.Fprint(w, "event: error\ndata: {\"error\":\"forbidden\"}\n\n")
			flusher.Flush()
			return
		}
		st, done, err := f.step(ctx, &lastProgress, func(l logs.Line) error {
			b, err := json.Marshal(l)
			if err != nil {
				return nil
			}
			// id: is the line's LAST number, so a resume continues after a gap
			// line rather than repeating it.
			_, werr := fmt.Fprintf(w, "id: %d\nevent: line\ndata: %s\n\n", l.End(), b)
			return werr
		})
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			a.log.ErrorContext(ctx, "log follow read failed", "attempt_id", att.ID, "error", err)
			fmt.Fprint(w, "event: error\ndata: {\"error\":\"read_failed\"}\n\n")
			flusher.Flush()
			return
		}
		if st.State != lastState {
			if b, err := json.Marshal(st); err == nil {
				fmt.Fprintf(w, "event: status\ndata: %s\n\n", b)
			}
			lastState = st.State
		}
		flusher.Flush()
		if done {
			b, _ := json.Marshal(st)
			fmt.Fprintf(w, "event: end\ndata: %s\n\n", b)
			flusher.Flush()
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-keepalive.C:
			fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
		case <-ticker.C:
		}
	}
}

// streamLogsText writes raw lines. Without follow it writes the whole log as it
// stands and ends; with follow it continues until the log ends.
func (a *API) streamLogsText(w http.ResponseWriter, r *http.Request, ns string, att store.AttemptRow, filter string, follow bool) {
	ctx := r.Context()
	row, found, st, err := a.logs.Summary(ctx, att.ID, att.State)
	if err != nil {
		a.fail(w, r, "read log summary", err)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/plain; charset=utf-8")
	h.Set("X-Nexus-Log-State", st.State)
	h.Set("X-Accel-Buffering", "no")
	if found {
		h.Set("X-Nexus-Log-Lines", strconv.FormatInt(row.LineCount, 10))
	}
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	flush := func() {
		if flusher != nil {
			flusher.Flush()
		}
	}
	emit := func(l logs.Line) error {
		_, err := fmt.Fprintln(w, l.Message)
		return err
	}

	f := &logFollower{a: a, r: r, ns: ns, attID: att.ID, cursor: a.followStart(r, att), filter: filter}
	if !follow {
		// One pass over what exists now, page by page so memory stays bounded.
		for found && f.cursor <= row.LineCount {
			page, err := a.logs.ReadLines(ctx, att.ID, row, f.cursor, logFollowBatch)
			if err != nil || len(page.Lines) == 0 {
				if err != nil && ctx.Err() == nil {
					a.log.ErrorContext(ctx, "log text read failed", "attempt_id", att.ID, "error", err)
				}
				return
			}
			for _, l := range filterStream(page.Lines, filter) {
				if emit(l) != nil {
					return
				}
			}
			f.cursor = page.NextLine
			flush()
		}
		return
	}
	lastProgress := time.Now()
	ticker := time.NewTicker(logFollowPoll)
	defer ticker.Stop()
	for {
		if !a.stillAllowed(r, ns) {
			return
		}
		_, done, err := f.step(ctx, &lastProgress, emit)
		flush()
		if err != nil || done {
			if err != nil && ctx.Err() == nil {
				a.log.ErrorContext(ctx, "log text follow failed", "attempt_id", att.ID, "error", err)
			}
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
