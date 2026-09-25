-- Copyright 2026 Khuriwal Group
-- SPDX-License-Identifier: Apache-2.0
-- Reverse 0025.
--
-- Roll the gateways back FIRST: a gateway that advertises log shipping writes
-- these tables on every ReportLogs. Dropping them removes the INDEX only; the
-- chunk objects stay in the result store and become orphans, which artifact-gc
-- collects after its grace period -- so a rollback is not a data-destroying
-- step for anything but the logs themselves.
DROP TABLE IF EXISTS attempt_log_chunks;
DROP TABLE IF EXISTS attempt_logs;
