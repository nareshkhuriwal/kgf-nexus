-- Copyright 2026 Khuriwal Group
-- SPDX-License-Identifier: Apache-2.0

-- Reverses 0041. Safe in any order with the services: a process that writes
-- the registry logs and meters each failed beat once the table is gone
-- (nexus_instance_heartbeat_failures_total) and keeps serving, and the
-- gateway's components route answers 500 until it is rolled back too. The
-- rows are observability only; nothing is lost that the next beat of a newer
-- release would not write again. Dropping the table drops its grants with it.
DROP TABLE IF EXISTS service_instances;
