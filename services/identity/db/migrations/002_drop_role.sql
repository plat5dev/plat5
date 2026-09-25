-- Role is gone. Checks that mention the column go with it.
ALTER TABLE members DROP COLUMN role CASCADE;
ALTER TABLE organization_invites DROP COLUMN role CASCADE;
ALTER TABLE organization_invites ALTER COLUMN created_by DROP NOT NULL;
