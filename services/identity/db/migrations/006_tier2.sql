-- Tier 2: per-root auth & security policies, per-root custom roles, and public
-- principal bindings (all_users, all_authenticated_users).

-- 1. Per-root auth & security policies (root nodes only).
CREATE TABLE root_security_policies (
    root_id             TEXT PRIMARY KEY REFERENCES nodes (id),
    allow_personal_keys BOOLEAN NOT NULL DEFAULT true,
    allowed_cidrs       TEXT[]  NOT NULL DEFAULT '{}',
    required_idp_claims JSONB   NOT NULL DEFAULT '{}'::jsonb,
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 2. Per-root custom roles (ULID-keyed; never collides with letter-prefixed predefined slugs).
CREATE TABLE org_roles (
    id           TEXT PRIMARY KEY,
    root_id      TEXT NOT NULL REFERENCES nodes (id),
    display_name TEXT NOT NULL,
    description  TEXT NOT NULL DEFAULT '',
    permissions  TEXT[] NOT NULL DEFAULT '{}',
    bindable_at  TEXT[] NOT NULL DEFAULT '{}',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX org_roles_root_idx ON org_roles (root_id);

-- 3. Public principal bindings (all_users, all_authenticated_users).
ALTER TABLE bindings
    ADD COLUMN public_principal TEXT CHECK (public_principal IN ('all_users', 'all_authenticated_users'));

ALTER TABLE bindings DROP CONSTRAINT bindings_subject_chk;
ALTER TABLE bindings ADD CONSTRAINT bindings_subject_chk CHECK (
    ((member_id IS NOT NULL)::int + (group_id IS NOT NULL)::int + (public_principal IS NOT NULL)::int) = 1
);

CREATE UNIQUE INDEX bindings_public_role_uidx
    ON bindings (public_principal, node_id, role)
    WHERE public_principal IS NOT NULL;
CREATE INDEX bindings_public_node_idx
    ON bindings (node_id, public_principal)
    WHERE public_principal IS NOT NULL;
