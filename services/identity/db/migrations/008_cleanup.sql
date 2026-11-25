-- 008_cleanup:
-- 1. Remove public_principal bindings (allow_public removed from the model).
-- 2. Drop redundant/unused indexes on bindings and members.
-- 3. Enforce case-insensitive unique custom role display_name per root.

DELETE FROM bindings WHERE public_principal IS NOT NULL;

DROP INDEX IF EXISTS bindings_public_role_uidx;
DROP INDEX IF EXISTS bindings_public_node_idx;

ALTER TABLE bindings DROP CONSTRAINT IF EXISTS bindings_subject_chk;
ALTER TABLE bindings DROP COLUMN IF EXISTS public_principal;
ALTER TABLE bindings ADD CONSTRAINT bindings_subject_chk CHECK (
    ((member_id IS NOT NULL)::int + (group_id IS NOT NULL)::int) = 1
);

-- Redundant with bindings_member_role_uidx, bindings_group_role_uidx, members_active_uidx,
-- and unused bindings_expiry_idx.
DROP INDEX IF EXISTS bindings_member_idx;
DROP INDEX IF EXISTS bindings_group_idx;
DROP INDEX IF EXISTS members_principal_idx;
DROP INDEX IF EXISTS bindings_expiry_idx;

-- Custom role display names must be unique per root (case-insensitive).
CREATE UNIQUE INDEX org_roles_root_name_uidx
    ON org_roles (root_id, lower(display_name));
