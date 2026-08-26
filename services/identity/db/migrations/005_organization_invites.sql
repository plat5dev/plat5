CREATE TABLE IF NOT EXISTS organization_invites (
    id               TEXT PRIMARY KEY,
    organization_id  TEXT NOT NULL REFERENCES organizations (id) ON DELETE CASCADE,
    role             TEXT NOT NULL CHECK (role IN ('member', 'admin', 'owner')),
    email            TEXT,
    token_hash       TEXT NOT NULL,
    token_prefix     TEXT NOT NULL,
    created_by       TEXT NOT NULL,
    expires_at       TIMESTAMPTZ NOT NULL,
    redeemed_at      TIMESTAMPTZ,
    redeemed_by      TEXT,
    revoked_at       TIMESTAMPTZ,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX IF NOT EXISTS organization_invites_token_hash_uidx
    ON organization_invites (token_hash);

CREATE INDEX IF NOT EXISTS organization_invites_org_created_idx
    ON organization_invites (organization_id, created_at DESC);
