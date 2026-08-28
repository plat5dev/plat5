-- Plaintext token while active; status; max_uses / use_count.
-- Existing unused rows (hash+prefix only, no plaintext) are revoked so there
-- is no active row without token. Outstanding old copy-links die.

ALTER TABLE organization_invites
    ADD COLUMN IF NOT EXISTS token TEXT,
    ADD COLUMN IF NOT EXISTS status TEXT,
    ADD COLUMN IF NOT EXISTS max_uses INTEGER,
    ADD COLUMN IF NOT EXISTS use_count INTEGER NOT NULL DEFAULT 0;

UPDATE organization_invites
SET
    use_count = CASE WHEN redeemed_at IS NOT NULL THEN 1 ELSE 0 END,
    max_uses = 1,
    status = CASE
        WHEN redeemed_at IS NOT NULL THEN 'redeemed'
        WHEN revoked_at IS NOT NULL THEN 'revoked'
        WHEN expires_at <= now() THEN 'expired'
        ELSE 'active'
    END,
    token = NULL;

-- Remaining unused/active rows cannot recover plaintext. Revoke them.
UPDATE organization_invites
SET status = 'revoked',
    revoked_at = now(),
    token = NULL
WHERE status = 'active';

ALTER TABLE organization_invites
    ALTER COLUMN status SET DEFAULT 'active',
    ALTER COLUMN status SET NOT NULL;

ALTER TABLE organization_invites
    DROP CONSTRAINT IF EXISTS organization_invites_status_chk;
ALTER TABLE organization_invites
    ADD CONSTRAINT organization_invites_status_chk
        CHECK (status IN ('active', 'redeemed', 'revoked', 'expired'));

ALTER TABLE organization_invites
    DROP CONSTRAINT IF EXISTS organization_invites_active_token_chk;
ALTER TABLE organization_invites
    ADD CONSTRAINT organization_invites_active_token_chk
        CHECK ((status = 'active') = (token IS NOT NULL));

ALTER TABLE organization_invites
    DROP CONSTRAINT IF EXISTS organization_invites_max_uses_chk;
ALTER TABLE organization_invites
    ADD CONSTRAINT organization_invites_max_uses_chk
        CHECK (max_uses IS NULL OR max_uses >= 1);

ALTER TABLE organization_invites
    DROP CONSTRAINT IF EXISTS organization_invites_use_count_chk;
ALTER TABLE organization_invites
    ADD CONSTRAINT organization_invites_use_count_chk
        CHECK (use_count >= 0);
