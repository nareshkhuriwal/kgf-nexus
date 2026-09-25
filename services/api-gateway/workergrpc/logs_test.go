// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package workergrpc

import (
	"strings"
	"testing"

	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
	"github.com/khuriwalgroup/kgf-nexus/internal/store"
)

func logServer(db *leaseDB, enabled bool) *Server {
	s := testServer(db, &fakeRM{exists: true, state: "ACTIVE"}, &fakeSched{})
	s.cfg.LogShipping = &nexusv1.LogShippingConfig{Enabled: enabled}
	return s
}

func logReq(token string) *nexusv1.ReportLogsRequest {
	return &nexusv1.ReportLogsRequest{
		AttemptId: "a1", LeaseToken: token,
		Chunks: []*nexusv1.LogChunk{{
			Seq: 0, Key: "team-ml/logs/a1/00000000.ndjson", FirstLine: 1, LineCount: 1,
			SizeBytes: 10, Sha256: strings.Repeat("0", 64),
		}},
	}
}

// TestReportLogsIsFenced is ADR-0020's fencing claim: a stale attempt cannot add
// to an attempt's log. The leaseDB fake answers only lease reads, so if the
// report got past the fence into the index write, the fake's "unexpected
// QueryRow"/Begin would surface as a transport error rather than an Ack.
func TestReportLogsIsFenced(t *testing.T) {
	db := newLeaseDB()
	db.tokens["a1"] = "tok-current"
	s := logServer(db, true)

	ack, err := s.ReportLogs(workerCtx(), logReq("tok-stale"))
	if err != nil {
		t.Fatalf("a stale token must be a protocol rejection, not a transport error: %v", err)
	}
	if ack.GetAccepted() || ack.GetReason() != store.ReasonStaleLeaseToken {
		t.Fatalf("ack = %+v, want rejected with %q", ack, store.ReasonStaleLeaseToken)
	}

	ack, err = s.ReportLogs(workerCtx(), logReq(""))
	if err != nil || ack.GetAccepted() {
		t.Fatalf("a missing token was not rejected: ack=%+v err=%v", ack, err)
	}
}

// TestReportLogsRefusedWhenShippingDisabled: a gateway that did not advertise
// log shipping must not index anything, even for a live lease -- it has no way
// to serve the log back, so the rows would be cost without value.
func TestReportLogsRefusedWhenShippingDisabled(t *testing.T) {
	db := newLeaseDB()
	db.tokens["a1"] = "tok-current"
	s := logServer(db, false)

	ack, err := s.ReportLogs(workerCtx(), logReq("tok-current"))
	if err != nil {
		t.Fatalf("ReportLogs: %v", err)
	}
	if ack.GetAccepted() || ack.GetReason() != "log_shipping_disabled" {
		t.Fatalf("ack = %+v, want log_shipping_disabled", ack)
	}
}

// TestRegisterAdvertisesLogShipping: the policy rides the registration response,
// so the control plane -- not each host -- decides the cap.
func TestRegisterAdvertisesLogShipping(t *testing.T) {
	s := logServer(newLeaseDB(), true)
	s.cfg.LogShipping.MaxBytesPerAttempt = 1 << 20
	resp, err := s.Register(workerCtx(), &nexusv1.RegisterRequest{
		Namespace: "team-ml", Hostname: "h", Queues: []string{"default"},
		Capacity: &nexusv1.ResourceVector{CpuMillis: 1000},
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if !resp.GetLogShipping().GetEnabled() || resp.GetLogShipping().GetMaxBytesPerAttempt() != 1<<20 {
		t.Fatalf("log_shipping = %+v, want enabled with the configured cap", resp.GetLogShipping())
	}
}

func TestReportLogsRequiresAWorkerPrincipal(t *testing.T) {
	s := logServer(newLeaseDB(), true)
	if _, err := s.ReportLogs(t.Context(), logReq("x")); err == nil {
		t.Fatal("an unauthenticated ReportLogs was served")
	}
}
