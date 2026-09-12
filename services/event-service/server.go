// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package eventservice

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"
)

// Server exposes the run history over HTTP, including an SSE tail.
type Server struct {
	store *Store
	log   *slog.Logger
	// pollInterval is how often the SSE tail checks for new events. Polling the
	// table rather than using LISTEN/NOTIFY is deliberate for Phase 1: NOTIFY is
	// not durable, caps payloads at 8KB, and holds a connection per listener
	// (ADR-0003). A 1s poll is well inside what an operator perceives as live.
	pollInterval time.Duration
}

func NewServer(store *Store, log *slog.Logger) *Server {
	return &Server{store: store, log: log, pollInterval: time.Second}
}

func (s *Server) Routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.healthz)
	mux.HandleFunc("GET /readyz", s.readyz)
	mux.HandleFunc("GET /v1/runs/{runID}/events", s.listEvents)
	mux.HandleFunc("GET /v1/runs/{runID}/events/stream", s.streamEvents)
	mux.HandleFunc("POST /v1/runs/{runID}/events", s.appendEvent)
	return mux
}

// healthz answers liveness and deliberately never touches Postgres: a liveness
// probe that fails on a transient DB blip gets the container killed, turning a
// recoverable database hiccup into a restart storm across the fleet.
func (s *Server) healthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// readyz answers readiness and DOES query the DB, because a replica that cannot
// reach Postgres should be taken out of rotation even though it should not be
// killed.
func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := contextWithTimeout(r, 2*time.Second)
	defer cancel()
	if err := s.store.pool.Ping(ctx); err != nil {
		writeJSON(w, http.StatusServiceUnavailable,
			map[string]string{"status": "unready", "reason": "database unreachable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

func (s *Server) listEvents(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("runID")
	opt := ListOpts{
		AfterSeq: int32(queryInt(r, "after_seq", 0)),
		Limit:    queryInt(r, "limit", 500),
		Kind:     r.URL.Query().Get("kind"),
		Actor:    r.URL.Query().Get("actor"),
	}
	events, err := s.store.List(r.Context(), runID, opt)
	if err != nil {
		s.log.ErrorContext(r.Context(), "list events failed", "run_id", runID, "error", err)
		writeJSON(w, http.StatusInternalServerError, errBody("could not read run history"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": events, "count": len(events)})
}

func (s *Server) appendEvent(w http.ResponseWriter, r *http.Request) {
	var body Event
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, errBody("malformed event body"))
		return
	}
	body.RunID = r.PathValue("runID")
	if body.Kind == "" || body.Actor == "" {
		// Actor is required, not optional: an unattributed state transition is
		// useless in an audit trail, which is a primary purpose of this table.
		writeJSON(w, http.StatusBadRequest, errBody("kind and actor are required"))
		return
	}
	ev, err := s.store.Append(r.Context(), body)
	if errors.Is(err, ErrRunNotFound) {
		writeJSON(w, http.StatusNotFound, errBody("run not found"))
		return
	}
	if err != nil {
		s.log.ErrorContext(r.Context(), "append event failed", "run_id", body.RunID, "error", err)
		writeJSON(w, http.StatusInternalServerError, errBody("could not append event"))
		return
	}
	writeJSON(w, http.StatusCreated, ev)
}

// streamEvents tails a run's history over SSE.
//
// The stream carries events, but a client must still treat the REST API as
// authoritative: a dropped or missed message costs latency, never correctness,
// because every event is durable in Postgres and re-readable by seq. That is the
// same principle ADR-0003 applies to the event bus.
func (s *Server) streamEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, errBody("streaming unsupported"))
		return
	}
	runID := r.PathValue("runID")
	after := int32(queryInt(r, "after_seq", 0))

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ticker := time.NewTicker(s.pollInterval)
	defer ticker.Stop()
	// Heartbeat comments keep intermediary proxies from reaping an idle stream.
	keepalive := time.NewTicker(15 * time.Second)
	defer keepalive.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-keepalive.C:
			fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
		case <-ticker.C:
			events, err := s.store.List(r.Context(), runID, ListOpts{AfterSeq: after, Limit: 200})
			if err != nil {
				// Do not kill the stream on a transient read error; the client
				// would reconnect and re-poll anyway, so retrying is cheaper.
				s.log.WarnContext(r.Context(), "stream read failed", "run_id", runID, "error", err)
				continue
			}
			for _, ev := range events {
				b, err := json.Marshal(ev)
				if err != nil {
					continue
				}
				fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", ev.Seq, ev.Kind, b)
				after = ev.Seq
			}
			if len(events) > 0 {
				flusher.Flush()
			}
		}
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func errBody(msg string) map[string]string { return map[string]string{"error": msg} }

func queryInt(r *http.Request, key string, def int) int {
	if v := r.URL.Query().Get(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}
