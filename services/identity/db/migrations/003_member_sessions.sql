CREATE TABLE member_sessions (
    id            TEXT PRIMARY KEY,
    member_id     TEXT NOT NULL REFERENCES members (id) ON DELETE CASCADE,
    token_prefix  TEXT NOT NULL,
    token_hash    TEXT NOT NULL,
    expires_at    TIMESTAMPTZ NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX member_sessions_token_hash_uidx ON member_sessions (token_hash);
CREATE INDEX member_sessions_member_id_idx ON member_sessions (member_id);
