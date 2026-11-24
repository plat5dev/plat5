-- Tier 1: per-tree member suspension, multiple & expiring bindings, API key
-- TTLs/scopes/root allowlists, and opaque root rate-limit tiers.

-- 1. Root rate-limit tiers (opaque string; root nodes only).
ALTER TABLE nodes ADD COLUMN rate_limit_tier TEXT;
ALTER TABLE nodes ADD CONSTRAINT nodes_rate_limit_tier_root_chk CHECK (parent_id IS NULL OR rate_limit_tier IS NULL);

-- 2. Reversible per-tree member suspension.
ALTER TABLE members ADD COLUMN suspended_at TIMESTAMPTZ;

-- 3. Multiple distinct role bindings per member/group per node, with optional expiry.
ALTER TABLE bindings ADD COLUMN expires_at TIMESTAMPTZ;
CREATE INDEX bindings_expiry_idx ON bindings (expires_at) WHERE expires_at IS NOT NULL;

DROP INDEX bindings_member_uidx;
DROP INDEX bindings_group_uidx;
CREATE UNIQUE INDEX bindings_member_role_uidx ON bindings (member_id, node_id, role) WHERE member_id IS NOT NULL;
CREATE UNIQUE INDEX bindings_group_role_uidx ON bindings (group_id, node_id, role) WHERE group_id IS NOT NULL;

-- 4. API key TTLs, permission scopes, and root allowlists.
ALTER TABLE api_keys
    ADD COLUMN scopes        TEXT[] NOT NULL DEFAULT '{}',
    ADD COLUMN allowed_roots TEXT[] NOT NULL DEFAULT '{}',
    ADD COLUMN expires_at    TIMESTAMPTZ;
