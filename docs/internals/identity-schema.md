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
    deleted_at   TIMESTAMPTZ,
    purge_after  TIMESTAMPTZ,                           -- deleted_at + deletion_grace, fixed at delete
    deletion_id  TEXT,                                  -- one per delete; restore undoes exactly that one
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
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
    name          TEXT,                                -- a service account's name; a person's from name_claim
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
    removed_at    TIMESTAMPTZ,                         -- set once; never cleared
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX members_active_uidx ON members (principal_id, root_id) WHERE removed_at IS NULL;

CREATE TABLE bindings (
    id          TEXT PRIMARY KEY,
    member_id   TEXT NOT NULL REFERENCES members (id),
    node_id     TEXT NOT NULL REFERENCES nodes (id),   -- under the member's root
    role        TEXT NOT NULL,                         -- grants nothing if the role is gone
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

CREATE TABLE tree_events (                            -- append-only; outlives what it names
    id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    type        TEXT NOT NULL CHECK (type IN ('node.created', 'node.deleted', 'node.restored', 'node.purged',
                                              'member.created', 'member.removed')),
    kind        TEXT,                                  -- node events: the node's kind
    node_id     TEXT,
    member_id   TEXT,
    root_id     TEXT NOT NULL,
    principal   TEXT CHECK (principal IN ('person', 'service_account')),
    occurred_at TIMESTAMPTZ NOT NULL DEFAULT now()     -- the transaction's time
);

CREATE TABLE user_events (                            -- append-only; its own sequence, never joined to tree_events
    id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    type        TEXT NOT NULL CHECK (type IN ('user.created', 'user.deleted')),
    user_id     TEXT NOT NULL,
    occurred_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
```

Ids are lowercase ULIDs with no prefix ([ids](../concepts/model.md#ids-and-slugs)). Principal ids never leave identity; a person's `user_id` is a separate id, sent only on user routes and in user events.

### Invariants the code enforces

The rules themselves are in the concept docs ([access](../concepts/access.md), [lifecycle](../concepts/lifecycle.md)). What the tables hold to:

- **Members.** At most one active member per principal per tree (`members_active_uidx`). `removed_at` is set once and never cleared.
- **Guests.** Not stored; read from the bindings.
- **Closed world.** A binding's node is under its member's root. A service account's member is in the tree of its `node_id`.
- **Roles.** A binding's `role` is kept as written. A role that's missing, or not bindable at the binding's kind, grants nothing.
- **Deletions.** Each delete stamps one `deletion_id` on every node it soft-deletes; restore matches on it. A node already deleted keeps its own id.
- **Purge.** Purge sets `status = 'purged'` and keeps the row. Below a root it sets `slug` to `NULL`, freeing it; a purged root keeps its slug until an operator releases it.
- **Writes that touch members** take the tree's lock, so "one active member" and "removed when left with no bindings" can't race.
- **Events.** Each feed's writes take that feed's lock, held to commit, so a reader never sees a later id before an earlier one commits. A transaction that writes both takes the tree events' lock first. A person's row and its `user.created` are written together, as are the principal's deletion and its `user.deleted`.
