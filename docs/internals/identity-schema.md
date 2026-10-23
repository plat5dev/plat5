# Identity schema

How identity stores the model. For implementers; clients depend on the [service API](../reference/api/identity.md), not on this.

A fresh schema, `identity`.

```sql
CREATE TABLE nodes (
    id          TEXT PRIMARY KEY,
    kind        TEXT NOT NULL,
    parent_id   TEXT REFERENCES nodes (id),
    root_id     TEXT NOT NULL REFERENCES nodes (id),   -- itself for a root
    slug        TEXT,                                  -- NULL only for the sign-in node, or once released
    label       TEXT NOT NULL,
    status      TEXT NOT NULL CHECK (status IN ('active', 'deleted', 'purged')),
    deleted_at  TIMESTAMPTZ,
    purge_after TIMESTAMPTZ,                           -- deleted_at + deletion_grace, fixed at delete
    deletion_id TEXT,                                  -- one per delete or kind removal; restore undoes exactly that one
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- Slugs never reused: purged rows stay as tombstones and keep the index.
CREATE UNIQUE INDEX nodes_slug_uidx ON nodes (COALESCE(parent_id, ''), kind, slug) WHERE slug IS NOT NULL;

CREATE TABLE node_ancestors (                         -- closure table, depth 0 = itself
    node_id      TEXT NOT NULL REFERENCES nodes (id),
    ancestor_id  TEXT NOT NULL REFERENCES nodes (id),
    depth        INT  NOT NULL,
    PRIMARY KEY (node_id, ancestor_id)
);

CREATE TABLE principals (
    id            TEXT PRIMARY KEY,                    -- never sent to services
    kind          TEXT NOT NULL CHECK (kind IN ('person', 'service_account')),
    status        TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'suspended')),
    name          TEXT,                                -- a service account's name; a person's from name_claim
    born_node_id  TEXT REFERENCES nodes (id),          -- service accounts only
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK ((kind = 'service_account') = (born_node_id IS NOT NULL))
);

CREATE TABLE idp_links (
    issuer TEXT NOT NULL, subject TEXT NOT NULL,
    principal_id TEXT NOT NULL REFERENCES principals (id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (issuer, subject)
);

CREATE TABLE members (                                -- one per principal per tree: the id services see
    id            TEXT PRIMARY KEY,
    principal_id  TEXT REFERENCES principals (id) ON DELETE SET NULL,
    principal     TEXT NOT NULL CHECK (principal IN ('person', 'service_account')),  -- outlives the principal
    root_id       TEXT NOT NULL REFERENCES nodes (id),
    removed_at    TIMESTAMPTZ,                         -- set once; never cleared
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX members_active_uidx ON members (principal_id, root_id) WHERE removed_at IS NULL;

CREATE TABLE bindings (
    id          TEXT PRIMARY KEY,
    member_id   TEXT NOT NULL REFERENCES members (id),
    node_id     TEXT NOT NULL REFERENCES nodes (id),   -- under the member's root
    role        TEXT NOT NULL,                         -- grants nothing if the role is gone
    expires_at  TIMESTAMPTZ,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX bindings_uidx ON bindings (member_id, node_id);

CREATE TABLE api_keys (
    id            TEXT PRIMARY KEY,
    principal_id  TEXT NOT NULL REFERENCES principals (id) ON DELETE CASCADE,
    name          TEXT NOT NULL,
    key_prefix    TEXT NOT NULL,
    key_hash      TEXT NOT NULL UNIQUE,
    revoked_at    TIMESTAMPTZ,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE invites (
    id           TEXT PRIMARY KEY,
    node_id      TEXT NOT NULL REFERENCES nodes (id),  -- below a root: can admit a guest
    role         TEXT NOT NULL,                        -- default_role when made, if none was given
    role_duration TEXT,                                -- a config duration, counted from redeem
    grants       JSONB NOT NULL DEFAULT '[]',         -- [{node_id, role, duration}] under the same root
    token_hash   TEXT NOT NULL UNIQUE,                 -- the token is shown once, at create
    token_prefix TEXT NOT NULL,
    status       TEXT NOT NULL CHECK (status IN ('active', 'redeemed', 'revoked', 'expired')),
    max_uses     INT, use_count INT NOT NULL DEFAULT 0,
    expires_at   TIMESTAMPTZ NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE lifecycle_events (                       -- append-only; outlives what it names
    id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    type        TEXT NOT NULL CHECK (type IN ('node.created', 'node.deleted', 'node.restored', 'node.purged',
                                              'member.created', 'member.removed')),
    kind        TEXT,                -- node events: the node's kind; on the wire: project.deleted
    node_id     TEXT,
    member_id   TEXT,
    root_id     TEXT NOT NULL,
    principal   TEXT CHECK (principal IN ('person', 'service_account')),
    occurred_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE kind_removals (                          -- a kind removed from the config, until it's back or purged
    kind        TEXT PRIMARY KEY,
    deletion_id TEXT NOT NULL,                         -- stamped on every node the removal soft-deleted
    removed_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
```

Ids are ULIDs with a type prefix: `n_` nodes, `m_` members, `b_` bindings, `k_` keys, `i_` invites. Principal ids never leave identity.

### Invariants the code enforces

- **Members.** One active member per principal per tree. In an org's tree it's created with its first binding; in the directory, with the person. Removal sets `removed_at`, deletes its bindings and emits `member.removed`; it's never undone, and re-adding makes a new member. A person's member outside the directory left with no bindings, by delete or expiry, is removed. A service account's member is removed only with the service account (deleted directly, or with its born node: either way its principal and keys are deleted); with no bindings its keys still authenticate and are refused everywhere, so a team can re-bind it.
- **Guests.** A member with no binding at the root. Not stored; read from the bindings.
- **Closed world.** A binding's node is under its member's root. A service account's member is in the tree of its `born_node_id`.
- **Directory.** One sign-in node, created at boot. Every person has one member there and no bindings, created with the person; that member's id is their user id.
- **Roles.** A binding's `role` names a role in the roles document. A role that's missing, or not bindable at the binding's kind, grants nothing.
- **Expiry.** A binding past `expires_at` grants nothing; a sweeper deletes it, and marks invites past their `expires_at` `expired`.
- **Invites.** A node's delete revokes the active invites on it and below; restore leaves them revoked.
- **Suspension.** A suspended principal's credentials are all invalid. There's no per-member suspension: remove the member instead.
- **Deletions.** Each delete, and each kind removal, stamps one `deletion_id` on every node it soft-deletes. A restore, or a kind applied again, restores exactly the nodes with that id. A node already deleted keeps its own id.
- **Kind removals.** Every config identity reads is checked against the nodes. Live nodes of a kind it lacks are soft-deleted with their descendants under one `deletion_id`, recorded in `kind_removals` in the same transaction. A kind in the config with a row restores the nodes with that id still before `purge_after` (not those under a parent deleted since) and drops the row. The sweeper drops a row once none of its nodes is left unpurged.
- **Deletion.** A node with `status = 'deleted'` grants nothing on it or below, except on a `resolve_deleted` route (restore, listing deleted). Each deleted node stores `purge_after`, set from `deletion_grace` when it's deleted, so changing `deletion_grace` applies to new deletes only. Once `purge_after` has passed, the purge sweeper sets `status = 'purged'`, deletes bindings on it and below, removes members of a purged root, and keeps the row.
