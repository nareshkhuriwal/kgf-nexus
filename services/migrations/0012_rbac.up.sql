-- Copyright 2026 Khuriwal Group
-- SPDX-License-Identifier: Apache-2.0
CREATE TABLE rbac_roles (
 namespace TEXT NOT NULL, name TEXT NOT NULL, permissions JSONB NOT NULL CHECK(jsonb_typeof(permissions)='array'),
 revision BIGINT NOT NULL DEFAULT 1, PRIMARY KEY(namespace,name)
);
CREATE TABLE rbac_user_access (
 user_id UUID PRIMARY KEY REFERENCES users(id), namespace TEXT NOT NULL,
 roles JSONB NOT NULL CHECK(jsonb_typeof(roles)='array'), revision BIGINT NOT NULL DEFAULT 1
);
GRANT SELECT,INSERT,UPDATE,DELETE ON rbac_roles,rbac_user_access TO nexus_api_gateway;
