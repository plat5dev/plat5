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
