-- Roles are the deployment's (docs/roles.md). The roles file gives a slug its
-- meaning, so there is no CHECK. NULL is no role: every row while roles are off.
-- With roles on, a NULL role grants nothing until the operator assigns one.
ALTER TABLE members ADD COLUMN role TEXT;
ALTER TABLE organization_invites ADD COLUMN role TEXT;
