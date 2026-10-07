-- Sessions minted by a restricted credential carry that credential's scopes.
-- NULL = unrestricted. Empty array = restricted, no labels.
ALTER TABLE member_sessions ADD COLUMN scopes TEXT[];
