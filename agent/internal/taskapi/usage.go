// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package taskapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
)

// Usage bounds (ADR-0032 §2). The scheduler applies the same ones and refuses
// a report that breaks them whole, so enforcing them here too turns what would
// be a silent upstream refusal (after the task exited) into an immediate 400
// the task author can see.
const (
	MaxUsageKeyBytes    = 200
	MaxUsageKeys        = 64
	MaxUsageIncrement   = 1_000_000_000_000     // 10^12 per call
	MaxUsageTotalPerKey = 1_000_000_000_000_000 // 10^15 per key per attempt
)

// usageKeyPattern is the key convention shared with the price book:
// <kind>:<subject>[:<qualifier>].
var usageKeyPattern = regexp.MustCompile(
	`^[a-z][a-z0-9_]{0,31}:[A-Za-z0-9][A-Za-z0-9._/@+-]{0,127}(:[a-z][a-z0-9_]{0,31})?$`)

// ValidUsageKey reports whether key follows the ADR-0032 §2 grammar.
func ValidUsageKey(key string) bool {
	return len(key) <= MaxUsageKeyBytes && usageKeyPattern.MatchString(key)
}

// parseUsageIncrements validates a /v1/usage body's units. Values must be JSON
// integers (not strings, not fractions: a fractional quantity uses a smaller
// unit), in [0, MaxUsageIncrement].
func parseUsageIncrements(raw map[string]json.RawMessage) (map[string]int64, error) {
	if raw == nil {
		return nil, errors.New(`body must be {"units": {"<key>": <integer>}}`)
	}
	out := make(map[string]int64, len(raw))
	for k, v := range raw {
		if !ValidUsageKey(k) {
			return nil, fmt.Errorf("invalid usage key %q: want <kind>:<subject>[:<qualifier>] "+
				"(e.g. tokens:openai/gpt-4o-mini:input), at most %d bytes", k, MaxUsageKeyBytes)
		}
		v = bytes.TrimSpace(v)
		if len(v) == 0 || !(v[0] == '-' || (v[0] >= '0' && v[0] <= '9')) {
			return nil, fmt.Errorf("usage %q: value must be a JSON integer", k)
		}
		n, err := strconv.ParseInt(string(v), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("usage %q: value %s is not an integer in range", k, v)
		}
		if n < 0 || n > MaxUsageIncrement {
			return nil, fmt.Errorf("usage %q: increment %d outside [0, %d]", k, n, int64(MaxUsageIncrement))
		}
		out[k] = n
	}
	return out, nil
}

// usageState is the attempt's cumulative usage and its flush bookkeeping.
type usageState struct {
	mu      sync.Mutex
	totals  map[string]int64
	version uint64 // bumped on every change
	flushed uint64 // version last acknowledged upstream

	// flushMu serializes upstream sends so the background flusher and the
	// final flush never interleave. (A reordered snapshot would be harmless
	// -- the scheduler merges with max -- but serial sends make "the final
	// flush covers everything" trivially true.)
	flushMu sync.Mutex
	// disabled stops all flushing: the lease is stale or the gateway does not
	// implement ReportUsage. Guarded by flushMu.
	disabled bool
	// warned rate-limits the flusher's failure log to once per attempt.
	warned bool

	stop     chan struct{}
	stopOnce sync.Once
}

func (u *usageState) init() {
	u.totals = map[string]int64{}
	u.stop = make(chan struct{})
}

func (u *usageState) stopFlusher() { u.stopOnce.Do(func() { close(u.stop) }) }

// add applies increments atomically: all or none.
func (u *usageState) add(inc map[string]int64) (map[string]int64, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	newKeys := 0
	for k, n := range inc {
		cur, exists := u.totals[k]
		if !exists {
			newKeys++
		}
		if cur+n > MaxUsageTotalPerKey {
			return nil, fmt.Errorf("usage %q: cumulative total would exceed %d", k, int64(MaxUsageTotalPerKey))
		}
	}
	if len(u.totals)+newKeys > MaxUsageKeys {
		return nil, fmt.Errorf("at most %d distinct usage keys per attempt (have %d, request adds %d)",
			MaxUsageKeys, len(u.totals), newKeys)
	}
	changed := false
	for k, n := range inc {
		if _, exists := u.totals[k]; !exists || n != 0 {
			changed = true
		}
		u.totals[k] += n
	}
	if changed {
		u.version++
	}
	return maps.Clone(u.totals), nil
}

func (u *usageState) snapshot() (map[string]int64, uint64) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return maps.Clone(u.totals), u.version
}

// UsageTotals returns a copy of the attempt's cumulative usage.
func (s *Server) UsageTotals() map[string]int64 {
	t, _ := s.usage.snapshot()
	return t
}

func (s *Server) handleUsage(w http.ResponseWriter, r *http.Request) {
	if !s.caps.Usage {
		writeError(w, http.StatusNotImplemented, "unsupported",
			"the control plane does not record usage units for this agent")
		return
	}
	if s.stale.Load() {
		writeError(w, http.StatusConflict, "stale_lease", "this attempt no longer owns its lease")
		return
	}
	var body struct {
		Units map[string]json.RawMessage `json:"units"`
	}
	if !decodeJSON(w, r, &body, "invalid_usage") {
		return
	}
	inc, err := parseUsageIncrements(body.Units)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_usage", err.Error())
		return
	}
	totals, err := s.usage.add(inc)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_usage", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"totals": totals})
}

// flushLoop sends the cumulative snapshot at most every UsageFlushInterval
// while it changes. It never retries faster than the interval: the final
// flush is what must land, and it has its own bounded retry.
func (s *Server) flushLoop() {
	t := time.NewTicker(s.cfg.UsageFlushInterval)
	defer t.Stop()
	for {
		select {
		case <-s.usage.stop:
			return
		case <-t.C:
			ctx, cancel := context.WithTimeout(s.baseCtx, s.cfg.RPCTimeout)
			s.flushOnce(ctx)
			cancel()
		}
	}
}

// flushResult is what one flush attempt concluded.
type flushResult int

const (
	flushDone  flushResult = iota // nothing to send, or sent and acknowledged
	flushRetry                    // transient failure; try again
	flushFinal                    // permanent: stale lease, unimplemented, refused
)

// flushOnce sends the current snapshot if it changed since the last
// acknowledged one.
func (s *Server) flushOnce(ctx context.Context) flushResult {
	u := &s.usage
	u.flushMu.Lock()
	defer u.flushMu.Unlock()
	if u.disabled {
		return flushFinal
	}
	totals, version := u.snapshot()
	if version == u.flushed {
		return flushDone
	}
	ack, err := s.cfg.Upstream.ReportUsage(ctx, &nexusv1.ReportUsageRequest{
		AttemptId: s.cfg.AttemptID, LeaseToken: s.cfg.LeaseToken(), Units: totals,
	})
	switch {
	case err != nil && status.Code(err) == codes.Unimplemented:
		// Advertised but not served: a gateway mid-rollout. Nothing will
		// change within this attempt.
		u.disabled = true
		s.cfg.Metrics.UsageFlush("error")
		s.log.Warn("the control plane does not implement ReportUsage; this attempt's usage is not recorded",
			"keys", len(totals))
		return flushFinal
	case err != nil:
		s.cfg.Metrics.UsageFlush("error")
		if !u.warned {
			u.warned = true
			s.log.Warn("ReportUsage failed; will retry", "error", err)
		}
		return flushRetry
	case ack.GetAccepted():
		u.flushed = version
		s.cfg.Metrics.UsageFlush("ok")
		return flushDone
	case ack.GetReason() == reasonStaleLease:
		u.disabled = true
		s.stale.Store(true)
		s.cfg.Metrics.UsageFlush("stale")
		s.log.Warn("ReportUsage fenced: the attempt no longer owns its lease; usage flushing stopped")
		return flushFinal
	case ack.GetReason() == reasonLeaseUnverified:
		// A database blip on the control plane, not a verdict.
		s.cfg.Metrics.UsageFlush("error")
		return flushRetry
	default:
		// A validation refusal (the same bounds are enforced here, so this is
		// a version skew) or usage recording disabled. Asking again with the
		// same totals cannot help.
		u.disabled = true
		s.cfg.Metrics.UsageFlush("error")
		s.log.Warn("ReportUsage refused; usage flushing stopped for this attempt",
			"reason", ack.GetReason(), "keys", sortedKeys(totals))
		return flushFinal
	}
}

// ErrFinalFlushIncomplete means the final usage snapshot could not be
// delivered within its budget.
var ErrFinalFlushIncomplete = errors.New("taskapi: final usage flush did not complete")

// FinalFlush stops the background flusher and synchronously delivers the
// attempt's final cumulative usage, retrying transient failures with capped
// exponential backoff for at most FinalFlushTimeout (or ctx, if sooner).
//
// It MUST complete before the attempt's ReportResult: the control plane
// records usage into the ledger in the same transaction as the terminal
// transition, and a report that arrives after it is refused as stale (ADR-0032
// §2). Call it after Close, so no increment can arrive after the snapshot.
// A stale lease ends it at once: those units can no longer be recorded.
func (s *Server) FinalFlush(ctx context.Context) error {
	s.usage.stopFlusher()
	if !s.caps.Usage {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, s.cfg.FinalFlushTimeout)
	defer cancel()
	delay := 250 * time.Millisecond
	for {
		rctx, rcancel := context.WithTimeout(ctx, s.cfg.RPCTimeout)
		res := s.flushOnce(rctx)
		rcancel()
		switch res {
		case flushDone:
			return nil
		case flushFinal:
			if s.stale.Load() {
				return nil // fenced: nothing more can be recorded, and that is not an agent failure
			}
			return fmt.Errorf("%w: usage recording refused or unavailable", ErrFinalFlushIncomplete)
		}
		t := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			t.Stop()
			totals, _ := s.usage.snapshot()
			return fmt.Errorf("%w within %s (%d keys unreported): %w",
				ErrFinalFlushIncomplete, s.cfg.FinalFlushTimeout, len(totals), ctx.Err())
		case <-t.C:
		}
		delay = min(delay*2, 5*time.Second)
	}
}

func sortedKeys(m map[string]int64) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
