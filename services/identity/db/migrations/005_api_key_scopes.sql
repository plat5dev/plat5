-- Optional scope labels on API keys.
-- NULL = unrestricted (legacy keys). Empty array = grants nothing.
ALTER TABLE user_api_keys ADD COLUMN scopes TEXT[];
ALTER TABLE member_api_keys ADD COLUMN scopes TEXT[];
