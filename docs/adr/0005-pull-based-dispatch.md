# ADR-0005: Pull-based dispatch with push notification

- **Status:** Proposed
- **Date:** 2026-09-12
- **Deciders:** platform team

## Context

Work must get from the control plane to a worker. Either the control plane connects to workers (push) or workers connect to the control plane (pull).

Push requires the control plane to know and reach every worker's network address. That means workers need inbound connectivity — no NAT, no arbitrary VPC, no developer laptop, no edge host. It also makes the control plane responsible for tracking reachability, managing per-worker connections, and implementing its own backpressure when a worker is saturated.

Pull inverts all of that, at the cost of poll-interval latency.

## Decision

**Workers pull. The control plane never initiates a connection to a worker.** A NATS notification (ADR-0003) removes the latency penalty.

Flow:
1. Worker registers over gRPC, advertising capacity, queues, and labels.
2. Worker subscribes to `task.dispatch.<queue>` for each queue it serves.
3. On notification — or on a fallback poll interval — the worker calls `PollTask(worker_id, free_capacity)`.
4. The control plane returns an attempt plus a lease token, or empty.
5. The worker reports state changes via gRPC, which it initiates.

Control-plane→worker directives (drain, cancel a specific attempt, config update) ride on the **heartbeat response** rather than a pushed connection. This is the key trick that keeps the design fully pull-based without losing the ability to command the fleet: at a 5s heartbeat, worst-case command latency is 5s, which is entirely adequate for drain and cancel.

## Consequences

**Positive**
- Workers run anywhere with outbound network access: behind NAT, in another cloud, in a customer VPC, on a laptop. This substantially widens deployment topology.
- **Backpressure is automatic and needs no mechanism.** A saturated worker stops polling. No queue-depth signalling, no flow control protocol, no rate negotiation.
- The control plane holds no per-worker connection state, so it stays stateless and horizontally scalable.
- The worker decides admission based on its *actual* local state, catching cases where the control plane's ledger is momentarily stale.
- Simpler security posture: no inbound ports on workers, one outbound direction to authorize.

**Negative**
- Worst-case dispatch latency is the poll interval when NATS is unavailable. Accepted: it is a degraded mode, not the normal path.
- Poll traffic scales with fleet size even when idle. Mitigated by long-polling (hold the request open server-side until work arrives or timeout) and a server-controlled interval in `HeartbeatResponse`, so the control plane can slow the fleet down under load.
- Directive latency is bounded by the heartbeat interval. Acceptable for drain/cancel; if a use case ever needs faster, a notification subject per worker is a small addition.

**Neutral**
- Long-polling holds connections open; needs connection-limit tuning on the gateway.

## Alternatives considered

**Push (control plane dials workers).** Lowest latency and no poll overhead. Rejected: it forecloses NAT'd and cross-network workers, makes the control plane stateful in connection tracking, and requires building explicit backpressure that pull gets for free.

**Bidirectional gRPC streams (worker dials, then server pushes over the stream).** Genuinely attractive — worker-initiated, so it preserves the NAT story, and gives push latency without NATS. Rejected for MVP because it makes the gateway stateful (each stream pins a worker to a specific replica, complicating rolling upgrades and load balancing) and complicates graceful shutdown. **Reconsider in Phase 2**; if adopted, it could replace the NATS dispatch subject entirely. Worth flagging as the most likely design change of this set.

**Worker-side queue subscription (Celery-style: workers consume the broker directly).** Rejected: it puts task assignment in the broker, so the control plane loses the atomic capacity-plus-lease decision of §5.3 and cannot do resource-aware placement at all. That would gut goal G3.
