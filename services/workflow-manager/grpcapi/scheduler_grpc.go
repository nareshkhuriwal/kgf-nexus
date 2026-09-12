// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package grpcapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/khuriwalgroup/kgf-nexus/internal/config"
	"github.com/khuriwalgroup/kgf-nexus/internal/transport"
	"log/slog"
	"strings"

	"google.golang.org/grpc"
)

// The scheduler's attempt-report half is served over a hand-written JSON codec
// rather than generated stubs: the frozen nexus.v1 proto declares no
// CancelRunAttempts RPC, and workflow-manager may not write task_attempts itself
// (ADR-0011), so an RPC the proto does not yet describe is the only honest
// option. These types mirror services/scheduler/reports_grpc.go field for field
// -- they are one wire contract written twice, and they must be changed together.

type schedCancelRunAttemptsRequest struct {
	RunID string `json:"run_id"`
}

type schedCancelRunAttemptsResponse struct {
	Cancelled int `json:"cancelled"`
}

// schedJSONCodec encodes with encoding/json, matching the server's codec.
type schedJSONCodec struct{}

func (schedJSONCodec) Marshal(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func (schedJSONCodec) Unmarshal(data []byte, v any) error {
	if len(data) == 0 {
		return nil
	}
	return json.Unmarshal(data, v)
}

// Name must match the server's codec name exactly, or content-subtype
// negotiation fails and every call errors on an unknown codec.
func (schedJSONCodec) Name() string { return "json" }

// schedConn lazily dials the scheduler.
//
// grpc.NewClient does not connect eagerly, so this is cheap and the connection
// is established on the first call. That matters at startup: workflow-manager
// must come up and serve health even when the scheduler is still starting.
type schedConn struct {
	conn *grpc.ClientConn
	log  *slog.Logger
}

func dialScheduler(addr string, log *slog.Logger) (*schedConn, error) {
	// NEXUS_SCHEDULER_ADDR may be a comma-separated list, because a replica drains
	// one queue and the GATEWAY has to offer a worker poll to each of them. This
	// call is not a poll: cancelling attempts is keyed on run_id, not on a queue,
	// so any replica can serve it and the first entry is enough. Dialing the raw
	// list would produce a name resolution failure at the first cancel.
	if i := strings.IndexByte(addr, ','); i >= 0 {
		addr = strings.TrimSpace(addr[:i])
	}
	// Insecure for Phase 1, matching every other inter-service client here.
	// ADR-0011 puts mTLS in the deployment layer.
	creds, terr := transport.DialOption()
	if terr != nil {
		return nil, fmt.Errorf("resolve TLS dial options: %w", terr)
	}
	conn, err := grpc.NewClient(config.PeerTarget(addr), append(config.PeerDialOptions(), creds)...)
	if err != nil {
		return nil, fmt.Errorf("dial scheduler at %s: %w", addr, err)
	}
	return &schedConn{conn: conn, log: log}, nil
}

func (s *schedConn) invoke(ctx context.Context, method string, req, resp any) error {
	return s.conn.Invoke(ctx, method, req, resp, grpc.ForceCodec(schedJSONCodec{}))
}
