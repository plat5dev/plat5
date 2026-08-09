CREATE TABLE IF NOT EXISTS organizations (
    id          TEXT PRIMARY KEY,
    name        TEXT NOT NULL,
    slug        TEXT NOT NULL,
    settings    JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX IF NOT EXISTS organizations_slug_uidx ON organizations (slug);

CREATE TABLE IF NOT EXISTS organization_memberships (
    id               TEXT PRIMARY KEY,
    organization_id  TEXT NOT NULL REFERENCES organizations (id) ON DELETE CASCADE,
    user_id          TEXT NOT NULL,
    role             TEXT NOT NULL CHECK (role IN ('member', 'admin', 'owner')),
    status           TEXT NOT NULL CHECK (status IN ('pending', 'active', 'suspended', 'removed')),
    invited_by       TEXT,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (organization_id, user_id)
);

CREATE INDEX IF NOT EXISTS organization_memberships_user_id_idx
    ON organization_memberships (user_id);

CREATE INDEX IF NOT EXISTS organization_memberships_org_status_idx
    ON organization_memberships (organization_id, status);
