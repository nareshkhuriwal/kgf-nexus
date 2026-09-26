// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package taskapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
)

// Permit bounds (ADR-0032 §3).
const (
	MaxPermitKeys     = 8
	MaxPermitRequests = 1_000_000
	MaxPermitTokens   = 1_000_000_000
)

// subjectPattern is the rate-limit key grammar: a usage-key subject,
// <provider> or <provider>/<model>.
var subjectPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/@+-]{0,127}$`)

type permitsBody struct {
	Keys     []string `json:"keys"`
	Requests int64    `json:"requests"`
	Tokens   int64    `json:"tokens"`
}

// validatePermitKeys checks 1..MaxPermitKeys distinct subject-grammar keys.
// Duplicates are refused rather than folded: the scheduler takes every key's
// bucket row in one transaction, and a request that names a key twice is a
// caller bug worth surfacing.
func validatePermitKeys(keys []string) error {
	if len(keys) == 0 || len(keys) > MaxPermitKeys {
		return fmt.Errorf("keys: want 1 to %d rate-limit keys, got %d", MaxPermitKeys, len(keys))
	}
	seen := make(map[string]bool, len(keys))
	for _, k := range keys {
		if !subjectPattern.MatchString(k) {
			return fmt.Errorf("keys: %q is not a rate-limit key (<provider> or <provider>/<model>)", k)
		}
		if seen[k] {
			return fmt.Errorf("keys: %q appears twice", k)
		}
		seen[k] = true
	}
	return nil
}

func (b permitsBody) validate() error {
	if err := validatePermitKeys(b.Keys); err != nil {
		return err
	}
	if b.Requests < 0 || b.Requests > MaxPermitRequests {
		return fmt.Errorf("requests: %d outside [0, %d]", b.Requests, MaxPermitRequests)
	}
	if b.Tokens < 0 || b.Tokens > MaxPermitTokens {
		return fmt.Errorf("tokens: %d outside [0, %d]", b.Tokens, MaxPermitTokens)
	}
	if b.Requests == 0 && b.Tokens == 0 {
		return errors.New("at least one of requests and tokens must be positive")
	}
	return nil
}

func (s *Server) handlePermits(w http.ResponseWriter, r *http.Request) {
	if !s.caps.RateLimits {
		writeError(w, http.StatusNotImplemented, "unsupported",
			"the control plane does not offer shared rate limits to this agent")
		return
	}
	var body permitsBody
	if !decodeJSON(w, r, &body, "invalid_request") {
		return
	}
	if err := body.validate(); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	resp, ok := s.acquire(r.Context(), w, &nexusv1.AcquirePermitsRequest{
		Keys: body.Keys, Requests: body.Requests, Tokens: body.Tokens,
	})
	if !ok {
		return
	}
	unlimited := resp.GetUnlimitedKeys()
	if unlimited == nil {
		unlimited = []string{} // [] rather than null: simpler for every client
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"granted":        resp.GetGranted(),
		"retry_after_ms": resp.GetRetryAfterMs(),
		"unlimited_keys": unlimited,
	})
}

// handleSettle corrects tokens already taken (actual - estimated), applied
// by the scheduler without a check.
func (s *Server) handleSettle(w http.ResponseWriter, r *http.Request) {
	if !s.caps.RateLimits {
		writeError(w, http.StatusNotImplemented, "unsupported",
			"the control plane does not offer shared rate limits to this agent")
		return
	}
	var body struct {
		Keys   []string `json:"keys"`
		Tokens int64    `json:"tokens"`
	}
	if !decodeJSON(w, r, &body, "invalid_request") {
		return
	}
	if err := validatePermitKeys(body.Keys); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if body.Tokens < -MaxPermitTokens || body.Tokens > MaxPermitTokens {
		writeError(w, http.StatusBadRequest, "invalid_request",
			fmt.Sprintf("tokens: %d outside [-%d, %d]", body.Tokens, MaxPermitTokens, MaxPermitTokens))
		return
	}
	if body.Tokens == 0 {
		// The estimate was exact: nothing to correct, no round trip.
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if _, ok := s.acquire(r.Context(), w, &nexusv1.AcquirePermitsRequest{
		Keys: body.Keys, Tokens: body.Tokens, Settle: true,
	}); !ok {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// acquire makes the fenced AcquirePermits call and answers the workload
// itself on every failure.
func (s *Server) acquire(ctx context.Context, w http.ResponseWriter,
	req *nexusv1.AcquirePermitsRequest) (*nexusv1.AcquirePermitsResponse, bool) {
	req.AttemptId = s.cfg.AttemptID
	req.LeaseToken = s.cfg.LeaseToken()
	ctx, cancel := context.WithTimeout(ctx, s.cfg.RPCTimeout)
	defer cancel()
	resp, err := s.cfg.Upstream.AcquirePermits(ctx, req)
	if err != nil {
		s.upstreamError(w, "AcquirePermits", err)
		return nil, false
	}
	if !resp.GetAccepted() {
		s.refused(w, "AcquirePermits", resp.GetReason())
		return nil, false
	}
	return resp, true
}

// upstreamError maps a transport or status error. Only UNIMPLEMENTED and
// INVALID_ARGUMENT are verdicts; everything else is "the control plane could
// not answer", which the workload retries.
func (s *Server) upstreamError(w http.ResponseWriter, rpc string, err error) {
	switch status.Code(err) {
	case codes.Unimplemented:
		writeError(w, http.StatusNotImplemented, "unsupported", "the control plane does not implement "+rpc)
	case codes.InvalidArgument:
		writeError(w, http.StatusBadRequest, "invalid_request", status.Convert(err).Message())
	default:
		s.log.Debug(rpc+" failed", "error", err)
		writeError(w, http.StatusServiceUnavailable, "unavailable",
			"the control plane is unreachable; retry: "+status.Convert(err).Message())
	}
}

// refused maps an accepted=false answer. A stale lease is final for the whole
// attempt, so it is remembered.
func (s *Server) refused(w http.ResponseWriter, rpc, reason string) {
	switch reason {
	case reasonStaleLease:
		s.stale.Store(true)
		writeError(w, http.StatusConflict, "stale_lease", "this attempt no longer owns its lease; stop")
	case reasonStaleCheckpoint:
		writeError(w, http.StatusConflict, "stale_checkpoint", "a newer checkpoint is already registered")
	case reasonLeaseUnverified, "":
		writeError(w, http.StatusServiceUnavailable, "unavailable", rpc+" could not be verified; retry")
	default:
		writeError(w, http.StatusBadRequest, "rejected", rpc+" refused: "+reason)
	}
}
