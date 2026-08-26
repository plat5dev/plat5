-- Optional scopes on user and member API keys.
-- NULL = unrestricted. Empty array = grants nothing.
ALTER TABLE user_api_keys
    ADD COLUMN scopes TEXT[];

ALTER TABLE member_api_keys
    ADD COLUMN scopes TEXT[];
