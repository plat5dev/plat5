-- Realms (docs/model.md): every person lives in the directory, organizations
-- are tenants, and every credential belongs to one membership in one realm.
-- Existing ids carry over: a person's user_id stays their IdP subject until
-- they first sign in again, member, service account, key, and session ids are
-- unchanged.

-- An identity is a person or a service account. A person's status holds in
-- every realm. A service account's standing is its membership; its own
-- status stays active.
CREATE TABLE identities (
    id               TEXT PRIMARY KEY,
    kind             TEXT NOT NULL CHECK (kind IN ('user', 'service_account')),
    status           TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'suspended')),
    organization_id  TEXT REFERENCES organizations (id) ON DELETE CASCADE,
    name             TEXT,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK ((kind = 'service_account') = (organization_id IS NOT NULL)),
    CHECK ((kind = 'service_account') = (name IS NOT NULL)),
    CHECK (kind = 'user' OR status = 'active')
);

CREATE INDEX identities_organization_id_idx
    ON identities (organization_id)
    WHERE organization_id IS NOT NULL;

-- A person's IdP subjects. A person carried over from before realms has none
-- until their first sign-in links the subject that is their id.
CREATE TABLE idp_links (
    issuer       TEXT NOT NULL,
    subject      TEXT NOT NULL,
    identity_id  TEXT NOT NULL REFERENCES identities (id) ON DELETE CASCADE,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (issuer, subject)
);

CREATE INDEX idp_links_identity_id_idx ON idp_links (identity_id);

-- An identity's standing in a realm. organization_id NULL is the directory:
-- the person's anchor, with the same id as the person, no role and no status.
-- An org membership is a member. Account deletion clears identity_id on the
-- person's removed members, so principal is kept on the row.
CREATE TABLE memberships (
    id               TEXT PRIMARY KEY,
    identity_id      TEXT REFERENCES identities (id) ON DELETE SET NULL,
    organization_id  TEXT REFERENCES organizations (id) ON DELETE CASCADE,
    principal        TEXT NOT NULL CHECK (principal IN ('user', 'service_account')),
    display_name     TEXT,
    role             TEXT,
    status           TEXT CHECK (status IN ('active', 'suspended', 'removed')),
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK ((organization_id IS NULL) = (status IS NULL)),
    CHECK (organization_id IS NOT NULL OR (
        id = identity_id AND principal = 'user' AND role IS NULL
    )),
    CHECK (identity_id IS NOT NULL OR status = 'removed')
);

CREATE UNIQUE INDEX memberships_directory_uidx
    ON memberships (identity_id)
    WHERE organization_id IS NULL;

CREATE UNIQUE INDEX memberships_org_identity_uidx
    ON memberships (organization_id, identity_id)
    WHERE organization_id IS NOT NULL AND status <> 'removed';

CREATE UNIQUE INDEX memberships_service_account_uidx
    ON memberships (identity_id)
    WHERE principal = 'service_account';

CREATE INDEX memberships_identity_id_idx ON memberships (identity_id);

CREATE INDEX memberships_org_status_idx
    ON memberships (organization_id, status)
    WHERE organization_id IS NOT NULL;

-- Every API key and session. A credential carries its membership's
-- permissions and has none of its own (docs/roles.md). key_prefix is the
-- display prefix for both kinds.
CREATE TABLE credentials (
    id             TEXT PRIMARY KEY,
    membership_id  TEXT NOT NULL REFERENCES memberships (id) ON DELETE CASCADE,
    kind           TEXT NOT NULL CHECK (kind IN ('key', 'session')),
    name           TEXT,
    key_prefix     TEXT NOT NULL,
    key_hash       TEXT NOT NULL,
    expires_at     TIMESTAMPTZ,
    revoked_at     TIMESTAMPTZ,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK ((kind = 'key') = (name IS NOT NULL)),
    CHECK ((kind = 'session') = (expires_at IS NOT NULL)),
    CHECK (kind = 'key' OR revoked_at IS NULL)
);

CREATE UNIQUE INDEX credentials_key_hash_uidx ON credentials (key_hash);
CREATE INDEX credentials_membership_kind_idx ON credentials (membership_id, kind, id);

-- Carry over. Persons are every user id seen on a member or a user key.
INSERT INTO identities (id, kind, created_at, updated_at)
SELECT user_id, 'user', min(created_at), min(created_at)
FROM (
    SELECT user_id, created_at FROM members WHERE user_id IS NOT NULL
    UNION ALL
    SELECT user_id, created_at FROM user_api_keys
) persons
GROUP BY user_id;

INSERT INTO memberships (id, identity_id, organization_id, principal, created_at, updated_at)
SELECT id, id, NULL, 'user', created_at, updated_at
FROM identities;

INSERT INTO identities (id, kind, organization_id, name, created_at, updated_at)
SELECT id, 'service_account', organization_id, name, created_at, updated_at
FROM service_accounts;

INSERT INTO memberships (id, identity_id, organization_id, principal, role, status, created_at, updated_at)
SELECT
    id,
    COALESCE(user_id, service_account_id),
    organization_id,
    CASE WHEN user_id IS NOT NULL THEN 'user' ELSE 'service_account' END,
    role,
    status,
    created_at,
    updated_at
FROM members;

INSERT INTO credentials (id, membership_id, kind, name, key_prefix, key_hash, revoked_at, created_at)
SELECT id, user_id, 'key', name, key_prefix, key_hash, revoked_at, created_at
FROM user_api_keys;

INSERT INTO credentials (id, membership_id, kind, name, key_prefix, key_hash, revoked_at, created_at)
SELECT id, member_id, 'key', name, key_prefix, key_hash, revoked_at, created_at
FROM member_api_keys;

INSERT INTO credentials (id, membership_id, kind, key_prefix, key_hash, expires_at, created_at)
SELECT id, member_id, 'session', token_prefix, token_hash, expires_at, created_at
FROM member_sessions
WHERE expires_at > now();

DROP TABLE member_sessions;
DROP TABLE member_api_keys;
DROP TABLE user_api_keys;
DROP TABLE members;
DROP TABLE service_accounts;
