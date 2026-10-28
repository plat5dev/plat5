-- Identity's schema (docs/internals/identity-schema.md).

CREATE TABLE nodes (
    id           TEXT PRIMARY KEY,
    kind         TEXT NOT NULL,
    parent_id    TEXT REFERENCES nodes (id),
    root_id      TEXT NOT NULL REFERENCES nodes (id),
    slug         TEXT,
    display_name TEXT NOT NULL,
    status       TEXT NOT NULL CHECK (status IN ('active', 'deleted', 'purged')),
    deleted_at   TIMESTAMPTZ,
    purge_after  TIMESTAMPTZ,
    deletion_id  TEXT,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- A purged root keeps its slug as a tombstone; a purged node below a root frees it.
CREATE UNIQUE INDEX nodes_slug_uidx ON nodes (COALESCE(parent_id, ''), kind, slug) WHERE slug IS NOT NULL;
CREATE INDEX nodes_parent_idx ON nodes (parent_id, kind);
CREATE INDEX nodes_purge_idx ON nodes (purge_after) WHERE status = 'deleted';
CREATE INDEX nodes_deletion_idx ON nodes (deletion_id) WHERE deletion_id IS NOT NULL;

CREATE TABLE node_ancestors (
    node_id      TEXT NOT NULL REFERENCES nodes (id),
    ancestor_id  TEXT NOT NULL REFERENCES nodes (id),
    depth        INT  NOT NULL,
    PRIMARY KEY (node_id, ancestor_id)
);
CREATE INDEX node_ancestors_ancestor_idx ON node_ancestors (ancestor_id, depth);

CREATE TABLE principals (
    id            TEXT PRIMARY KEY,
    kind          TEXT NOT NULL CHECK (kind IN ('person', 'service_account')),
    user_id       TEXT UNIQUE,
    status        TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'suspended')),
    name          TEXT,
    node_id       TEXT REFERENCES nodes (id),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK ((kind = 'service_account') = (node_id IS NOT NULL)),
    CHECK ((kind = 'person') = (user_id IS NOT NULL))
);
CREATE INDEX principals_node_idx ON principals (node_id) WHERE node_id IS NOT NULL;

CREATE TABLE idp_links (
    issuer TEXT NOT NULL, subject TEXT NOT NULL,
    principal_id TEXT NOT NULL REFERENCES principals (id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (issuer, subject)
);

CREATE TABLE members (
    id            TEXT PRIMARY KEY,
    principal_id  TEXT REFERENCES principals (id) ON DELETE SET NULL,
    -- The principal's kind outlives the principal: a removed service account
    -- or a deleted account still reads as one.
    principal     TEXT NOT NULL CHECK (principal IN ('person', 'service_account')),
    root_id       TEXT NOT NULL REFERENCES nodes (id),
    removed_at    TIMESTAMPTZ,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX members_active_uidx ON members (principal_id, root_id) WHERE removed_at IS NULL;
CREATE INDEX members_root_idx ON members (root_id) WHERE removed_at IS NULL;
CREATE INDEX members_principal_idx ON members (principal_id) WHERE removed_at IS NULL;

CREATE TABLE bindings (
    id          TEXT PRIMARY KEY,
    member_id   TEXT NOT NULL REFERENCES members (id),
    node_id     TEXT NOT NULL REFERENCES nodes (id),
    role        TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX bindings_uidx ON bindings (member_id, node_id);
CREATE INDEX bindings_node_idx ON bindings (node_id);
CREATE INDEX bindings_member_idx ON bindings (member_id);

CREATE TABLE api_keys (
    id            TEXT PRIMARY KEY,
    principal_id  TEXT NOT NULL REFERENCES principals (id) ON DELETE CASCADE,
    name          TEXT NOT NULL,
    key_prefix    TEXT NOT NULL,
    key_hash      TEXT NOT NULL UNIQUE,
    revoked_at    TIMESTAMPTZ,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX api_keys_principal_idx ON api_keys (principal_id);

CREATE TABLE invites (
    id           TEXT PRIMARY KEY,
    node_id      TEXT NOT NULL REFERENCES nodes (id),
    role         TEXT NOT NULL,
    grants       JSONB NOT NULL DEFAULT '[]',
    token_hash   TEXT NOT NULL UNIQUE,
    token_prefix TEXT NOT NULL,
    status       TEXT NOT NULL CHECK (status IN ('active', 'redeemed', 'revoked', 'expired')),
    max_uses     INT, use_count INT NOT NULL DEFAULT 0,
    expires_at   TIMESTAMPTZ NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX invites_node_idx ON invites (node_id);
CREATE INDEX invites_expiry_idx ON invites (expires_at) WHERE status = 'active';

CREATE TABLE tree_events (
    id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    type        TEXT NOT NULL CHECK (type IN ('node.created', 'node.deleted', 'node.restored', 'node.purged',
                                              'member.created', 'member.removed')),
    kind        TEXT,
    node_id     TEXT,
    member_id   TEXT,
    root_id     TEXT NOT NULL,
    principal   TEXT CHECK (principal IN ('person', 'service_account')),
    occurred_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Its own sequence: nothing joins a user event to a tree event.
CREATE TABLE user_events (
    id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    type        TEXT NOT NULL CHECK (type IN ('user.created', 'user.deleted')),
    user_id     TEXT NOT NULL,
    occurred_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
