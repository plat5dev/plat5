-- v2 step 3 (docs/internals/identity-schema.md): what members and invites
-- need beyond step 2's tables.

-- The member's principal kind outlives the principal: a removed service
-- account or a deleted account still reads as one.
ALTER TABLE members ADD COLUMN principal TEXT NOT NULL CHECK (principal IN ('person', 'service_account'));
CREATE INDEX members_principal_idx ON members (principal_id) WHERE removed_at IS NULL;

ALTER TABLE invites ADD COLUMN role_duration TEXT;
CREATE INDEX invites_node_idx ON invites (node_id);
CREATE INDEX invites_expiry_idx ON invites (expires_at) WHERE status = 'active';

CREATE INDEX bindings_expiry_idx ON bindings (expires_at) WHERE expires_at IS NOT NULL;
CREATE INDEX bindings_member_idx ON bindings (member_id);
