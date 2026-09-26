-- Copyright 2026 Khuriwal Group
-- SPDX-License-Identifier: Apache-2.0
--
-- The grants services need when each connects as ITS OWN login, a member of
-- its own 0005 group role only (ADR-0011's write matrix enforced;
-- `postgres.runtimeRoles.scope: service`; CONTRACT-FINDINGS F132). Every one
-- was found by running the stack that way (deploy/e2e/docker-compose.service-
-- logins.yml): each statement below failed with "permission denied".
--
-- nexus_scheduler. 0005 granted INSERT and DELETE on timers (it creates and
-- clears deadline timers) and UPDATE (committed, state) on workers (capacity
-- and the DEAD transition). Two loops that moved into the scheduler later
-- write more:
--   * ClaimDueTimers (scheduler_timers.go) leases due timers with
--     UPDATE timers SET claimed_by, claimed_at ... FOR UPDATE SKIP LOCKED and
--     releases them with UPDATE timers SET claimed_by = NULL; without it no
--     timer -- deadline, retry backoff, lease expiry -- ever fired.
--   * ReapDeadWorkers (scheduler_liveness.go) DELETEs workers DEAD longer than
--     the retention window that hold no lease or reservation.
--
-- nexus_api_gateway. The batched lease renewal (GatewayRenewLeases,
-- gateway_leases.go, ADR-0010) extends the leases of a worker's attempts in
-- one statement per renewal, on the ExtendLease RPC path: UPDATE leases SET
-- expires_at, renewed_at, fenced by attempt, worker and token. Leases are the
-- scheduler's; without this every renewal failed, and every attempt running
-- longer than one lease TTL was reclaimed as LOST.
--
-- Every service but the gateway. Tenant ids are resolved from a namespace
-- inside queries that only the tenants table can answer: the dag-engine's
-- memo lookup (dagengine_memo.go), and the usage rollups. 0013 granted the
-- tenants table to the gateway alone, so under per-service logins every memo
-- lookup failed (the engine then ran without memoization, silently slower).
-- SELECT (id, namespace) only: never the contact and registration e-mails.
--
-- Column-scoped wherever the statement allows (timers: the two lease columns,
-- never fire_at, kind or payload; leases: the expiry and renewal time, never
-- the token, worker or epoch), and each recorded as a documented cross-owner
-- exception in internal/store/service_roles_integration_test.go.
--
-- The retry budget grants are 0035's (UPDATE (attempts_used,
-- preemptions_used) ON task_nodes, F147) and 0036's (UPDATE (free_retries));
-- F149 was the same gap as F147.
--
-- ADDITIVE (ADR-0011 rule 5): grants only. Guarded on the role existing, like
-- every grant since 0005.
DO $$
DECLARE
    r TEXT;
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'nexus_scheduler') THEN
        GRANT UPDATE (claimed_by, claimed_at) ON timers TO nexus_scheduler;
        GRANT DELETE ON workers TO nexus_scheduler;
    END IF;
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'nexus_api_gateway') THEN
        GRANT UPDATE (expires_at, renewed_at) ON leases TO nexus_api_gateway;
    END IF;
    FOREACH r IN ARRAY ARRAY[
        'nexus_workflow_manager', 'nexus_dag_engine', 'nexus_scheduler',
        'nexus_resource_manager', 'nexus_event_service'
    ] LOOP
        IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = r) THEN
            EXECUTE format('GRANT SELECT (id, namespace) ON tenants TO %I', r);
        END IF;
    END LOOP;
END $$;
