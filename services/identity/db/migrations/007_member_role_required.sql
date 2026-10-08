-- Every member holds a role from the roles file (docs/roles.md). ROLES_FILE is
-- required, and boot passes its slugs in as plat5.creator_role and
-- plat5.default_role.
--
-- A NULL role was unrestricted. Backfill members with creator_role so no one
-- loses access on upgrade and every org keeps a creator_role holder. An invite
-- with no role gets default_role, what an omitted role means today.
UPDATE members SET role = current_setting('plat5.creator_role') WHERE role IS NULL;
UPDATE organization_invites SET role = current_setting('plat5.default_role') WHERE role IS NULL;

ALTER TABLE members ALTER COLUMN role SET NOT NULL;
ALTER TABLE organization_invites ALTER COLUMN role SET NOT NULL;
