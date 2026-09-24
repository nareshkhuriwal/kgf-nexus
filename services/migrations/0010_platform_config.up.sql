-- Copyright 2026 Khuriwal Group
-- SPDX-License-Identifier: Apache-2.0
CREATE TABLE platform_config (
 namespace TEXT PRIMARY KEY,
 revision BIGINT NOT NULL CHECK (revision > 0),
 config JSONB NOT NULL,
 updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
DO $$ BEGIN
 IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname='nexus_api_gateway') THEN
 GRANT SELECT, INSERT, UPDATE ON platform_config TO nexus_api_gateway;
 END IF;
END $$;
