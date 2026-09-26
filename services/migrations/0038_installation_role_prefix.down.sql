-- Copyright 2026 Khuriwal Group
-- SPDX-License-Identifier: Apache-2.0

-- Moves every privilege back to the shared nexus_* group roles, then removes
-- 0038's table and functions. The prefixed roles are dropped only when
-- nothing else holds them (roles are server-wide; the guard mirrors 0005's
-- rollback). Stop the services first: logins that are members of the
-- prefixed roles lose their grants here, and must be re-granted the nexus_*
-- roles (re-run nexus-db-roles without NEXUS_DB_ROLE_PREFIX).
DO $$
DECLARE
    p   TEXT;
    svc TEXT;
BEGIN
    p := nexus_unapply_role_prefix();
    IF p IS NOT NULL THEN
        FOREACH svc IN ARRAY nexus_service_roles() LOOP
            BEGIN
                EXECUTE format('DROP ROLE IF EXISTS %I', p || '_' || svc);
            EXCEPTION WHEN dependent_objects_still_exist OR OTHERS THEN
                RAISE NOTICE 'role %_% is still granted or holds privileges elsewhere; kept', p, svc;
            END;
        END LOOP;
    END IF;
END $$;

DROP FUNCTION IF EXISTS nexus_unapply_role_prefix();
DROP FUNCTION IF EXISTS nexus_ensure_role_prefix(TEXT);
DROP FUNCTION IF EXISTS nexus_move_role_privileges(TEXT, TEXT);
DROP FUNCTION IF EXISTS nexus_role_privileges(TEXT);
DROP FUNCTION IF EXISTS nexus_role(TEXT);
DROP FUNCTION IF EXISTS nexus_role_prefix();
DROP FUNCTION IF EXISTS nexus_service_roles();
DROP TABLE IF EXISTS installation_role_prefix;
