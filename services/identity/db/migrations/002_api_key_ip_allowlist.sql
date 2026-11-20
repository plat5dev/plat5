-- API key IP allowlists (docs/reference/api/identity.md#key).
ALTER TABLE api_keys ADD COLUMN allowed_cidrs TEXT[] NOT NULL DEFAULT '{}';
