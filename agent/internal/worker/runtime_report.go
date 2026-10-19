// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package worker

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/khuriwalgroup/kgf-nexus/agent/internal/executor"
	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
)

// Attempt placement reporting (ADR-0037): where each attempt's workload runs,
// sent to the control plane through WorkerService.ReportPlacement so the
// console's task-runner view can name the pod and the node.
//
//   - kubernetes: the attempt's own pod. The executor calls TaskSpec.OnPlacement
//     when the pod appears and again when it is scheduled onto a node.
//   - subprocess and docker: the workload runs beside the agent, so the
//     placement is the agent's own pod, namespace and node (NEXUS_POD_NAME,
//     NEXUS_POD_NAMESPACE, NEXUS_NODE_NAME), reported once the workload has
//     started. A docker agent outside Kubernetes has no pod; it reports the
//     container's short id instead, the handle an operator can `docker
//     inspect` on the worker host.
//
// Advisory throughout: a report that cannot be delivered leaves the attempt
// without a recorded placement and changes nothing else about it.

// PlacementClient is the ReportPlacement RPC. Optional on a Client, like
// logship.Indexer: placement is reported only when the client speaks it AND
// the control plane advertised placement_reporting at registration.
type PlacementClient interface {
	ReportPlacement(ctx context.Context, req *nexusv1.ReportPlacementRequest) (*nexusv1.Ack, error)
}

const (
	// placementTries bounds delivery of one placement. Few: the report is
	// advisory, and the next change of placement sends a fresh one.
	placementTries   = 3
	placementBackoff = time.Second
	placementTimeout = 10 * time.Second
	// dockerKind is executor.DockerExecutor's Kind.
	dockerKind = "docker"
)

// placementReporter delivers one attempt's placement from its own goroutine,
// latest first: the executor's wait loop must never block on an RPC, and a
// placement superseded while an earlier one is still being retried is simply
// replaced, so the control plane ends with the newest one.
type placementReporter struct {
	r       *Runner
	a       *nexusv1.TaskAssignment
	log     *slog.Logger
	backoff time.Duration

	mu      sync.Mutex
	pending *nexusv1.AttemptPlacement
	wake    chan struct{}
}

// startPlacementReporter returns the attempt's reporter, running until ctx
// ends, or nil when this session reports no placement.
func (r *Runner) startPlacementReporter(ctx context.Context, a *nexusv1.TaskAssignment, log *slog.Logger) *placementReporter {
	if r.Placement == nil || r.placementOff.Load() {
		return nil
	}
	p := &placementReporter{r: r, a: a, log: log, backoff: placementBackoff, wake: make(chan struct{}, 1)}
	go p.loop(ctx)
	return p
}

// set makes pl the placement to report. Never blocks; nil-safe.
func (p *placementReporter) set(pl *nexusv1.AttemptPlacement) {
	if p == nil || pl == nil {
		return
	}
	p.mu.Lock()
	p.pending = pl
	p.mu.Unlock()
	select {
	case p.wake <- struct{}{}:
	default: // a wake-up is already pending and will read the newest value
	}
}

func (p *placementReporter) latest() *nexusv1.AttemptPlacement {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.pending
}

func (p *placementReporter) loop(ctx context.Context) {
	var delivered *nexusv1.AttemptPlacement
	for {
		select {
		case <-ctx.Done():
			return
		case <-p.wake:
		}
		pl := p.latest()
		if pl == nil || proto.Equal(pl, delivered) {
			continue
		}
		if p.send(ctx, pl) {
			delivered = pl
		}
	}
}

// send delivers pl, retrying a transport failure a few times. It reports true
// once the control plane has ANSWERED -- accepted, or refused, which resending
// cannot change -- and false when it never got through or pl was superseded
// while retrying.
func (p *placementReporter) send(ctx context.Context, pl *nexusv1.AttemptPlacement) bool {
	backoff := p.backoff
	var lastErr error
	for try := 1; try <= placementTries; try++ {
		rctx, cancel := context.WithTimeout(ctx, placementTimeout)
		ack, err := p.r.Placement.ReportPlacement(rctx, &nexusv1.ReportPlacementRequest{
			AttemptId: p.a.GetAttemptId(), LeaseToken: p.r.currentToken(p.a), Placement: pl,
		})
		cancel()
		switch {
		case err == nil && ack.GetAccepted():
			p.log.Debug("attempt placement reported", "executor", pl.GetExecutor(),
				"pod", pl.GetPod(), "k8s_node", pl.GetK8SNode())
			return true
		case err == nil:
			p.log.Warn("attempt placement refused by the control plane", "reason", ack.GetReason())
			return true
		case status.Code(err) == codes.Unimplemented:
			// A gateway that stopped offering it (rolled back mid-session):
			// stop asking for the rest of the session.
			if p.r.placementOff.CompareAndSwap(false, true) {
				p.log.Info("the control plane does not record attempt placement; not reporting it this session")
			}
			return true
		}
		lastErr = err
		if ctx.Err() != nil {
			return false
		}
		if !proto.Equal(p.latest(), pl) {
			return false // superseded: the loop sends the newer one
		}
		if try < placementTries {
			select {
			case <-ctx.Done():
				return false
			case <-time.After(backoff):
			}
			backoff *= 2
		}
	}
	p.log.Warn("could not report the attempt's placement; it is shown without one", "error", lastErr)
	return false
}

// podPlacement is a kubernetes attempt's placement: its own pod.
func (r *Runner) podPlacement(kind string, pp executor.PodPlacement) *nexusv1.AttemptPlacement {
	return &nexusv1.AttemptPlacement{
		Executor: kind, Cluster: r.Runtime.Cluster,
		K8SNamespace: pp.Namespace, Pod: pp.Pod, K8SNode: pp.Node,
	}
}

// localPlacement is the placement of a workload running beside the agent: the
// agent's own pod and node, or, for a docker agent outside Kubernetes, the
// container.
func (r *Runner) localPlacement(kind string, h executor.Handle) *nexusv1.AttemptPlacement {
	pl := &nexusv1.AttemptPlacement{
		Executor: kind, Cluster: r.Runtime.Cluster,
		K8SNamespace: r.Runtime.PodNamespace, Pod: r.Runtime.Pod, K8SNode: r.Runtime.Node,
	}
	if kind == dockerKind && pl.GetPod() == "" {
		pl.Pod = shortContainerID(h.ID)
	}
	return pl
}

// reportsOwnPlacement reports whether exec delivers placement through
// TaskSpec.OnPlacement itself.
func reportsOwnPlacement(exec executor.Executor) bool {
	src, ok := exec.(executor.PlacementSource)
	return ok && src.ReportsPlacement()
}

// shortContainerID is the 12-character id `docker ps` shows.
func shortContainerID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}
