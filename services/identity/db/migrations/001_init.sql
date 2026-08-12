CREATE TABLE IF NOT EXISTS organizations (
    id          TEXT PRIMARY KEY,
    name        TEXT NOT NULL,
    slug        TEXT NOT NULL,
    settings    JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX IF NOT EXISTS organizations_slug_uidx ON organizations (slug);

CREATE TABLE IF NOT EXISTS service_accounts (
    id                    TEXT PRIMARY KEY,
    home_organization_id  TEXT NOT NULL REFERENCES organizations (id) ON DELETE CASCADE,
    name                  TEXT NOT NULL,
    created_by_user_id    TEXT,
    disabled_at           TIMESTAMPTZ,
    created_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at            TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS service_accounts_home_org_idx
    ON service_accounts (home_organization_id);

CREATE TABLE IF NOT EXISTS members (
    id                  TEXT PRIMARY KEY,
    organization_id     TEXT NOT NULL REFERENCES organizations (id) ON DELETE CASCADE,
    user_id             TEXT,
    service_account_id  TEXT REFERENCES service_accounts (id) ON DELETE CASCADE,
    role                TEXT NOT NULL CHECK (role IN ('member', 'admin', 'owner')),
    status              TEXT NOT NULL CHECK (status IN ('pending', 'active', 'suspended', 'removed')),
    invited_by          TEXT,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (
        (user_id IS NOT NULL AND service_account_id IS NULL)
        OR (user_id IS NULL AND service_account_id IS NOT NULL)
    ),
    CHECK (NOT (service_account_id IS NOT NULL AND role = 'owner'))
);

CREATE UNIQUE INDEX IF NOT EXISTS members_org_user_uidx
    ON members (organization_id, user_id)
    WHERE user_id IS NOT NULL;

CREATE UNIQUE INDEX IF NOT EXISTS members_org_sa_uidx
    ON members (organization_id, service_account_id)
    WHERE service_account_id IS NOT NULL;

CREATE INDEX IF NOT EXISTS members_user_id_idx
    ON members (user_id)
    WHERE user_id IS NOT NULL;

CREATE INDEX IF NOT EXISTS members_org_status_idx
    ON members (organization_id, status);

-- User-scoped keys (user-scope gateway routes). Independent of member keys.
CREATE TABLE IF NOT EXISTS user_api_keys (
    id          TEXT PRIMARY KEY,
    user_id     TEXT NOT NULL,
    name        TEXT NOT NULL,
    key_prefix  TEXT NOT NULL,
    key_hash    TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    revoked_at  TIMESTAMPTZ
);

CREATE UNIQUE INDEX IF NOT EXISTS user_api_keys_key_hash_uidx ON user_api_keys (key_hash);
CREATE INDEX IF NOT EXISTS user_api_keys_user_id_idx ON user_api_keys (user_id);

-- Member-scoped keys (organization-scope gateway routes). Independent of user keys.
CREATE TABLE IF NOT EXISTS member_api_keys (
    id          TEXT PRIMARY KEY,
    member_id   TEXT NOT NULL REFERENCES members (id) ON DELETE CASCADE,
    name        TEXT NOT NULL,
    key_prefix  TEXT NOT NULL,
    key_hash    TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    revoked_at  TIMESTAMPTZ
);

CREATE UNIQUE INDEX IF NOT EXISTS member_api_keys_key_hash_uidx ON member_api_keys (key_hash);
CREATE INDEX IF NOT EXISTS member_api_keys_member_id_idx ON member_api_keys (member_id);
