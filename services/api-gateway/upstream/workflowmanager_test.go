// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package upstream

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/encoding"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
	"github.com/khuriwalgroup/kgf-nexus/internal/store"
	"github.com/khuriwalgroup/kgf-nexus/services/api-gateway/rest"
)

type specServiceAPI interface{}

// fakeSpecService answers GetSpec with a fixed error, as the workflow-manager
// does for a name that resolves to nothing in the namespace.
func fakeSpecService(err error) *grpc.ServiceDesc {
	return &grpc.ServiceDesc{
		ServiceName: "nexus.v1.WorkflowSpecService",
		HandlerType: (*specServiceAPI)(nil),
		Methods: []grpc.MethodDesc{{
			MethodName: "GetSpec",
			Handler: func(_ any, _ context.Context, dec func(any) error, _ grpc.UnaryServerInterceptor) (any, error) {
				var in wfmGetSpecRequest
				if derr := dec(&in); derr != nil {
					return nil, derr
				}
				return nil, err
			},
		}},
	}
}

func wfmOverBufconn(t *testing.T, getSpecErr error) *wfmGRPC {
	t.Helper()
	// The server side of the JSON codec the gateway forces per call. Registered
	// only in this test binary, where no protobuf peer shares the registry.
	encoding.RegisterCodec(wfmJSONCodec{})
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	srv.RegisterService(fakeSpecService(getSpecErr), struct{}{})
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return &wfmGRPC{conn: conn, runs: nexusv1.NewWorkflowManagerServiceClient(conn),
		log: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

// TestCreateRunWithAnUnknownSpecNameIsNotFound: POST /runs with a spec_name
// that names nothing used to answer 500, because the NotFound from resolving
// the name was wrapped without re-attaching store.ErrNotFound, which the REST
// layer maps to 404 spec_not_found. InvalidArgument likewise becomes 422.
func TestCreateRunWithAnUnknownSpecNameIsNotFound(t *testing.T) {
	cases := []struct {
		err  error
		want error
	}{
		{status.Error(codes.NotFound, "get spec: not found"), store.ErrNotFound},
		{status.Error(codes.InvalidArgument, "either spec_id or spec_name is required"), store.ErrInvalidRequest},
	}
	for _, c := range cases {
		w := wfmOverBufconn(t, c.err)
		_, _, err := w.createRun(context.Background(), rest.CreateRunRequest{Namespace: "ns", SpecName: "nope"})
		if !errors.Is(err, c.want) {
			t.Errorf("GetSpec %v: createRun error = %v, want it to wrap %v", c.err, err, c.want)
		}
	}
}
