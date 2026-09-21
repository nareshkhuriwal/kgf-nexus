-- Copyright 2026 Khuriwal Group
-- SPDX-License-Identifier: Apache-2.0
-- Customer identity is separate from tenant-local Administrator permissions.
CREATE TABLE tenants (
 id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
 namespace TEXT NOT NULL UNIQUE,
 name TEXT NOT NULL CHECK(length(name) BETWEEN 1 AND 160),
 contact_email TEXT NOT NULL DEFAULT '',
 registration_email TEXT UNIQUE,
 owner_user_id UUID REFERENCES users(id),
 created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- Backfill all existing namespace-bearing tables without changing identifiers.
DO $$ DECLARE t record; BEGIN
 FOR t IN SELECT table_name FROM information_schema.columns
 WHERE table_schema='public' AND column_name='namespace' AND table_name <> 'tenants'
 LOOP
  EXECUTE format('INSERT INTO tenants(namespace,name) SELECT DISTINCT namespace, namespace FROM %I WHERE namespace IS NOT NULL AND namespace <> '''' ON CONFLICT(namespace) DO NOTHING', t.table_name);
 END LOOP;
END $$;
UPDATE tenants t SET owner_user_id=(SELECT u.id FROM users u WHERE u.namespace=t.namespace AND u.role='admin' AND u.disabled_at IS NULL ORDER BY u.created_at,u.id LIMIT 1);
CREATE TABLE platform_operators (user_id UUID PRIMARY KEY REFERENCES users(id));
-- No automatic elevation: an installation operator explicitly enrolls a user.
GRANT SELECT,INSERT,UPDATE ON tenants TO nexus_api_gateway;
GRANT SELECT ON platform_operators TO nexus_api_gateway;
