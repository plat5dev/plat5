-- One row per audited request (docs/audit.md). Monthly partitions on occurred_at
-- are created by the service (events.EnsurePartitions), not by migrations.
CREATE TABLE audit_events (
    id               TEXT        NOT NULL,  -- ULID on occurred_at
    occurred_at      TIMESTAMPTZ NOT NULL,
    request_id       TEXT        NOT NULL,
    organization_id  TEXT        NOT NULL,
    actor_member_id  TEXT        NOT NULL,
    actor_auth_type  TEXT        NOT NULL,
    actor_key_prefix TEXT        NOT NULL,
    service          TEXT        NOT NULL,
    method           TEXT        NOT NULL,
    route            TEXT        NOT NULL,
    params           JSONB       NOT NULL DEFAULT '{}'::jsonb,
    ip               TEXT        NOT NULL,
    user_agent       TEXT,
    outcome          TEXT        NOT NULL DEFAULT 'pending'
        CHECK (outcome IN ('pending', 'rejected', 'responded', 'no_response')),
    status           INTEGER,
    details          JSONB,
    PRIMARY KEY (id, occurred_at),
    -- The partition key must be in a unique constraint. A retried intent sends
    -- the same body, so this is the request_id dedupe; the insert also checks
    -- request_id alone.
    UNIQUE (request_id, occurred_at)
) PARTITION BY RANGE (occurred_at);

CREATE INDEX audit_events_org_id ON audit_events (organization_id, id DESC);
CREATE INDEX audit_events_org_actor_id ON audit_events (organization_id, actor_member_id, id DESC);
CREATE INDEX audit_events_params ON audit_events USING GIN (params jsonb_path_ops);

-- Append-only, except one pending -> final transition that sets outcome,
-- status, and details. Dropping a partition (retention, later) is not a row
-- delete and does not fire this.
CREATE FUNCTION audit_events_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'audit events are not deleted';
    END IF;
    IF OLD.outcome <> 'pending' THEN
        RAISE EXCEPTION 'audit event % already has an outcome', OLD.request_id;
    END IF;
    IF NEW.outcome = 'pending'
        OR (NEW.id, NEW.occurred_at, NEW.request_id, NEW.organization_id,
            NEW.actor_member_id, NEW.actor_auth_type, NEW.actor_key_prefix,
            NEW.service, NEW.method, NEW.route, NEW.params, NEW.ip, NEW.user_agent)
           IS DISTINCT FROM
           (OLD.id, OLD.occurred_at, OLD.request_id, OLD.organization_id,
            OLD.actor_member_id, OLD.actor_auth_type, OLD.actor_key_prefix,
            OLD.service, OLD.method, OLD.route, OLD.params, OLD.ip, OLD.user_agent)
    THEN
        RAISE EXCEPTION 'audit event % may only gain an outcome', OLD.request_id;
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER audit_events_guard
    BEFORE UPDATE OR DELETE ON audit_events
    FOR EACH ROW EXECUTE FUNCTION audit_events_guard();
