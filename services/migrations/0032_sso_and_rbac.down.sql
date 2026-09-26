-- Copyright 2026 Khuriwal Group
-- SPDX-License-Identifier: Apache-2.0

-- Reverses 0032. Roll the gateway and console back FIRST: a new gateway reads
-- every object dropped here. Export sso_providers, sso_group_mappings,
-- user_identities, service_accounts and namespace_owner_grants before running
-- this if they may be needed again; SSO-provisioned users remain but have no
-- password and cannot sign in until an administrator sets one.
--
-- The clusters.* grants added to custom roles are left in place: the older
-- gateway ignores permission identifiers it does not know (they grant nothing),
-- and removing them would silently narrow roles on a later re-apply.

DROP TRIGGER IF EXISTS service_account_namespace ON scoped_tokens;
DROP FUNCTION IF EXISTS public.scoped_token_account_namespace();
ALTER TABLE scoped_tokens DROP COLUMN IF EXISTS permissions;
ALTER TABLE scoped_tokens DROP COLUMN IF EXISTS service_account_id;

ALTER TABLE sessions DROP COLUMN IF EXISTS sso_session_index;
ALTER TABLE sessions DROP COLUMN IF EXISTS sso_name_id;
ALTER TABLE sessions DROP COLUMN IF EXISTS sso_provider_id;
ALTER TABLE rbac_user_access DROP COLUMN IF EXISTS idp_roles;

DROP TABLE IF EXISTS namespace_owner_grants;
DROP TABLE IF EXISTS service_account_roles;
DROP TABLE IF EXISTS service_accounts;
DROP TABLE IF EXISTS sso_group_mappings;
DROP TABLE IF EXISTS sso_assertion_replay;
DROP TABLE IF EXISTS sso_handoffs;
DROP TABLE IF EXISTS sso_login_states;
DROP TABLE IF EXISTS user_identities;
DROP FUNCTION IF EXISTS public.sso_identity_same_namespace();
DROP TABLE IF EXISTS sso_providers;
