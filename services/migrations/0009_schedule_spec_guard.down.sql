-- Copyright 2026 Khuriwal Group
-- SPDX-License-Identifier: Apache-2.0

-- Removes the guard, restoring 0006's behaviour: schedules.spec_name becomes plain
-- TEXT that accepts anything again.
--
-- Safe to run -- nothing depends on the trigger existing, and no stored data is
-- shaped by it. What it costs is the invariant: after this, a direct INSERT can
-- once again create a schedule that arms, fires on time and resolves nothing. The
-- gateway's submit-time check still rejects that over the REST API, so the window
-- reopened here is writers that are not the gateway.
--
-- The trigger is dropped before the function it calls; the reverse order would
-- leave a trigger pointing at a missing function on any server that does not
-- refuse the drop.
DROP TRIGGER IF EXISTS trg_schedules_spec_exists ON schedules;
DROP FUNCTION IF EXISTS schedules_require_existing_spec();
