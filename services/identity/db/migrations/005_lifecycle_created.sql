-- Births of the two ids services store (docs/identity.md#lifecycle-events).
-- Ids and principal only. The type check and the member-id check were
-- unnamed in 002; drop them by definition, then add named ones.

DO $$
DECLARE
    r record;
BEGIN
    FOR r IN
        SELECT con.conname
        FROM pg_constraint con
        WHERE con.conrelid = 'lifecycle_events'::regclass
          AND con.contype = 'c'
          AND pg_get_constraintdef(con.oid) LIKE '%member.removed%'
    LOOP
        EXECUTE format('ALTER TABLE lifecycle_events DROP CONSTRAINT %I', r.conname);
    END LOOP;
END $$;

ALTER TABLE lifecycle_events
    ADD CONSTRAINT lifecycle_events_type_check
    CHECK (type IN (
        'organization.created',
        'organization.deleted',
        'member.created',
        'member.removed'
    ));

ALTER TABLE lifecycle_events
    ADD CONSTRAINT lifecycle_events_member_id_check
    CHECK ((type IN ('member.created', 'member.removed')) = (member_id IS NOT NULL));
