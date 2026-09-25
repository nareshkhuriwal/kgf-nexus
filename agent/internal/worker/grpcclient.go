// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package worker

import (
	"context"
	"fmt"
	"github.com/khuriwalgroup/kgf-nexus/internal/config"
	"github.com/khuriwalgroup/kgf-nexus/internal/transport"

	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

// GRPCClient adapts the generated WorkerService client to the agent's Client
// interface, attaching the session token to every call.
type GRPCClient struct {
	conn *grpc.ClientConn
	c    nexusv1.WorkerServiceClient

	// apiKey is the BOOTSTRAP credential, read from NEXUS_API_KEY. Register has no
	// session token yet -- obtaining one is the point of the call -- so without a
	// separate bootstrap credential the very first RPC is unauthenticated and the
	// gateway correctly refuses it. Every RPC carries the key; the session token
	// is layered on once Register returns it.
	apiKey string

	// sessionToken is set after Register. Every subsequent RPC carries it so the
	// control plane can reject calls from a session it has already invalidated —
	// the per-session identity rule from §5.5.
	sessionToken string
}

// WithAPIKey sets the bootstrap credential presented on every RPC, including
// Register.
func (g *GRPCClient) WithAPIKey(key string) *GRPCClient { g.apiKey = key; return g }

// DialControlPlane connects to the worker gRPC endpoint.
//
// This is the one hop that leaves the control-plane network, and every RPC over it
// carries an API key, a session token and lease tokens -- all bearer credentials,
// which a passive observer can simply reuse. So transport security matters more
// here than on any internal hop.
//
// Credentials come from the environment when the caller supplies no options (see
// internal/transport), so a deployment that configures certs gets TLS without a
// code change, and one that configures none still gets plaintext exactly as before.
// Caller-supplied options still win, which keeps the existing contract for tests
// and for an embedder that wants its own credentials.
func DialControlPlane(ctx context.Context, target string, opts ...grpc.DialOption) (*GRPCClient, error) {
	if len(opts) == 0 {
		creds, terr := transport.DialOption()
		if terr != nil {
			// Refusing to start beats connecting in the clear: an agent that
			// silently downgraded would ship its credentials unencrypted while
			// every health check stayed green.
			return nil, fmt.Errorf("resolve TLS dial options: %w", terr)
		}
		opts = append(config.PeerDialOptions(), creds)
	}
	conn, err := grpc.NewClient(config.PeerTarget(target), opts...)
	if err != nil {
		return nil, fmt.Errorf("dial control plane %s: %w", target, err)
	}
	return &GRPCClient{conn: conn, c: nexusv1.NewWorkerServiceClient(conn)}, nil
}

// Close releases the connection.
func (g *GRPCClient) Close() error { return g.conn.Close() }

func (g *GRPCClient) auth(ctx context.Context) context.Context {
	// The API key authenticates the WORKER (is this host allowed to join?); the
	// session token authenticates the SESSION (is this registration still valid?).
	// They answer different questions, so both are sent once both exist.
	if g.apiKey != "" {
		ctx = metadata.AppendToOutgoingContext(ctx, "x-api-key", g.apiKey)
	}
	if g.sessionToken == "" {
		return ctx
	}
	return metadata.AppendToOutgoingContext(ctx, "x-nexus-session-token", g.sessionToken)
}

func (g *GRPCClient) Register(ctx context.Context, req *nexusv1.RegisterRequest) (*nexusv1.RegisterResponse, error) {
	// auth() here carries the bootstrap API key only, since no session exists yet.
	resp, err := g.c.Register(g.auth(ctx), req)
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

// ReportLogs indexes log chunks already written to the result store (ADR-0020).
func (g *GRPCClient) ReportLogs(ctx context.Context, req *nexusv1.ReportLogsRequest) (*nexusv1.Ack, error) {
	resp, err := g.c.ReportLogs(g.auth(ctx), req)
	if err != nil {
		return nil, fmt.Errorf("ReportLogs: %w", err)
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
