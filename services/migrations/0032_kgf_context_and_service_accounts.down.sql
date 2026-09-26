-- Copyright 2026 Khuriwal Group
-- SPDX-License-Identifier: Apache-2.0

-- Reverses 0032. Roll the gateway back FIRST (a new gateway reads every object
-- dropped here), and switch it to NEXUS_AUTH_MODE=standalone. Export
-- kgf_scopes and service_accounts first if they may be needed again.
--
-- Tenants provisioned for KGF scopes are NOT removed: every row they own
-- references them, and their data outlives the mapping. Without kgf_scopes they
-- are ordinary tenants that nothing signs in to.

DROP TRIGGER IF EXISTS service_account_namespace ON scoped_tokens;
DROP FUNCTION IF EXISTS public.scoped_token_account_namespace();
ALTER TABLE scoped_tokens DROP COLUMN IF EXISTS permissions;
ALTER TABLE scoped_tokens DROP COLUMN IF EXISTS service_account_id;
DROP TABLE IF EXISTS service_account_roles;
DROP TABLE IF EXISTS service_accounts;
DROP INDEX IF EXISTS audit_log_request_id_idx;
ALTER TABLE audit_log DROP COLUMN IF EXISTS request_id;
DROP TABLE IF EXISTS kgf_scopes;
