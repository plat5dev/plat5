-- A credential carries its principal's permissions (docs/roles.md). Keys and
-- sessions no longer hold scopes of their own.
--
-- Fail closed. Without the column, a key or session that had a list would carry
-- its owner's full permissions. Revoke those keys and drop those sessions so
-- nothing gains access by upgrading. Keys with a NULL list already followed
-- their owner and are kept.
UPDATE user_api_keys SET revoked_at = now() WHERE scopes IS NOT NULL AND revoked_at IS NULL;
UPDATE member_api_keys SET revoked_at = now() WHERE scopes IS NOT NULL AND revoked_at IS NULL;
DELETE FROM member_sessions WHERE scopes IS NOT NULL;

ALTER TABLE user_api_keys DROP COLUMN scopes;
ALTER TABLE member_api_keys DROP COLUMN scopes;
ALTER TABLE member_sessions DROP COLUMN scopes;
