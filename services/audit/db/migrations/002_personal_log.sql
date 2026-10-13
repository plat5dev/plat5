-- Each realm owns its log (docs/audit.md#which-log). An event on a user route
-- is in the person's log (user_id), with no member until the service names
-- the org it changed; then it is in that org's log too. created_id is the id
-- a create produced.
ALTER TABLE audit_events
    ALTER COLUMN organization_id DROP NOT NULL,
    ALTER COLUMN actor_member_id DROP NOT NULL,
    ALTER COLUMN actor_key_prefix DROP NOT NULL,
    ADD COLUMN user_id TEXT,
    ADD COLUMN created_id TEXT;

-- An org event has its org and member from the credential. A person's event
-- has the person, and gains the org and member together or not at all.
ALTER TABLE audit_events ADD CONSTRAINT audit_events_owner CHECK (
    (user_id IS NULL AND organization_id IS NOT NULL AND actor_member_id IS NOT NULL AND actor_key_prefix IS NOT NULL)
    OR (user_id IS NOT NULL AND (organization_id IS NULL) = (actor_member_id IS NULL))
);

CREATE INDEX audit_events_user_id ON audit_events (user_id, id DESC) WHERE user_id IS NOT NULL;
CREATE INDEX audit_events_created_id ON audit_events (created_id) WHERE created_id IS NOT NULL;

-- Append-only, except one pending -> final transition that sets outcome,
-- status, details, and created_id. On a person's event it may also add the
-- org and member the service named. Nothing else ever changes.
CREATE OR REPLACE FUNCTION audit_events_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'audit events are not deleted';
    END IF;
    IF OLD.outcome <> 'pending' THEN
        RAISE EXCEPTION 'audit event % already has an outcome', OLD.request_id;
    END IF;
    IF NEW.outcome = 'pending'
        OR (NEW.id, NEW.occurred_at, NEW.request_id, NEW.user_id,
            NEW.actor_auth_type, NEW.actor_key_prefix,
            NEW.service, NEW.method, NEW.route, NEW.params, NEW.ip, NEW.user_agent)
           IS DISTINCT FROM
           (OLD.id, OLD.occurred_at, OLD.request_id, OLD.user_id,
            OLD.actor_auth_type, OLD.actor_key_prefix,
            OLD.service, OLD.method, OLD.route, OLD.params, OLD.ip, OLD.user_agent)
        OR ((NEW.organization_id, NEW.actor_member_id) IS DISTINCT FROM (OLD.organization_id, OLD.actor_member_id)
            AND (OLD.user_id IS NULL OR OLD.organization_id IS NOT NULL))
    THEN
        RAISE EXCEPTION 'audit event % may only gain an outcome', OLD.request_id;
    END IF;
    RETURN NEW;
END;
$$;
