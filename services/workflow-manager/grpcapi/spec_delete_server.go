// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package grpcapi

import (
	"context"
	"errors"

	"github.com/khuriwalgroup/kgf-nexus/internal/store"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Workflow-spec DELETION.
//
// WHY THIS IS AN RPC AT ALL. The gateway reads workflow_specs directly -- ADR-0011
// lets it read every table, which is how the catalog and the spec definition are
// served. But workflow_specs is workflow-manager's table, so a DELETE has to come
// through the owner or migration 0005's grants refuse it at the database. That
// refusal is the point: it is what makes ownership real rather than a convention.
//
// Added to the existing hand-written WorkflowSpecService rather than given its own
// service, for the same reason the schedule RPCs were: same owner, same table
// group, and a second descriptor would be a second thing to replace when these
// become generated stubs.

// maxSpecVersionsCounted bounds the version count reported on a successful
// delete. A workflow with more versions than this is vanishingly unlikely, and
// under-reporting a count in a log line is a far smaller cost than letting one
// request scan an unbounded number of rows.
const maxSpecVersionsCounted = 10000

// DeleteSpecRequest removes every version of a workflow NAME.
//
// By name rather than by spec id, because versions of one workflow are not
// independently deletable: removing v3 of five would leave a catalog entry whose
// "latest" silently rolls back to v2, which is a worse surprise than refusing.
type DeleteSpecRequest struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

// DeleteSpecResponse carries the refusal detail.
//
// Not empty like DeleteScheduleResponse, because this RPC's interesting outcome is
// the REFUSAL, and gRPC status messages are a string. Returning the blocker as
// structured fields lets the gateway render "3 runs and the schedule 'nightly'
// still reference this" without parsing an error message -- and message text is not
// a stable contract across a version skew.
//
// Populated ONLY on the FailedPrecondition path; a success returns the zero value.
type DeleteSpecResponse struct {
	// Deleted is how many spec VERSIONS were removed, so the caller can report
	// "deleted 5 versions" rather than a bare success.
	Deleted int `json:"deleted"`
	// ReferencingRuns and ReferencingSchedules are the named blockers. See
	// store.ErrSpecReferenced for why naming rather than counting matters.
	ReferencingRuns      int      `json:"referencing_runs,omitempty"`
	ReferencingSchedules []string `json:"referencing_schedules,omitempty"`
}

// DeleteSpec removes a workflow's spec versions, or refuses with named blockers.
//
// The referential-integrity decision lives in store.WFMDeleteSpec, which explains
// at length why this refuses rather than cascading or soft-deleting. This layer's
// job is the classification: a refusal is FailedPrecondition, NOT Internal, because
// the caller can do something about it and a 500 would tell them to file a bug
// instead of deleting their schedule.
func (s *SpecServer) DeleteSpec(ctx context.Context, req *DeleteSpecRequest) (*DeleteSpecResponse, error) {
	if req.Namespace == "" || req.Name == "" {
		return nil, status.Error(codes.InvalidArgument, "namespace and name are required")
	}

	// Counted BEFORE the delete rather than after, because after a successful
	// delete the rows are gone and the count is unrecoverable. The number is what
	// makes the success message honest: "deleted" with no quantity leaves an
	// operator unsure whether they removed one version or twelve.
	//
	// maxSpecVersionsCounted rather than 0: that helper puts its limit straight
	// into SQL, and LIMIT 0 would return an empty slice -- reporting "deleted 0
	// versions" for a delete that removed twelve.
	versions, verr := store.GatewayListSpecVersions(ctx, s.db, req.Namespace, req.Name, maxSpecVersionsCounted)
	if verr != nil {
		return nil, status.Errorf(codes.Internal, "count spec versions: %v", verr)
	}

	err := store.WFMDeleteSpec(ctx, s.db, req.Namespace, req.Name)

	var refErr *store.ErrSpecReferenced
	if errors.As(err, &refErr) {
		// The detail travels in the RESPONSE, not only the status message, so the
		// gateway can render it structurally. gRPC delivers no response body
		// alongside a non-OK status, so the status message carries a human summary
		// and the gateway re-derives the structure -- which is why the gateway
		// counts references itself before calling. See the gateway handler.
		return nil, status.Errorf(codes.FailedPrecondition, "%s", refErr.Error())
	}
	if errors.Is(err, store.ErrNotFound) {
		return nil, status.Errorf(codes.NotFound, "no workflow named %q in namespace %s", req.Name, req.Namespace)
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "delete spec: %v", err)
	}

	// WARN, not INFO: this destroys a workflow DEFINITION. It is only reachable
	// when nothing references it, so nothing is orphaned -- but the document
	// itself is unrecoverable, and that is worth finding in a log later.
	s.log.WarnContext(ctx, "workflow spec deleted; its definition is unrecoverable",
		"namespace", req.Namespace, "name", req.Name, "versions", len(versions))
	return &DeleteSpecResponse{Deleted: len(versions)}, nil
}

func deleteSpecHandler(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
	in := new(DeleteSpecRequest)
	if err := dec(in); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return srv.(specService).DeleteSpec(ctx, in)
	}
	return interceptor(ctx, in, &grpc.UnaryServerInfo{
		Server: srv, FullMethod: "/nexus.v1.WorkflowSpecService/DeleteSpec",
	}, func(ctx context.Context, req any) (any, error) {
		return srv.(specService).DeleteSpec(ctx, req.(*DeleteSpecRequest))
	})
}
