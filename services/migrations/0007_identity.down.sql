-- Drops the identity tables. Destructive: every human account, every live session
-- and the entire administrative audit trail is in these rows, and nothing else
-- records them. Rolling this back logs every console user out and discards the
-- history of who changed what.
--
-- API-key authentication is unaffected by this rollback, which is the point of
-- having kept the two apart: the CLI and every worker keep working through it.
--
-- Dropped in dependency order so the intent is visible rather than implied
-- (sessions and scoped_tokens reference users).
DROP TABLE IF EXISTS audit_log;
DROP TABLE IF EXISTS sessions;
DROP TABLE IF EXISTS scoped_tokens;
DROP TABLE IF EXISTS users;
