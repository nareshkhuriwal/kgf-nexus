// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package grpcapi

import (
	"context"
	"errors"
	"fmt"
	"net"
	"reflect"
	"sync"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/encoding"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
	"github.com/khuriwalgroup/kgf-nexus/internal/store"
	"github.com/khuriwalgroup/kgf-nexus/services/workflow-manager/runs"
)

// F100: the node selection and the actor must survive every hop, and each of the
// engine's refusals must come out the other side as the error that tells the
// run service whether anything changed.

// retryRuns is a RunService that records retries. Only Retry is exercised; the
// embedded nil interface makes any other call fail loudly.
type retryRuns struct {
	RunService
	mu   sync.Mutex
	got  []runs.RetryRequest
	resp runs.RetryResult
	err  error
}

func (r *retryRuns) Retry(_ context.Context, req runs.RetryRequest) (runs.RetryResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.got = append(r.got, req)
	return r.resp, r.err
}

func TestRetryRunPassesTheSelectionAndActorThrough(t *testing.T) {
	fake := &retryRuns{resp: runs.RetryResult{Run: store.RunRow{ID: "r1", State: "RUNNING"}}}
	srv := &RunServer{runs: fake, log: quietLogger()}

	resp, err := srv.RetryRun(context.Background(), &nexusv1.RetryRunRequest{
		RunId: "r1", NodeIds: []string{"b", "c"}, Actor: "user:alice",
	})
	if err != nil {
		t.Fatalf("RetryRun: %v", err)
	}
	if !resp.GetAccepted() || resp.GetState() != nexusv1.RunState_RUN_STATE_RUNNING {
		t.Errorf("response = %v", resp)
	}
	want := runs.RetryRequest{RunID: "r1", NodeIDs: []string{"b", "c"}, Actor: "user:alice"}
	if len(fake.got) != 1 || !reflect.DeepEqual(fake.got[0], want) {
		t.Errorf("run service received %+v, want %+v", fake.got, want)
	}
}

func TestRetryRunMapsEachRefusalToItsCode(t *testing.T) {
	cases := []struct {
		err  error
		want codes.Code
	}{
		{fmt.Errorf("%w: FAILED -> x", store.ErrRunStateConflict), codes.FailedPrecondition},
		{fmt.Errorf("%w: node b is SUCCEEDED", runs.ErrRetryRefused), codes.FailedPrecondition},
		{fmt.Errorf("%w: nope", runs.ErrRetryInvalidNodes), codes.InvalidArgument},
		{fmt.Errorf("%w: b moved", runs.ErrRetryConflict), codes.Aborted},
		{fmt.Errorf("%w: old engine", runs.ErrRetryUnavailable), codes.Unimplemented},
		{fmt.Errorf("%w: timeout", runs.ErrRetryIndeterminate), codes.Unavailable},
		{store.ErrNotFound, codes.NotFound},
		{errors.New("disk on fire"), codes.Internal},
	}
	for _, c := range cases {
		srv := &RunServer{runs: &retryRuns{err: c.err}, log: quietLogger()}
		_, err := srv.RetryRun(context.Background(), &nexusv1.RetryRunRequest{RunId: "r1"})
		if got := status.Code(err); got != c.want {
			t.Errorf("%v: code %v, want %v", c.err, got, c.want)
		}
	}
}

// ── DAGEngineClient.ResetNodes against an in-process engine ──────────────────

type fakeResetServer struct {
	mu   sync.Mutex
	got  []deResetNodesRequest
	resp *deResetNodesResponse
	err  error
}

func (f *fakeResetServer) handle(_ any, ctx context.Context, dec func(any) error, _ grpc.UnaryServerInterceptor) (any, error) {
	in := new(deResetNodesRequest)
	if err := dec(in); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.got = append(f.got, *in)
	if f.err != nil {
		return nil, f.err
	}
	return f.resp, nil
}

var registerJSONOnce sync.Once

// resetClient serves fake over bufconn and returns a DAGEngineClient wired to it.
func resetClient(t *testing.T, fake *fakeResetServer) *DAGEngineClient {
	t.Helper()
	// The server needs a codec named "json" to decode the client's content
	// subtype; dag-engine registers the same one at startup.
	registerJSONOnce.Do(func() { encoding.RegisterCodec(deJSONCodec{}) })

	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	srv.RegisterService(&grpc.ServiceDesc{
		ServiceName: "nexus.v1.DagEngineService",
		HandlerType: (*any)(nil),
		Methods:     []grpc.MethodDesc{{MethodName: "ResetNodes", Handler: fake.handle}},
	}, struct{}{})
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial bufconn: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return &DAGEngineClient{addr: "bufnet", log: quietLogger(), conn: &deConn{conn: conn, log: quietLogger()}}
}

func TestResetNodesSendsTheSelection(t *testing.T) {
	fake := &fakeResetServer{resp: &deResetNodesResponse{RunID: "r1", Selected: []string{"b"}, Reset: []string{"b", "c"}}}
	c := resetClient(t, fake)

	selected, reset, err := c.ResetNodes(context.Background(), "r1", []string{"b"}, "user:alice")
	if err != nil {
		t.Fatalf("ResetNodes: %v", err)
	}
	if !reflect.DeepEqual(selected, []string{"b"}) || !reflect.DeepEqual(reset, []string{"b", "c"}) {
		t.Errorf("selected/reset = %v/%v", selected, reset)
	}
	want := deResetNodesRequest{RunID: "r1", NodeIDs: []string{"b"}, Actor: "user:alice"}
	if len(fake.got) != 1 || !reflect.DeepEqual(fake.got[0], want) {
		t.Errorf("engine received %+v, want %+v", fake.got, want)
	}
}

func TestResetNodesTranslatesTheEnginesAnswers(t *testing.T) {
	cases := []struct {
		code codes.Code
		want error
	}{
		{codes.InvalidArgument, runs.ErrRetryInvalidNodes},
		{codes.FailedPrecondition, runs.ErrRetryRefused},
		{codes.NotFound, runs.ErrRetryRefused},
		{codes.Aborted, runs.ErrRetryConflict},
		{codes.Unimplemented, runs.ErrRetryUnavailable},
	}
	for _, c := range cases {
		fake := &fakeResetServer{err: status.Error(c.code, "engine says no")}
		_, _, err := resetClient(t, fake).ResetNodes(context.Background(), "r1", []string{"b"}, "")
		if !errors.Is(err, c.want) {
			t.Errorf("%v: err = %v, want %v", c.code, err, c.want)
		}
	}

	// Anything else is indeterminate: none of the definite sentinels, so the
	// run service commits and reconciles rather than rolling back.
	fake := &fakeResetServer{err: status.Error(codes.Unavailable, "connection reset")}
	_, _, err := resetClient(t, fake).ResetNodes(context.Background(), "r1", nil, "")
	for _, definite := range []error{runs.ErrRetryRefused, runs.ErrRetryInvalidNodes, runs.ErrRetryConflict, runs.ErrRetryUnavailable} {
		if errors.Is(err, definite) {
			t.Errorf("Unavailable was reported as the definite %v", definite)
		}
	}
	if err == nil {
		t.Error("Unavailable was reported as success")
	}
}

func TestResetNodesWithNoEngineIsUnavailable(t *testing.T) {
	c := &DAGEngineClient{log: quietLogger()}
	if _, _, err := c.ResetNodes(context.Background(), "r1", nil, ""); !errors.Is(err, runs.ErrRetryUnavailable) {
		t.Errorf("err = %v, want ErrRetryUnavailable", err)
	}
}
