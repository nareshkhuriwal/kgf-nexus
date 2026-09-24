-- Copyright 2026 Khuriwal Group
-- SPDX-License-Identifier: Apache-2.0

-- Remove the outbox.
--
-- Safe to run only after every writer has been rolled back to a version that does
-- not INSERT into it: a replica still writing outbox rows would fail its claim
-- transaction, and dispatch would stop. Dropping it loses nothing durable --
-- every row announces a fact that is already committed elsewhere, and the bus is
-- never the source of truth (ADR-0003).
DROP TABLE IF EXISTS outbox;
