// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package taskapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"

	"github.com/khuriwalgroup/kgf-nexus/agent/internal/resultstore"
	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
)

// CheckpointKey is where an attempt's seq-th checkpoint object lives
// (ADR-0032 §4). Every checkpoint is a NEW object: the seq makes the key
// unique, so a zombie's late upload can never overwrite newer bytes.
func CheckpointKey(namespace, idempotencyKey, attemptID string, seq int64) string {
	return fmt.Sprintf("%s/checkpoints/%s/%s/%d", namespace, idempotencyKey, attemptID, seq)
}

// handleCheckpointPut uploads the body, THEN registers it. The order is the
// one artifact GC's safety argument needs: an object may exist that no pointer
// references (an orphan, swept after the grace period), never a pointer to an
// object that does not exist yet.
//
// Checkpoints are serialized per attempt. That bounds the agent's memory to
// one checkpoint per attempt, and it keeps registration in seq order: were two
// saves to race, the later seq could register first and the earlier one would
// then be refused as stale, surprising the workload for nothing.
func (s *Server) handleCheckpointPut(w http.ResponseWriter, r *http.Request) {
	if !s.caps.Checkpoints {
		writeError(w, http.StatusNotImplemented, "unsupported",
			"checkpoints are not available to this attempt (no result store, or not offered by the control plane)")
		return
	}
	if r.ContentLength > s.maxCheckpoint {
		s.cfg.Metrics.Checkpoint("too_large")
		writeError(w, http.StatusRequestEntityTooLarge, "too_large",
			fmt.Sprintf("checkpoint is %d bytes; the limit is %d", r.ContentLength, s.maxCheckpoint))
		return
	}
	s.ckptMu.Lock()
	defer s.ckptMu.Unlock()

	body, err := io.ReadAll(io.LimitReader(r.Body, s.maxCheckpoint+1))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "could not read the checkpoint body: "+err.Error())
		return
	}
	if int64(len(body)) > s.maxCheckpoint {
		s.cfg.Metrics.Checkpoint("too_large")
		writeError(w, http.StatusRequestEntityTooLarge, "too_large",
			fmt.Sprintf("checkpoint exceeds the limit of %d bytes", s.maxCheckpoint))
		return
	}

	// The seq is consumed even if the upload fails: gaps are harmless, reuse
	// is not (a retried save must never reuse a key an earlier, possibly
	// partially visible upload already wrote).
	s.ckptSeq++
	seq := s.ckptSeq
	key := CheckpointKey(s.cfg.Namespace, s.cfg.IdempotencyKey, s.cfg.AttemptID, seq)
	sum := sha256.Sum256(body)
	digest := hex.EncodeToString(sum[:])
	size := int64(len(body))

	uctx, cancel := context.WithTimeout(r.Context(), storeTimeout)
	err = s.cfg.Store.PutCheckpointObject(uctx, key, bytes.NewReader(body), size)
	cancel()
	if err != nil {
		s.cfg.Metrics.Checkpoint("error")
		s.log.Warn("checkpoint upload failed", "key", key, "size_bytes", size, "error", err)
		writeError(w, http.StatusServiceUnavailable, "unavailable", "the result store is unavailable; retry")
		return
	}

	rctx, rcancel := context.WithTimeout(r.Context(), s.cfg.RPCTimeout)
	ack, err := s.cfg.Upstream.SaveCheckpoint(rctx, &nexusv1.SaveCheckpointRequest{
		AttemptId: s.cfg.AttemptID, LeaseToken: s.cfg.LeaseToken(),
		Seq: seq, Key: key, Sha256: digest, SizeBytes: size,
	})
	rcancel()
	if err != nil {
		s.cfg.Metrics.Checkpoint("error")
		s.upstreamError(w, "SaveCheckpoint", err)
		return
	}
	if !ack.GetAccepted() {
		switch ack.GetReason() {
		case reasonStaleLease, reasonStaleCheckpoint:
			s.cfg.Metrics.Checkpoint("stale")
		default:
			s.cfg.Metrics.Checkpoint("error")
		}
		s.log.Info("checkpoint registration refused; the uploaded object is an orphan for GC",
			"key", key, "reason", ack.GetReason())
		s.refused(w, "SaveCheckpoint", ack.GetReason())
		return
	}
	s.cfg.Metrics.Checkpoint("saved")
	s.log.Debug("checkpoint saved", "seq", seq, "key", key, "size_bytes", size)
	writeJSON(w, http.StatusOK, map[string]any{
		"seq": seq, "key": key, "sha256": digest, "size_bytes": size,
	})
}

// handleCheckpointGet returns the latest checkpoint for the attempt's
// idempotency key. A pointer whose object is missing or does not match its
// digest is answered 404 checkpoint_unavailable, not 5xx: resuming from
// scratch is always correct, while resuming from corrupt state is not.
func (s *Server) handleCheckpointGet(w http.ResponseWriter, r *http.Request) {
	if !s.caps.Checkpoints {
		writeError(w, http.StatusNotImplemented, "unsupported",
			"checkpoints are not available to this attempt (no result store, or not offered by the control plane)")
		return
	}
	s.ckptMu.Lock()
	defer s.ckptMu.Unlock()

	rctx, cancel := context.WithTimeout(r.Context(), s.cfg.RPCTimeout)
	resp, err := s.cfg.Upstream.GetCheckpoint(rctx, &nexusv1.GetCheckpointRequest{
		AttemptId: s.cfg.AttemptID, LeaseToken: s.cfg.LeaseToken(),
	})
	cancel()
	if err != nil {
		s.upstreamError(w, "GetCheckpoint", err)
		return
	}
	if !resp.GetAccepted() {
		s.refused(w, "GetCheckpoint", resp.GetReason())
		return
	}
	if !resp.GetFound() {
		writeError(w, http.StatusNotFound, "not_found", "no checkpoint has been saved for this task")
		return
	}
	log := s.log.With("key", resp.GetKey(), "seq", resp.GetSeq(), "saved_by_attempt", resp.GetAttemptNumber())
	unavailable := func(why string, args ...any) {
		log.Warn("checkpoint unavailable; the task will start from scratch: "+why, args...)
		writeError(w, http.StatusNotFound, "checkpoint_unavailable",
			"the latest checkpoint could not be read back ("+why+"); start from scratch")
	}
	if resp.GetSizeBytes() > s.maxCheckpoint {
		unavailable("larger than this agent's checkpoint limit", "size_bytes", resp.GetSizeBytes(), "limit", s.maxCheckpoint)
		return
	}

	gctx, gcancel := context.WithTimeout(r.Context(), storeTimeout)
	b, err := s.cfg.Store.GetCheckpointObject(gctx, resp.GetBucket(), resp.GetKey(), s.maxCheckpoint)
	gcancel()
	switch {
	case errors.Is(err, resultstore.ErrObjectNotFound), errors.Is(err, resultstore.ErrObjectTooLarge),
		errors.Is(err, resultstore.ErrCheckpointKeyOutsideNamespace):
		unavailable("object missing or unreadable", "error", err)
		return
	case err != nil:
		log.Warn("checkpoint download failed", "error", err)
		writeError(w, http.StatusServiceUnavailable, "unavailable", "the result store is unavailable; retry")
		return
	}
	sum := sha256.Sum256(b)
	if got := hex.EncodeToString(sum[:]); resp.GetSha256() == "" || got != resp.GetSha256() {
		unavailable("digest mismatch", "want_sha256", resp.GetSha256(), "got_sha256", got)
		return
	}
	if resp.GetSizeBytes() != 0 && int64(len(b)) != resp.GetSizeBytes() {
		unavailable("size mismatch", "want", resp.GetSizeBytes(), "got", len(b))
		return
	}

	h := w.Header()
	h.Set("Content-Type", "application/octet-stream")
	h.Set("Content-Length", strconv.Itoa(len(b)))
	h.Set("X-Nexus-Checkpoint-Seq", strconv.FormatInt(resp.GetSeq(), 10))
	h.Set("X-Nexus-Checkpoint-Attempt", strconv.FormatInt(int64(resp.GetAttemptNumber()), 10))
	h.Set("X-Nexus-Checkpoint-Saved-At", strconv.FormatInt(resp.GetSavedAtUnixMs(), 10))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(b)
}
