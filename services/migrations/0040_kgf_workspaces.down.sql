-- Copyright 2026 Khuriwal Group
-- SPDX-License-Identifier: Apache-2.0

-- Reverses 0040. Roll the gateway back FIRST (a new gateway reads and writes
-- kgf_workspaces), or unset NEXUS_KGF_FRONT_ISSUERS so no workspace context is
-- accepted. Export kgf_workspaces first if the mapping may be needed again.
--
-- Tenants provisioned for workspaces are NOT removed: every row they own
-- references them, and their data outlives the mapping. Without
-- kgf_workspaces they are ordinary tenants that nothing signs in to (their
-- machine credentials were revoked when each workspace was deleted; those of
-- workspaces still active at rollback keep working).
-- Dropping the table drops its grants with it.
DROP TABLE IF EXISTS kgf_workspaces;
