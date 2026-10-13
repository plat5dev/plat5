-- removed is final (docs/identity.md#status). Adding the person again inserts
-- a new member, so only non-removed rows are unique per (org, user).
DROP INDEX members_org_user_uidx;

CREATE UNIQUE INDEX members_org_user_uidx
    ON members (organization_id, user_id)
    WHERE user_id IS NOT NULL AND status <> 'removed';

-- A remove revokes the member's keys and deletes its sessions. Apply that to
-- members removed before this rule.
UPDATE member_api_keys k
SET revoked_at = now()
FROM members m
WHERE k.member_id = m.id AND m.status = 'removed' AND k.revoked_at IS NULL;

DELETE FROM member_sessions s
USING members m
WHERE s.member_id = m.id AND m.status = 'removed';

-- Facts other services may act on (docs/identity.md#lifecycle-events).
-- Append-only. No FK: an event outlives the org it names. Writers take
-- lifecycle.lockKey before inserting, so id order is commit order.
CREATE TABLE lifecycle_events (
    id               BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    type             TEXT NOT NULL CHECK (type IN ('organization.deleted', 'member.removed')),
    occurred_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    organization_id  TEXT NOT NULL,
    member_id        TEXT,
    principal        TEXT CHECK (principal IN ('user', 'service_account')),
    CHECK ((type = 'member.removed') = (member_id IS NOT NULL)),
    CHECK ((member_id IS NULL) = (principal IS NULL))
);
