-- Roles are the deployment's (docs/roles.md). The roles file gives a slug its
-- meaning, so there is no CHECK. NULL = unrestricted: every member without a
-- roles file, and every row from before one.
ALTER TABLE members ADD COLUMN role TEXT;
ALTER TABLE organization_invites ADD COLUMN role TEXT;
