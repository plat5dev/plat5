-- A credential carries its principal's permissions (docs/roles.md). Keys and
-- sessions no longer hold scopes of their own.
ALTER TABLE user_api_keys DROP COLUMN scopes;
ALTER TABLE member_api_keys DROP COLUMN scopes;
ALTER TABLE member_sessions DROP COLUMN scopes;
