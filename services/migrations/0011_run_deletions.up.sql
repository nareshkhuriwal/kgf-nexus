-- Copyright 2026 Khuriwal Group
-- SPDX-License-Identifier: Apache-2.0
CREATE TABLE run_deletions (
 run_id UUID PRIMARY KEY REFERENCES workflow_runs(id) ON DELETE CASCADE,
 namespace TEXT NOT NULL,
 deleted_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
GRANT SELECT, INSERT ON run_deletions TO nexus_api_gateway;
