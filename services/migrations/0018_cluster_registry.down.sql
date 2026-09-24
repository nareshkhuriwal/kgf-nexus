-- Copyright 2026 Khuriwal Group
-- SPDX-License-Identifier: Apache-2.0

-- Roll back 0018. Stop every cluster-controller and deploy a gateway and
-- resource-manager that predate 0018 FIRST: those are the only readers and
-- writers of these tables. Export cluster_scaling_events beforehand if the
-- scaling audit trail must be retained -- dropping it discards it.
DROP TABLE IF EXISTS cluster_scaling_events;
DROP TABLE IF EXISTS cluster_reconcile_errors;
DROP TABLE IF EXISTS cluster_scaling_state;
DROP TABLE IF EXISTS cluster_observations;
DROP TABLE IF EXISTS cluster_status;
DROP TABLE IF EXISTS cluster_namespace_bindings;
DROP TABLE IF EXISTS clusters;
