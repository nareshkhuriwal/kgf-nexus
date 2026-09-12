package config

import (
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/backoff"
	"google.golang.org/grpc/keepalive"
)

// THE FAILURE THESE OPTIONS PREVENT.
//
// gRPC resolves a peer's address when it first connects and then keeps using the
// connection it has. Container orchestrators reassign IPs freely, and recreating
// several services together makes two of them SWAP addresses. A client holding an
// open connection to the address it remembers then reaches a DIFFERENT service,
// and every call fails with
//
//	rpc error: code = Unimplemented desc = unknown service nexus.v1.SchedulerService
//
// which reads like a version skew or a missing registration and is neither.
//
// Reproduced deterministically: recreating dag-engine, scheduler and
// workflow-manager together swapped the latter two, after which the api-gateway
// held a connection to the scheduler's address while still sending
// WorkflowSpecService over it. 43 Unimplemented errors and every run creation
// failed. All six services were healthy; only the client's idea of where each one
// lived was wrong, and nothing recovered it short of restarting the client.
//
// It cannot be fixed on the client alone. gRPC re-resolves only when a connection
// BREAKS (clientconn.go calls resolveNow from the transient-failure path), and a
// swapped peer does not break anything: the TCP session stays perfectly healthy,
// it merely terminates at the wrong process. So the server has to close
// connections periodically -- see PeerServerOptions.

// PeerDialOptions are the options every control-plane client should dial an
// internal peer with.
//
// These make recovery FAST once a connection does drop; PeerServerOptions is what
// guarantees it eventually drops. Both halves are needed.
func PeerDialOptions() []grpc.DialOption {
	return []grpc.DialOption{
		// Detect a peer that went away without a clean close, which is the normal
		// shape of a container being replaced.
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:                20 * time.Second,
			Timeout:             5 * time.Second,
			PermitWithoutStream: true,
		}),
		// Reconnect in seconds rather than the default's tens of seconds, while
		// still bounding the delay so a genuinely down peer is not hammered. Each
		// reconnect re-resolves the name, which is what picks up the new address.
		grpc.WithConnectParams(grpc.ConnectParams{
			Backoff: backoff.Config{
				BaseDelay:  200 * time.Millisecond,
				Multiplier: 1.6,
				Jitter:     0.2,
				MaxDelay:   5 * time.Second,
			},
			MinConnectTimeout: 5 * time.Second,
		}),
	}
}

// MaxPeerConnectionAge bounds how long a client may keep one connection to a
// peer, and therefore the worst-case window in which a client can be talking to
// the wrong service after an address swap.
//
// Deliberately short enough that a bad deploy self-heals inside a minute, and long
// enough that reconnects are irrelevant to throughput: at this age a chatty client
// re-handshakes twice a minute, which is nothing next to the per-call work.
const MaxPeerConnectionAge = 30 * time.Second

// PeerServerOptions are the options every internal gRPC server should serve with.
//
// MaxConnectionAge is the load-bearing one. It makes the server send a GOAWAY and
// close each connection once it reaches that age; the client then reconnects, and
// reconnecting RE-RESOLVES the peer name. That is the only thing that recovers a
// client whose connection is healthy but pointed at the wrong service, because
// nothing else will ever invalidate it.
//
// MaxConnectionAgeGrace lets in-flight RPCs finish rather than being cut off, so
// the rotation is invisible to callers. Long-poll RPCs (PollTask) are held open
// deliberately and are longer than the grace period, so they are cancelled at the
// transport layer and retried by the worker -- which its poll loop already treats
// as a normal empty poll.
func PeerServerOptions() []grpc.ServerOption {
	return []grpc.ServerOption{
		grpc.KeepaliveParams(keepalive.ServerParameters{
			MaxConnectionAge:      MaxPeerConnectionAge,
			MaxConnectionAgeGrace: 10 * time.Second,
			Time:                  20 * time.Second,
			Timeout:               5 * time.Second,
		}),
		// Tolerate the client keepalive above. Without this the server answers a
		// ping it considers too frequent with GOAWAY ENHANCE_YOUR_CALM, which
		// looks like an outage.
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			MinTime:             10 * time.Second,
			PermitWithoutStream: true,
		}),
	}
}

// PeerTarget normalizes a peer address into a gRPC target.
//
// A bare "host:port" uses the passthrough resolver, which never looks the name up
// again; dns:/// selects the DNS resolver, so each reconnect re-reads the record.
// An address that already carries a scheme is returned untouched, so an operator
// can point a service at a unix socket or a custom resolver without this
// rewriting it.
func PeerTarget(addr string) string {
	if addr == "" || strings.Contains(addr, "://") {
		return addr
	}
	return "dns:///" + addr
}
