# Identity schema

How identity stores the model. For implementers; clients depend on the [service API](../reference/api/identity.md), not on this.

A fresh schema, `identity`.

```sql
CREATE TABLE nodes (
    id           TEXT PRIMARY KEY,
    kind         TEXT NOT NULL,
    parent_id    TEXT REFERENCES nodes (id),
    root_id      TEXT NOT NULL REFERENCES nodes (id),   -- itself for a root
    slug         TEXT,                                  -- NULL once a purge or a release frees it
    display_name TEXT NOT NULL,
    status       TEXT NOT NULL CHECK (status IN ('active', 'deleted', 'purged')),
    rate_limit_tier TEXT,                               -- root nodes only: opaque tier name
    deleted_at   TIMESTAMPTZ,
    purge_after  TIMESTAMPTZ,                           -- deleted_at + deletion_grace, fixed at delete
    deletion_id  TEXT,                                  -- one per delete; restore undoes exactly that one
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (parent_id IS NULL OR rate_limit_tier IS NULL)
);
-- A purged root keeps its slug as a tombstone; a purged node below a root frees it.
CREATE UNIQUE INDEX nodes_slug_uidx ON nodes (COALESCE(parent_id, ''), kind, slug) WHERE slug IS NOT NULL;

CREATE TABLE node_ancestors (                         -- closure table, depth 0 = itself
    node_id      TEXT NOT NULL REFERENCES nodes (id),
    ancestor_id  TEXT NOT NULL REFERENCES nodes (id),
    depth        INT  NOT NULL,
    PRIMARY KEY (node_id, ancestor_id)
);

CREATE TABLE principals (
    id            TEXT PRIMARY KEY,                    -- never sent outside identity
    kind          TEXT NOT NULL CHECK (kind IN ('person', 'service_account')),
    user_id       TEXT UNIQUE,                         -- persons only: the id user routes carry
    status        TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'suspended')),
    name          TEXT,                                -- a service account's name; a person's, null until known
    node_id       TEXT REFERENCES nodes (id),          -- service accounts only
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK ((kind = 'service_account') = (node_id IS NOT NULL)),
    CHECK ((kind = 'person') = (user_id IS NOT NULL))
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
    suspended_at  TIMESTAMPTZ,                         -- reversible per-tree suspension
    removed_at    TIMESTAMPTZ,                         -- set once; never cleared
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (removed_at IS NOT NULL OR principal_id IS NOT NULL)  -- an active member always has its principal
);
CREATE UNIQUE INDEX members_active_uidx ON members (principal_id, root_id) WHERE removed_at IS NULL;

CREATE TABLE groups (                                  -- flat collections of active members in one tree
    id          TEXT PRIMARY KEY,
    root_id     TEXT NOT NULL REFERENCES nodes (id),
    name        TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE group_members (
    group_id    TEXT NOT NULL REFERENCES groups (id) ON DELETE CASCADE,
    member_id   TEXT NOT NULL REFERENCES members (id) ON DELETE CASCADE,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (group_id, member_id)
);

CREATE TABLE bindings (
    id          TEXT PRIMARY KEY,
    member_id   TEXT REFERENCES members (id),
    group_id    TEXT REFERENCES groups (id) ON DELETE CASCADE,
    node_id     TEXT NOT NULL REFERENCES nodes (id),   -- under the member's or group's root
    role        TEXT NOT NULL,                         -- grants nothing if the role is gone
    expires_at  TIMESTAMPTZ,                           -- optional JIT expiry
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK ((member_id IS NOT NULL) <> (group_id IS NOT NULL))
);
CREATE UNIQUE INDEX bindings_member_role_uidx ON bindings (member_id, node_id, role) WHERE member_id IS NOT NULL;
CREATE UNIQUE INDEX bindings_group_role_uidx ON bindings (group_id, node_id, role) WHERE group_id IS NOT NULL;

CREATE TABLE api_keys (
    id            TEXT PRIMARY KEY,
    principal_id  TEXT NOT NULL REFERENCES principals (id) ON DELETE CASCADE,
    name          TEXT NOT NULL,
    key_prefix    TEXT NOT NULL,
    key_hash      TEXT NOT NULL UNIQUE,
    allowed_cidrs TEXT[] NOT NULL DEFAULT '{}',
    scopes        TEXT[] NOT NULL DEFAULT '{}',
    allowed_roots TEXT[] NOT NULL DEFAULT '{}',
    expires_at    TIMESTAMPTZ,
    revoked_at    TIMESTAMPTZ,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE invites (
    id           TEXT PRIMARY KEY,
    node_id      TEXT NOT NULL REFERENCES nodes (id),  -- below a root: can admit a guest
    role         TEXT NOT NULL,
    grants       JSONB NOT NULL DEFAULT '[]',         -- [{node_id, role}] under the same root
    token_hash   TEXT NOT NULL UNIQUE,                 -- the token is shown once, at create
    token_prefix TEXT NOT NULL,
    status       TEXT NOT NULL CHECK (status IN ('active', 'redeemed', 'revoked', 'expired')),
    max_uses     INT,                                  -- NULL: unlimited
    use_count    INT NOT NULL DEFAULT 0,
    expires_at   TIMESTAMPTZ NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE events (                                 -- append-only; outlives what it names
    id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    type        TEXT NOT NULL CHECK (type IN ('node.created', 'node.deleted', 'node.restored', 'node.purged',
                                              'member.created', 'member.removed',
                                              'user.created', 'user.deleted')),
    kind        TEXT,                                  -- node events: the node's kind
    node_id     TEXT,                                  -- node events
    member_id   TEXT,                                  -- member events
    root_id     TEXT,                                  -- node and member events
    principal   TEXT CHECK (principal IN ('person', 'service_account')),  -- member events
    user_id     TEXT,                                  -- user events
    occurred_at TIMESTAMPTZ NOT NULL DEFAULT now(),    -- the transaction's time
    -- Each type carries its own fields and no others.
    CHECK ((type LIKE 'node.%') = (kind IS NOT NULL AND node_id IS NOT NULL)),
    CHECK ((type LIKE 'member.%') = (member_id IS NOT NULL AND principal IS NOT NULL)),
    CHECK ((type LIKE 'user.%') = (user_id IS NOT NULL)),
    CHECK ((type LIKE 'user.%') = (root_id IS NULL)),
    CHECK (type LIKE 'node.%' OR (kind IS NULL AND node_id IS NULL)),
    CHECK (type LIKE 'member.%' OR (member_id IS NULL AND principal IS NULL))
);

CREATE TABLE admit_invalidations (                    -- commit-ordered cache invalidations; pruned on sweep
    id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    scope       TEXT NOT NULL CHECK (scope IN ('key', 'user', 'member', 'root')),
    target      TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX admit_invalidations_created_idx ON admit_invalidations (created_at);
```

Ids are lowercase ULIDs with no prefix ([ids](../concepts/model.md#ids-and-slugs)). Principal ids never leave identity; a person's `user_id` is a separate id, sent only on user routes and in `user.*` events.

### Invariants the code enforces

The rules themselves are in the concept docs ([access](../concepts/access.md), [lifecycle](../concepts/lifecycle.md)). What the tables hold to:

- **Members.** At most one active member per principal per tree (`members_active_uidx`). `removed_at` is set once and never cleared. An active member always has its principal (the `CHECK`): only a removed member's `principal_id` is ever `NULL`.
- **Guests.** Not stored; read from the bindings.
- **Closed world.** A binding's node is under its member's root. A service account's member is in the tree of its `node_id`.
- **Roles.** A binding's `role` is kept as written. A role that's missing, or not bindable at the binding's kind, grants nothing.
- **Deletions.** Each delete stamps one `deletion_id` on every node it soft-deletes; restore matches on it. A node already deleted keeps its own id.
- **Purge.** Purge sets `status = 'purged'` and keeps the row. Below a root it sets `slug` to `NULL`, freeing it; a purged root keeps its slug until an operator releases it.
- **Writes that touch members** take the tree's lock, so "one active member" and "removed when left with no bindings" can't race.
- **Account deletion** removes the members it found with their trees locked, then locks the principal row `FOR UPDATE`, removes any member that a join inserted meanwhile (the join's foreign key held the row, so the lock waited for it), and deletes the principal. A join that inserts after the lock waits on the row and then fails the foreign key.
- **Events and invalidations.** A change writes its lifecycle events and admit invalidations after every other write, taking the one serialization advisory lock held to commit so sequence ids commit in order, a reader never sees a later id before an earlier one commits, and concurrent writes cannot deadlock. A person's row and its `user.created` are written together, as are the principal's deletion and its `user.deleted`. Old `admit_invalidations` rows are pruned on the background sweep; any gateway cursor older than the oldest remaining row gets `reset: true`.
