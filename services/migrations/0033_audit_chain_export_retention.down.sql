-- Copyright 2026 Khuriwal Group
-- SPDX-License-Identifier: Apache-2.0
-- migrate: no-transaction

-- Rolls back 0033. DESTRUCTIVE for evidence: the chain columns, every
-- checkpoint, the retention/legal-hold policy and the sink configuration
-- (including their delivery cursors) are dropped. The audit rows themselves
-- are kept. Export the audit trail (and record each namespace's chain head)
-- BEFORE rolling back, or the tamper evidence for the retained rows is lost.
--
-- Roll back the gateway first: a 0033-aware gateway writes chain columns.
DROP TRIGGER IF EXISTS audit_log_no_update ON audit_log;
DROP TRIGGER IF EXISTS audit_log_no_delete ON audit_log;
DROP FUNCTION IF EXISTS audit_log_immutable();
DROP FUNCTION IF EXISTS audit_purge(TEXT);
DROP TABLE IF EXISTS audit_sinks;
DROP TABLE IF EXISTS audit_policies;
DROP TABLE IF EXISTS audit_chain_checkpoints;
DROP TABLE IF EXISTS audit_chain_heads;
DROP INDEX CONCURRENTLY IF EXISTS audit_log_chain_idx;
-- request_id belongs to 0032 (ADR-0024 §5) and stays until 0032 is rolled back.
ALTER TABLE audit_log DROP COLUMN IF EXISTS kgf_on_behalf_of;
ALTER TABLE audit_log DROP COLUMN IF EXISTS kgf_principal;
ALTER TABLE audit_log DROP COLUMN IF EXISTS kgf_resource_group;
ALTER TABLE audit_log DROP COLUMN IF EXISTS kgf_subscription;
ALTER TABLE audit_log DROP COLUMN IF EXISTS kgf_organization;
ALTER TABLE audit_log DROP COLUMN IF EXISTS row_hash;
ALTER TABLE audit_log DROP COLUMN IF EXISTS prev_hash;
ALTER TABLE audit_log DROP COLUMN IF EXISTS chain_seq;
