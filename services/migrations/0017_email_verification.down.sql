-- Copyright 2026 Khuriwal Group
-- SPDX-License-Identifier: Apache-2.0

-- Roll back 0017. Revert the gateway and console first: a gateway that expects
-- email_verified_at fails every login once the column is gone.
--
-- Dropping the column discards WHICH accounts proved their address. Re-applying
-- 0017 afterwards marks every account that exists at that moment as verified,
-- including ones that never were, so export `SELECT id FROM users WHERE
-- email_verified_at IS NULL` first if that distinction matters.
DROP TABLE IF EXISTS email_tokens;
DROP TRIGGER IF EXISTS users_email_change_unverifies ON users;
DROP FUNCTION IF EXISTS public.users_email_change_unverifies();
ALTER TABLE users DROP COLUMN IF EXISTS email_verified_at;
