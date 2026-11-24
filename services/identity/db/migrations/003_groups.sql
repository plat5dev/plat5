-- In-tree groups and group bindings (docs/concepts/access.md, docs/internals/identity-schema.md).

CREATE TABLE groups (
    id         TEXT PRIMARY KEY,
    root_id    TEXT NOT NULL REFERENCES nodes (id),
    name       TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX groups_root_idx ON groups (root_id);

CREATE TABLE group_members (
    group_id   TEXT NOT NULL REFERENCES groups (id) ON DELETE CASCADE,
    member_id  TEXT NOT NULL REFERENCES members (id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (group_id, member_id)
);
CREATE INDEX group_members_member_idx ON group_members (member_id);

ALTER TABLE bindings
    ALTER COLUMN member_id DROP NOT NULL,
    ADD COLUMN group_id TEXT REFERENCES groups (id) ON DELETE CASCADE,
    ADD CONSTRAINT bindings_subject_chk CHECK ((member_id IS NOT NULL) <> (group_id IS NOT NULL));

DROP INDEX bindings_uidx;
CREATE UNIQUE INDEX bindings_member_uidx ON bindings (member_id, node_id) WHERE member_id IS NOT NULL;
CREATE UNIQUE INDEX bindings_group_uidx ON bindings (group_id, node_id) WHERE group_id IS NOT NULL;
CREATE INDEX bindings_group_idx ON bindings (group_id) WHERE group_id IS NOT NULL;
