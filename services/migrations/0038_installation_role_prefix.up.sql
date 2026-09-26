-- Copyright 2026 Khuriwal Group
-- SPDX-License-Identifier: Apache-2.0
--
-- Opt-in per-installation names for the six service group roles
-- (CONTRACT-FINDINGS F153, OPERATIONS §20.12).
--
-- 0005 created nexus_api_gateway ... nexus_event_service once per SERVER, and
-- every later migration grants to those names. Roles are cluster-wide, so on
-- a server holding several Nexus databases every installation's logins are
-- members of the SAME group roles, and each database's grants to them are
-- usable by every other installation's logins wherever CONNECT allows.
--
-- The names are fixed in 0005-0037, which are applied everywhere and must not
-- be edited. This migration therefore renames nothing. It adds:
--
--   * installation_role_prefix: the prefix this database uses, if any. No
--     row (the default) means the shared nexus_* names, exactly as before:
--     existing installations are unaffected.
--   * nexus_ensure_role_prefix(requested): run by migrate.sh after every
--     migration run (NEXUS_DB_ROLE_PREFIX). With a prefix, it creates
--     <prefix>_<service> NOLOGIN roles and MOVES every privilege the shared
--     nexus_<service> holds in THIS database -- relations, columns, routines,
--     the schema, the database -- to them, then proves nothing is left. Run
--     again after later migrations, it moves whatever they granted to the
--     shared names. A prefix, once applied, cannot be changed.
--   * nexus_role(service): the name to grant to. Migrations after this one
--     must use it (EXECUTE format('GRANT ... TO %I', nexus_role('scheduler')))
--     rather than a literal nexus_<service>, so a REVOKE reaches the right
--     role too; internal/migrations enforces that.
--   * nexus_unapply_role_prefix(): moves everything back (the down path).
--
-- ADDITIVE (ADR-0011 rule 5): a new table and functions. With no prefix
-- configured nothing is created or moved.

CREATE TABLE IF NOT EXISTS installation_role_prefix (
    singleton  BOOLEAN PRIMARY KEY DEFAULT true CHECK (singleton),
    prefix     TEXT NOT NULL CHECK (prefix ~ '^[a-z][a-z0-9_]{0,39}$' AND prefix <> 'nexus'),
    applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Every service reads everything (ADR-0011's matrix); moved with the rest
-- when a prefix is applied.
DO $$
DECLARE
    r TEXT;
BEGIN
    FOREACH r IN ARRAY ARRAY[
        'nexus_api_gateway', 'nexus_workflow_manager', 'nexus_dag_engine',
        'nexus_scheduler', 'nexus_resource_manager', 'nexus_event_service'
    ] LOOP
        IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = r) THEN
            EXECUTE format('GRANT SELECT ON installation_role_prefix TO %I', r);
        END IF;
    END LOOP;
END $$;

CREATE OR REPLACE FUNCTION nexus_service_roles() RETURNS TEXT[]
LANGUAGE sql IMMUTABLE AS $$
    SELECT ARRAY['api_gateway', 'workflow_manager', 'dag_engine',
                 'scheduler', 'resource_manager', 'event_service']
$$;

CREATE OR REPLACE FUNCTION nexus_role_prefix() RETURNS TEXT
LANGUAGE sql STABLE AS $$
    SELECT coalesce((SELECT prefix FROM installation_role_prefix), 'nexus')
$$;

CREATE OR REPLACE FUNCTION nexus_role(service TEXT) RETURNS TEXT
LANGUAGE plpgsql STABLE AS $$
BEGIN
    IF service IS NULL OR NOT service = ANY (nexus_service_roles()) THEN
        RAISE EXCEPTION 'nexus_role: % is not a Nexus service (one of %)', service, nexus_service_roles();
    END IF;
    RETURN nexus_role_prefix() || '_' || service;
END $$;

-- Every privilege role r holds in this database, as (kind, object, column,
-- privilege, grantable). The one inventory both the move and its proof read.
CREATE OR REPLACE FUNCTION nexus_role_privileges(r TEXT)
RETURNS TABLE (kind TEXT, obj TEXT, col TEXT, privilege TEXT, grantable BOOLEAN)
LANGUAGE sql STABLE AS $$
    WITH g AS (SELECT oid FROM pg_roles WHERE rolname = r)
    SELECT CASE WHEN c.relkind = 'S' THEN 'SEQUENCE' ELSE 'TABLE' END,
           c.oid::regclass::text, NULL::text, a.privilege_type, a.is_grantable
      FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace,
           LATERAL aclexplode(c.relacl) a, g
     WHERE n.nspname = 'public' AND a.grantee = g.oid
    UNION ALL
    SELECT 'COLUMN', c.oid::regclass::text, att.attname::text, a.privilege_type, a.is_grantable
      FROM pg_attribute att JOIN pg_class c ON c.oid = att.attrelid
      JOIN pg_namespace n ON n.oid = c.relnamespace,
           LATERAL aclexplode(att.attacl) a, g
     WHERE n.nspname = 'public' AND NOT att.attisdropped AND a.grantee = g.oid
    UNION ALL
    SELECT 'ROUTINE', p.oid::regprocedure::text, NULL, a.privilege_type, a.is_grantable
      FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace,
           LATERAL aclexplode(p.proacl) a, g
     WHERE n.nspname = 'public' AND a.grantee = g.oid
    UNION ALL
    SELECT 'SCHEMA', 'public', NULL, a.privilege_type, a.is_grantable
      FROM pg_namespace n, LATERAL aclexplode(n.nspacl) a, g
     WHERE n.nspname = 'public' AND a.grantee = g.oid
    UNION ALL
    SELECT 'DATABASE', quote_ident(current_database()), NULL, a.privilege_type, a.is_grantable
      FROM pg_database d, LATERAL aclexplode(d.datacl) a, g
     WHERE d.datname = current_database() AND a.grantee = g.oid
$$;

-- Moves every privilege src holds in this database to dst; returns how many.
CREATE OR REPLACE FUNCTION nexus_move_role_privileges(src TEXT, dst TEXT) RETURNS INTEGER
LANGUAGE plpgsql AS $$
DECLARE
    p      RECORD;
    target TEXT;
    opt    TEXT;
    moved  INTEGER := 0;
BEGIN
    FOR p IN SELECT * FROM nexus_role_privileges(src) LOOP
        target := CASE p.kind
            WHEN 'COLUMN' THEN format('%s (%I) ON TABLE %s', p.privilege, p.col, p.obj)
            ELSE format('%s ON %s %s', p.privilege, p.kind, p.obj)
        END;
        opt := CASE WHEN p.grantable THEN ' WITH GRANT OPTION' ELSE '' END;
        EXECUTE format('GRANT %s TO %I%s', target, dst, opt);
        EXECUTE format('REVOKE %s FROM %I', target, src);
        moved := moved + 1;
    END LOOP;
    RETURN moved;
END $$;

-- The migrate step. requested: NEXUS_DB_ROLE_PREFIX ('' = whatever this
-- database already uses). Returns the prefix in force.
CREATE OR REPLACE FUNCTION nexus_ensure_role_prefix(requested TEXT) RETURNS TEXT
LANGUAGE plpgsql AS $$
DECLARE
    cur TEXT;
    p   TEXT;
    svc TEXT;
    src TEXT;
    dst TEXT;
    login BOOLEAN;
BEGIN
    requested := coalesce(requested, '');
    IF requested <> '' AND requested !~ '^[a-z][a-z0-9_]{0,39}$' THEN
        RAISE EXCEPTION 'role prefix % must match ^[a-z][a-z0-9_]{0,39}$', quote_literal(requested);
    END IF;
    -- Two migrators on one database must not interleave the move.
    PERFORM pg_advisory_xact_lock(hashtext('nexus_ensure_role_prefix'));
    SELECT prefix INTO cur FROM installation_role_prefix;
    IF cur IS NOT NULL AND requested <> '' AND requested <> cur THEN
        RAISE EXCEPTION 'this database''s group roles already use prefix %; changing it (to %) is not supported: roll back 0038 first',
            cur, requested;
    END IF;
    p := coalesce(cur, nullif(nullif(requested, ''), 'nexus'));
    IF p IS NULL THEN
        RETURN 'nexus';
    END IF;
    FOREACH svc IN ARRAY nexus_service_roles() LOOP
        src := 'nexus_' || svc;
        dst := p || '_' || svc;
        SELECT rolcanlogin OR rolsuper OR rolcreaterole OR rolbypassrls INTO login FROM pg_roles WHERE rolname = dst;
        IF login IS NULL THEN
            EXECUTE format('CREATE ROLE %I NOLOGIN', dst);
        ELSIF login THEN
            RAISE EXCEPTION 'role % exists and can log in or holds administrative attributes; refusing to make it a group role', dst;
        END IF;
        IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = src) THEN
            PERFORM nexus_move_role_privileges(src, dst);
            IF EXISTS (SELECT 1 FROM nexus_role_privileges(src)) THEN
                -- REVOKE only removes grants the revoker made (or, for a
                -- superuser, the owner made): anything granted by a third
                -- role survives, and must not be left silently.
                RAISE EXCEPTION '% still holds privileges in this database after the move (granted by a role other than the owner?)', src;
            END IF;
        END IF;
    END LOOP;
    IF cur IS NULL THEN
        INSERT INTO installation_role_prefix (prefix) VALUES (p);
    END IF;
    RETURN p;
END $$;

-- The down path: every privilege back on the shared names, the record gone.
CREATE OR REPLACE FUNCTION nexus_unapply_role_prefix() RETURNS TEXT
LANGUAGE plpgsql AS $$
DECLARE
    p   TEXT;
    svc TEXT;
BEGIN
    PERFORM pg_advisory_xact_lock(hashtext('nexus_ensure_role_prefix'));
    SELECT prefix INTO p FROM installation_role_prefix;
    IF p IS NULL THEN
        RETURN NULL;
    END IF;
    FOREACH svc IN ARRAY nexus_service_roles() LOOP
        IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = p || '_' || svc) THEN
            IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'nexus_' || svc) THEN
                EXECUTE format('CREATE ROLE %I NOLOGIN', 'nexus_' || svc);
            END IF;
            PERFORM nexus_move_role_privileges(p || '_' || svc, 'nexus_' || svc);
        END IF;
    END LOOP;
    DELETE FROM installation_role_prefix;
    RETURN p;
END $$;

-- Only the owner (the migrate login) moves grants.
REVOKE ALL ON FUNCTION nexus_move_role_privileges(TEXT, TEXT) FROM PUBLIC;
REVOKE ALL ON FUNCTION nexus_ensure_role_prefix(TEXT) FROM PUBLIC;
REVOKE ALL ON FUNCTION nexus_unapply_role_prefix() FROM PUBLIC;
