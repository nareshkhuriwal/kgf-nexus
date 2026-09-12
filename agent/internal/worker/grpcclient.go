package worker

import (
	"context"
	"fmt"

	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
)

// GRPCClient adapts the generated WorkerService client to the agent's Client
// interface, attaching the session token to every call.
type GRPCClient struct {
	conn *grpc.ClientConn
	c    nexusv1.WorkerServiceClient

	// sessionToken is set after Register. Every subsequent RPC carries it so the
	// control plane can reject calls from a session it has already invalidated —
	// the per-session identity rule from §5.5.
	sessionToken string
}

// DialControlPlane connects to the worker gRPC endpoint.
//
// Insecure by default is a DEVELOPMENT posture: the worker RPC surface carries
// lease tokens and session tokens, which are bearer credentials, so a production
// deployment must terminate TLS. This is flagged here rather than silently
// defaulted because the failure mode is invisible.
func DialControlPlane(ctx context.Context, target string, opts ...grpc.DialOption) (*GRPCClient, error) {
	if len(opts) == 0 {
		opts = []grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}
	}
	conn, err := grpc.NewClient(target, opts...)
	if err != nil {
		return nil, fmt.Errorf("dial control plane %s: %w", target, err)
	}
	return &GRPCClient{conn: conn, c: nexusv1.NewWorkerServiceClient(conn)}, nil
}

// Close releases the connection.
func (g *GRPCClient) Close() error { return g.conn.Close() }

func (g *GRPCClient) auth(ctx context.Context) context.Context {
	if g.sessionToken == "" {
		return ctx
	}
	return metadata.AppendToOutgoingContext(ctx, "x-nexus-session-token", g.sessionToken)
}

func (g *GRPCClient) Register(ctx context.Context, req *nexusv1.RegisterRequest) (*nexusv1.RegisterResponse, error) {
	resp, err := g.c.Register(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("Register: %w", err)
	}
	g.sessionToken = resp.GetSessionToken()
	return resp, nil
}

func (g *GRPCClient) Heartbeat(ctx context.Context, req *nexusv1.HeartbeatRequest) (*nexusv1.HeartbeatResponse, error) {
	resp, err := g.c.Heartbeat(g.auth(ctx), req)
	if err != nil {
		return nil, fmt.Errorf("Heartbeat: %w", err)
	}
	return resp, nil
}

func (g *GRPCClient) PollTask(ctx context.Context, req *nexusv1.PollTaskRequest) (*nexusv1.PollTaskResponse, error) {
	resp, err := g.c.PollTask(g.auth(ctx), req)
	if err != nil {
		return nil, fmt.Errorf("PollTask: %w", err)
	}
	return resp, nil
}

func (g *GRPCClient) ReportStarted(ctx context.Context, req *nexusv1.ReportStartedRequest) (*nexusv1.Ack, error) {
	resp, err := g.c.ReportStarted(g.auth(ctx), req)
	if err != nil {
		return nil, fmt.Errorf("ReportStarted: %w", err)
	}
	return resp, nil
}

func (g *GRPCClient) ReportProgress(ctx context.Context, req *nexusv1.ReportProgressRequest) (*nexusv1.Ack, error) {
	resp, err := g.c.ReportProgress(g.auth(ctx), req)
	if err != nil {
		return nil, fmt.Errorf("ReportProgress: %w", err)
	}
	return resp, nil
}

func (g *GRPCClient) ReportResult(ctx context.Context, req *nexusv1.ReportResultRequest) (*nexusv1.Ack, error) {
	resp, err := g.c.ReportResult(g.auth(ctx), req)
	if err != nil {
		return nil, fmt.Errorf("ReportResult: %w", err)
	}
	return resp, nil
}

func (g *GRPCClient) ExtendLease(ctx context.Context, req *nexusv1.ExtendLeaseRequest) (*nexusv1.ExtendLeaseResponse, error) {
	resp, err := g.c.ExtendLease(g.auth(ctx), req)
	if err != nil {
		return nil, fmt.Errorf("ExtendLease: %w", err)
	}
	return resp, nil
}
