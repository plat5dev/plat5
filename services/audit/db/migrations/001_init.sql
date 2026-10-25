-- One row per audited request (docs/concepts/audit.md). Monthly partitions on
-- occurred_at are created by the service (events.EnsurePartitions), not by
-- migrations.
--
-- The event as the gateway sent it, with no checks between fields: audit
-- stores what it's sent. root_id is nodes[0].id, null on a user route.
CREATE TABLE audit_events (
    id                TEXT        NOT NULL,  -- lowercase ULID on occurred_at
    occurred_at       TIMESTAMPTZ NOT NULL,
    request_id        TEXT        NOT NULL,
    root_id           TEXT,
    nodes             JSONB       NOT NULL DEFAULT '[]'::jsonb,  -- [{kind, id}], root first
    actor_member_id   TEXT,
    actor_user_id     TEXT,
    actor_principal   TEXT,
    actor_auth_type   TEXT,
    actor_key_id      TEXT,
    service           TEXT        NOT NULL,
    method            TEXT,
    route             TEXT,
    params            JSONB       NOT NULL DEFAULT '{}'::jsonb,
    ip                TEXT,
    user_agent        TEXT,
    outcome           TEXT        NOT NULL DEFAULT 'pending',
    status            INTEGER,
    details           JSONB,
    created_id        TEXT,
    named_nodes       TEXT[]      NOT NULL DEFAULT '{}',
    named_member_id   TEXT,
    PRIMARY KEY (id, occurred_at),
    -- The partition key must be in a unique constraint. A retried intent sends
    -- the same body, so this is the request_id dedupe; the insert also checks
    -- request_id alone.
    UNIQUE (request_id, occurred_at)
) PARTITION BY RANGE (occurred_at);

CREATE INDEX audit_events_params ON audit_events USING GIN (params jsonb_path_ops);

-- One row per log an event is in. log is a node id (each node in nodes, each
-- in named_nodes, and created_id) or 'user:' and a user id. Every read is one
-- range on (log, event_id DESC). Partitioned with the events.
CREATE TABLE audit_log_keys (
    log          TEXT        NOT NULL,
    event_id     TEXT        NOT NULL,
    occurred_at  TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (log, event_id, occurred_at)
) PARTITION BY RANGE (occurred_at);

-- Append-only, except one pending -> final transition that sets outcome,
-- status, details, created_id and the named nodes and member. Nothing else
-- ever changes. Dropping a partition (retention, later) is not a row delete
-- and does not fire this.
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
        OR (NEW.id, NEW.occurred_at, NEW.request_id, NEW.root_id, NEW.nodes,
            NEW.actor_member_id, NEW.actor_user_id, NEW.actor_principal, NEW.actor_auth_type, NEW.actor_key_id,
            NEW.service, NEW.method, NEW.route, NEW.params, NEW.ip, NEW.user_agent)
           IS DISTINCT FROM
           (OLD.id, OLD.occurred_at, OLD.request_id, OLD.root_id, OLD.nodes,
            OLD.actor_member_id, OLD.actor_user_id, OLD.actor_principal, OLD.actor_auth_type, OLD.actor_key_id,
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

-- Keys are only added: by the intent, and by the outcome for the nodes it
-- names.
CREATE FUNCTION audit_log_keys_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'audit log keys are only added';
END;
$$;

CREATE TRIGGER audit_log_keys_guard
    BEFORE UPDATE OR DELETE ON audit_log_keys
    FOR EACH ROW EXECUTE FUNCTION audit_log_keys_guard();
