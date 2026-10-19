// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package workergrpc

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
	"github.com/khuriwalgroup/kgf-nexus/internal/store"
)

// placementDB is leaseDB plus the one write ReportPlacement makes. rows is
// what the fenced INSERT reports as affected: 1 stored, 0 the lease was lost
// between the token check and the write.
type placementDB struct {
	*leaseDB
	mu      sync.Mutex
	rows    int64
	execErr error
	writes  [][]any
}

func (d *placementDB) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if !strings.Contains(sql, "INSERT INTO attempt_runtime") {
		return pgconn.NewCommandTag(""), fmt.Errorf("placementDB: unexpected Exec: %s", sql)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.writes = append(d.writes, args)
	if d.execErr != nil {
		return pgconn.NewCommandTag(""), d.execErr
	}
	return pgconn.NewCommandTag(fmt.Sprintf("INSERT 0 %d", d.rows)), nil
}

func newPlacementServer(t *testing.T, offered bool) (*Server, *placementDB) {
	t.Helper()
	db := &placementDB{leaseDB: newLeaseDB(), rows: 1}
	db.tokens["a1"] = "tok-current"
	s := testServer(db, &fakeRM{exists: true, state: "ACTIVE"}, &fakeSched{})
	s.cfg.PlacementReporting = offered
	return s, db
}

func placementReq(token string, p *nexusv1.AttemptPlacement) *nexusv1.ReportPlacementRequest {
	return &nexusv1.ReportPlacementRequest{AttemptId: "a1", LeaseToken: token, Placement: p}
}

func TestReportPlacementNotOfferedIsUnimplemented(t *testing.T) {
	s, db := newPlacementServer(t, false)
	_, err := s.ReportPlacement(workerCtx(), placementReq("tok-current", &nexusv1.AttemptPlacement{Executor: "subprocess"}))
	if status.Code(err) != codes.Unimplemented {
		t.Fatalf("err = %v, want Unimplemented: the agent's one path for \"not recorded here\"", err)
	}
	if len(db.writes) != 0 {
		t.Fatalf("a gateway that does not offer placement wrote %d rows", len(db.writes))
	}
}

// TestReportPlacementWritesThroughTheFencedLease pins the write: it names the
// lease the token check found (its id), and the report is normalised -- the
// "k8s" alias recorded as "kubernetes", control characters and padding gone.
func TestReportPlacementWritesThroughTheFencedLease(t *testing.T) {
	s, db := newPlacementServer(t, true)
	ack, err := s.ReportPlacement(workerCtx(), placementReq("tok-current", &nexusv1.AttemptPlacement{
		Executor: " K8s ", Cluster: "aks-prod", K8SNamespace: "nexus-tasks",
		Pod: "nexus-a1-x7k2p\n", K8SNode: "aks-gpu-vmss000003",
	}))
	if err != nil || !ack.GetAccepted() {
		t.Fatalf("ack=%v err=%v, want accepted", ack, err)
	}
	if len(db.writes) != 1 {
		t.Fatalf("writes = %d, want 1", len(db.writes))
	}
	got := db.writes[0]
	want := []any{"lease-a1", "a1", "kubernetes", "aks-prod", "nexus-tasks", "nexus-a1-x7k2p", "aks-gpu-vmss000003"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("write args = %v, want %v", got, want)
	}
}

func TestReportPlacementStaleTokenWritesNothing(t *testing.T) {
	s, db := newPlacementServer(t, true)
	ack, err := s.ReportPlacement(workerCtx(), placementReq("tok-stale", &nexusv1.AttemptPlacement{Executor: "docker"}))
	if err != nil {
		t.Fatalf("a stale token must be a protocol rejection, not %v", err)
	}
	if ack.GetAccepted() || ack.GetReason() != store.ReasonStaleLeaseToken {
		t.Fatalf("ack = %v, want rejected with %q", ack, store.ReasonStaleLeaseToken)
	}
	if len(db.writes) != 0 {
		t.Fatal("a zombie's placement reached the database")
	}
}

// TestReportPlacementLeaseLostAfterTheFenceIsStale: the write's own fence
// (same lease id, unexpired, not an older epoch) found nothing to write.
func TestReportPlacementLeaseLostAfterTheFenceIsStale(t *testing.T) {
	s, db := newPlacementServer(t, true)
	db.rows = 0
	ack, err := s.ReportPlacement(workerCtx(), placementReq("tok-current", &nexusv1.AttemptPlacement{Executor: "subprocess"}))
	if err != nil {
		t.Fatal(err)
	}
	if ack.GetAccepted() || ack.GetReason() != store.ReasonStaleLeaseToken {
		t.Fatalf("ack = %v, want stale_lease_token", ack)
	}
}

func TestReportPlacementInvalidIsRefusedNotRetried(t *testing.T) {
	s, db := newPlacementServer(t, true)
	for name, p := range map[string]*nexusv1.AttemptPlacement{
		"namespace over a DNS label": {Executor: "kubernetes", K8SNamespace: strings.Repeat("n", 64)},
		"executor outside grammar":   {Executor: "Fire cracker!"},
	} {
		t.Run(name, func(t *testing.T) {
			ack, err := s.ReportPlacement(workerCtx(), placementReq("tok-current", p))
			if err != nil {
				t.Fatalf("an unstorable report is a protocol outcome, not %v", err)
			}
			if ack.GetAccepted() || ack.GetReason() != ReasonInvalidPlacement {
				t.Fatalf("ack = %v, want %q", ack, ReasonInvalidPlacement)
			}
		})
	}
	if len(db.writes) != 0 {
		t.Fatalf("an invalid report reached the database %d times", len(db.writes))
	}
}

func TestReportPlacementDatabaseFailureIsUnavailable(t *testing.T) {
	s, db := newPlacementServer(t, true)
	db.execErr = errors.New("connection reset by peer")
	_, err := s.ReportPlacement(workerCtx(), placementReq("tok-current", &nexusv1.AttemptPlacement{Executor: "subprocess"}))
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("err = %v, want Unavailable (retryable, and never a stale-lease answer)", err)
	}
}

func TestRegisterAdvertisesPlacementReporting(t *testing.T) {
	for _, offered := range []bool{false, true} {
		s, _ := newPlacementServer(t, offered)
		resp, err := s.Register(workerCtx(), &nexusv1.RegisterRequest{
			Namespace: "team-ml", Hostname: "h", Queues: []string{"q"},
			Capacity: &nexusv1.ResourceVector{CpuMillis: 1000}, AgentVersion: "1.0.0",
		})
		if err != nil {
			t.Fatal(err)
		}
		if resp.GetPlacementReporting() != offered {
			t.Errorf("placement_reporting = %v, want %v", resp.GetPlacementReporting(), offered)
		}
	}
}

func TestReportPlacementNeedsAWorkerCredential(t *testing.T) {
	s, _ := newPlacementServer(t, true)
	_, err := s.ReportPlacement(context.Background(), placementReq("tok-current", &nexusv1.AttemptPlacement{}))
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("err = %v, want Unauthenticated", err)
	}
}
