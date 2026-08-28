-- Invite lifecycle is status. Drop one-shot redeem/revoke timestamps.
ALTER TABLE organization_invites
    DROP COLUMN IF EXISTS redeemed_at,
    DROP COLUMN IF EXISTS redeemed_by,
    DROP COLUMN IF EXISTS revoked_at;
