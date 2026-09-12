package grpcapi

import (
	"context"
	"log/slog"

	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"

	"github.com/khuriwalgroup/kgf-nexus/internal/store"
	"github.com/khuriwalgroup/kgf-nexus/services/workflow-manager/runs"
)

// RunService is the workflow-manager's inbound API, as its callers need it.
//
// Declared as an interface here rather than taking *runs.Service directly so the
// transport layer can be tested against a fake and so the API's surface is stated
// in one place, separate from the lifecycle implementation.
type RunService interface {
	Create(ctx context.Context, req runs.CreateRequest) (runs.CreateResult, error)
	Start(ctx context.Context, runID string) (store.RunRow, error)
	Pause(ctx context.Context, runID string) (store.RunRow, error)
	Resume(ctx context.Context, runID string) (store.RunRow, error)
	Cancel(ctx context.Context, runID string) (store.RunRow, error)
	RetryFromFailed(ctx context.Context, runID string) (store.RunRow, error)
	OnNodesTerminal(ctx context.Context, runID string) (store.RunRow, error)
	OnNodeFailedFailFast(ctx context.Context, runID, nodeID string) (store.RunRow, error)
	OnRunDeadline(ctx context.Context, runID string) error
	MaybeFinalizePause(ctx context.Context, runID string) (store.RunRow, error)
	MaybeFinalizeCancel(ctx context.Context, runID string) (store.RunRow, error)
}

// Server is the workflow-manager's gRPC service.
//
// CONTRACT GAP, reported rather than worked around: proto/nexus/v1 defines only
// WorkerService. There is no WorkflowManagerService message set, so the typed
// request/response types for the RPCs below do not exist and cannot be generated
// without editing the frozen protos (CONTRACT.md forbids that). See
// CONTRACT-FINDINGS.md.
//
// Until those protos land, this listener registers the standard gRPC health service
// so the process is a genuine, independently deployable gRPC service that a load
// balancer and an orchestrator can probe, and the RunService above is the exact
// surface the generated stubs will delegate to — so adding them is a transport
// change only, with no lifecycle logic to rewrite.
type Server struct {
	runs RunService
	log  *slog.Logger
}

// Register wires the workflow-manager's services onto a gRPC server.
func Register(srv *grpc.Server, runSvc RunService, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	s := &Server{runs: runSvc, log: log}

	// The standard health service, so readiness is probeable over gRPC as well as
	// HTTP. An orchestrator configured for gRPC probes should not have to fall back
	// to the HTTP port to find out whether this replica is serving.
	hs := health.NewServer()
	hs.SetServingStatus("nexus.v1.WorkflowManager", healthpb.HealthCheckResponse_SERVING)
	healthpb.RegisterHealthServer(srv, hs)

	log.Info("workflow-manager gRPC registered",
		"note", "typed run RPCs await a WorkflowManagerService definition in proto/nexus/v1")
	return s
}

// Runs exposes the lifecycle service to the transport layer once typed stubs exist.
func (s *Server) Runs() RunService { return s.runs }
