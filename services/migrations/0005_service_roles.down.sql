-- Revoke the per-service grants and drop the roles.
--
-- Safe to run only while no service is CONNECTED as one of these roles: Postgres
-- refuses to drop a role that still owns objects or is still granted to a live
-- login user, so the DROP is guarded and reports rather than half-completing.
--
-- Rolling this back returns the schema to convention-only ownership (what existed
-- before 0005), which is strictly weaker but not broken: every service worked that
-- way when they all connected as the superuser.
DO $$
DECLARE
    r TEXT;
BEGIN
    FOREACH r IN ARRAY ARRAY[
        'nexus_api_gateway', 'nexus_workflow_manager', 'nexus_dag_engine',
        'nexus_scheduler', 'nexus_resource_manager', 'nexus_event_service'
    ] LOOP
        IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = r) THEN
            EXECUTE format('REVOKE ALL ON ALL TABLES IN SCHEMA public FROM %I', r);
            EXECUTE format('REVOKE ALL ON ALL SEQUENCES IN SCHEMA public FROM %I', r);
            EXECUTE format('REVOKE ALL ON SCHEMA public FROM %I', r);
            -- A role still granted to a login user cannot be dropped. Left in
            -- place with no privileges rather than failing the migration, since a
            -- privilege-less role is harmless and a failed down-migration is not.
            BEGIN
                EXECUTE format('DROP ROLE %I', r);
            EXCEPTION WHEN dependent_objects_still_exist OR OTHERS THEN
                RAISE NOTICE 'role % still granted to a login user; privileges revoked, role kept', r;
            END;
        END IF;
    END LOOP;
END $$;
