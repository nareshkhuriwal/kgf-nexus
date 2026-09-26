-- Copyright 2026 Khuriwal Group
-- SPDX-License-Identifier: Apache-2.0
--
-- Reverses 0037. Under per-service logins the scheduler's timer loop and
-- dead-worker reaper, and the gateway's lease renewal, then fail with
-- permission denied again (F132); move the deployment back to the owner login
-- or `runtimeRoles.scope: union` first.
DO $$
DECLARE
    r TEXT;
BEGIN
    FOREACH r IN ARRAY ARRAY[
        'nexus_workflow_manager', 'nexus_dag_engine', 'nexus_scheduler',
        'nexus_resource_manager', 'nexus_event_service'
    ] LOOP
        IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = r) THEN
            EXECUTE format('REVOKE SELECT (id, namespace) ON tenants FROM %I', r);
        END IF;
    END LOOP;
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'nexus_api_gateway') THEN
        REVOKE UPDATE (expires_at, renewed_at) ON leases FROM nexus_api_gateway;
    END IF;
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'nexus_scheduler') THEN
        REVOKE DELETE ON workers FROM nexus_scheduler;
        REVOKE UPDATE (claimed_by, claimed_at) ON timers FROM nexus_scheduler;
    END IF;
END $$;
