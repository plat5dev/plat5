CREATE TABLE organizations (
    id          TEXT PRIMARY KEY,
    name        TEXT NOT NULL,
    slug        TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX organizations_slug_uidx ON organizations (slug);

CREATE TABLE service_accounts (
    id                  TEXT PRIMARY KEY,
    organization_id     TEXT NOT NULL REFERENCES organizations (id) ON DELETE CASCADE,
    name                TEXT NOT NULL,
    created_by_user_id  TEXT,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX service_accounts_organization_id_idx
    ON service_accounts (organization_id);

CREATE TABLE members (
    id                  TEXT PRIMARY KEY,
    organization_id     TEXT NOT NULL REFERENCES organizations (id) ON DELETE CASCADE,
    user_id             TEXT,
    service_account_id  TEXT REFERENCES service_accounts (id) ON DELETE CASCADE,
    role                TEXT NOT NULL CHECK (role IN ('member', 'admin', 'owner')),
    status              TEXT NOT NULL CHECK (status IN ('active', 'suspended', 'removed')),
    added_by            TEXT,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (
        (user_id IS NOT NULL AND service_account_id IS NULL)
        OR (user_id IS NULL AND service_account_id IS NOT NULL)
    ),
    CHECK (NOT (service_account_id IS NOT NULL AND role = 'owner'))
);

CREATE UNIQUE INDEX members_org_user_uidx
    ON members (organization_id, user_id)
    WHERE user_id IS NOT NULL;

CREATE UNIQUE INDEX members_sa_uidx
    ON members (service_account_id)
    WHERE service_account_id IS NOT NULL;

CREATE INDEX members_user_id_idx
    ON members (user_id)
    WHERE user_id IS NOT NULL;

CREATE INDEX members_org_status_idx
    ON members (organization_id, status);

CREATE TABLE organization_invites (
    id               TEXT PRIMARY KEY,
    organization_id  TEXT NOT NULL REFERENCES organizations (id) ON DELETE CASCADE,
    role             TEXT NOT NULL CHECK (role IN ('member', 'admin', 'owner')),
    email            TEXT,
    token            TEXT,
    token_hash       TEXT NOT NULL,
    token_prefix     TEXT NOT NULL,
    status           TEXT NOT NULL DEFAULT 'active'
                         CHECK (status IN ('active', 'redeemed', 'revoked', 'expired')),
    max_uses         INTEGER,
    use_count        INTEGER NOT NULL DEFAULT 0,
    created_by       TEXT NOT NULL,
    expires_at       TIMESTAMPTZ NOT NULL,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK ((status = 'active') = (token IS NOT NULL)),
    CHECK (max_uses IS NULL OR max_uses >= 1),
    CHECK (use_count >= 0)
);

CREATE UNIQUE INDEX organization_invites_token_hash_uidx
    ON organization_invites (token_hash);

CREATE INDEX organization_invites_org_created_idx
    ON organization_invites (organization_id, created_at DESC);

CREATE TABLE user_api_keys (
    id          TEXT PRIMARY KEY,
    user_id     TEXT NOT NULL,
    name        TEXT NOT NULL,
    key_prefix  TEXT NOT NULL,
    key_hash    TEXT NOT NULL,
    scopes      TEXT[],
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    revoked_at  TIMESTAMPTZ
);

CREATE UNIQUE INDEX user_api_keys_key_hash_uidx ON user_api_keys (key_hash);
CREATE INDEX user_api_keys_user_id_idx ON user_api_keys (user_id);

CREATE TABLE member_api_keys (
    id          TEXT PRIMARY KEY,
    member_id   TEXT NOT NULL REFERENCES members (id) ON DELETE CASCADE,
    name        TEXT NOT NULL,
    key_prefix  TEXT NOT NULL,
    key_hash    TEXT NOT NULL,
    scopes      TEXT[],
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    revoked_at  TIMESTAMPTZ
);

CREATE UNIQUE INDEX member_api_keys_key_hash_uidx ON member_api_keys (key_hash);
CREATE INDEX member_api_keys_member_id_idx ON member_api_keys (member_id);
