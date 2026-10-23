-- One row per audited request (docs/concepts/audit.md). Monthly partitions on
-- occurred_at are created by the service (events.EnsurePartitions), not by
-- migrations.
--
-- An event on a kind route is in its root's log: root_id is the chain's root,
-- node_ids the chain, root first. An event on a sign-in route is in the
-- person's own log, keyed by their directory member (actor_member_id), with no
-- root and no nodes; a success can name a tree it changed (named_root_id) and
-- the member in it, which puts it in that root's log too.
CREATE TABLE audit_events (
    id                TEXT        NOT NULL,  -- ULID on occurred_at
    occurred_at       TIMESTAMPTZ NOT NULL,
    request_id        TEXT        NOT NULL,
    root_id           TEXT,
    nodes             JSONB       NOT NULL,  -- [{kind, id}], root first
    node_ids          TEXT[]      NOT NULL,  -- nodes' ids, for reads by node
    named_root_id     TEXT,
    named_member_id   TEXT,
    actor_member_id   TEXT        NOT NULL,
    actor_principal   TEXT        NOT NULL CHECK (actor_principal IN ('person', 'service_account')),
    actor_auth_type   TEXT        NOT NULL CHECK (actor_auth_type IN ('jwt', 'apikey')),
    actor_key_prefix  TEXT,
    service           TEXT        NOT NULL,
    method            TEXT        NOT NULL,
    route             TEXT        NOT NULL,
    params            JSONB       NOT NULL DEFAULT '{}'::jsonb,
    ip                TEXT        NOT NULL,
    user_agent        TEXT,
    outcome           TEXT        NOT NULL DEFAULT 'pending'
        CHECK (outcome IN ('pending', 'rejected', 'responded', 'no_response')),
    status            INTEGER,
    details           JSONB,
    created_id        TEXT,
    PRIMARY KEY (id, occurred_at),
    -- The partition key must be in a unique constraint. A retried intent sends
    -- the same body, so this is the request_id dedupe; the insert also checks
    -- request_id alone.
    UNIQUE (request_id, occurred_at),
    CONSTRAINT audit_events_root CHECK (
        (root_id IS NULL AND cardinality(node_ids) = 0)
        OR (root_id IS NOT NULL AND COALESCE(node_ids[1] = root_id, false))
    ),
    -- Only a sign-in route's event names a tree, and it names a member with it.
    CONSTRAINT audit_events_named CHECK (
        (named_root_id IS NULL AND named_member_id IS NULL)
        OR (root_id IS NULL AND named_root_id IS NOT NULL AND named_member_id IS NOT NULL)
    ),
    CONSTRAINT audit_events_key_prefix CHECK ((actor_auth_type = 'jwt') = (actor_key_prefix IS NULL))
) PARTITION BY RANGE (occurred_at);

CREATE INDEX audit_events_node_ids ON audit_events USING GIN (node_ids);
CREATE INDEX audit_events_named_root ON audit_events (named_root_id, id DESC) WHERE named_root_id IS NOT NULL;
CREATE INDEX audit_events_person ON audit_events (actor_member_id, id DESC) WHERE root_id IS NULL;
CREATE INDEX audit_events_params ON audit_events USING GIN (params jsonb_path_ops);
CREATE INDEX audit_events_created_id ON audit_events (created_id) WHERE created_id IS NOT NULL;

-- Append-only, except one pending -> final transition that sets outcome,
-- status, details and created_id, and on a sign-in route's event may name a
-- tree and member. Nothing else ever changes. Dropping a partition (retention,
-- later) is not a row delete and does not fire this.
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
        OR (NEW.id, NEW.occurred_at, NEW.request_id, NEW.root_id, NEW.nodes, NEW.node_ids,
            NEW.actor_member_id, NEW.actor_principal, NEW.actor_auth_type, NEW.actor_key_prefix,
            NEW.service, NEW.method, NEW.route, NEW.params, NEW.ip, NEW.user_agent)
           IS DISTINCT FROM
           (OLD.id, OLD.occurred_at, OLD.request_id, OLD.root_id, OLD.nodes, OLD.node_ids,
            OLD.actor_member_id, OLD.actor_principal, OLD.actor_auth_type, OLD.actor_key_prefix,
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
