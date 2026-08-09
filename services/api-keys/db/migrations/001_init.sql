CREATE TABLE IF NOT EXISTS api_keys (
    id            TEXT PRIMARY KEY,
    user_id       TEXT NOT NULL,
    name          TEXT NOT NULL,
    key_prefix    TEXT NOT NULL,
    key_hash      TEXT NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    revoked_at    TIMESTAMPTZ
);

CREATE UNIQUE INDEX IF NOT EXISTS api_keys_key_hash_uidx ON api_keys (key_hash);

CREATE INDEX IF NOT EXISTS api_keys_user_id_idx ON api_keys (user_id);
