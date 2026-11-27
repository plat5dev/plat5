-- Adds the export sequence and finalized_seq column for deployment-wide pull
-- export (GET /internal/events), plus a SECURITY DEFINER ensure_partitions
-- function so the runtime writer role can create monthly partitions without DDL
-- privileges (docs/internals/audit-storage.md).

CREATE SEQUENCE export_seq;

ALTER TABLE audit_events ADD COLUMN finalized_seq BIGINT;

-- Backfill any existing finalized events from 001_init before tightening the
-- guard trigger.
ALTER TABLE audit_events DISABLE TRIGGER audit_events_guard;
UPDATE audit_events
SET finalized_seq = nextval('export_seq')
WHERE outcome <> 'pending' AND finalized_seq IS NULL;
ALTER TABLE audit_events ENABLE TRIGGER audit_events_guard;

CREATE INDEX audit_events_finalized_seq
    ON audit_events (finalized_seq)
    WHERE finalized_seq IS NOT NULL;

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
        OR NEW.finalized_seq IS NULL
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

-- Creates both monthly partitions (audit_events and audit_log_keys) for p_at.
-- Created dynamically against current_schema() with a locked-down search_path
-- so both the production 'audit' schema and isolated test schemas work safely.
DO $$
DECLARE
    s text := current_schema();
BEGIN
    EXECUTE format($fmt$
        CREATE OR REPLACE FUNCTION %I.ensure_partitions(p_at timestamptz) RETURNS void
        LANGUAGE plpgsql SECURITY DEFINER
        SET search_path = %I, pg_temp
        SET timezone = 'UTC'
        AS $fn$
        DECLARE
            month_start timestamptz := date_trunc('month', p_at);
            suffix text := '_y' || to_char(month_start, 'YYYY') || 'm' || to_char(month_start, 'MM');
            events_part text := 'audit_events' || suffix;
            keys_part text := 'audit_log_keys' || suffix;
        BEGIN
            PERFORM pg_advisory_xact_lock(hashtext('audit.audit_events.partitions'));
            EXECUTE format(
                'CREATE TABLE IF NOT EXISTS %I.%%I PARTITION OF %I.audit_events FOR VALUES FROM (%%L) TO (%%L)',
                events_part, month_start, month_start + interval '1 month');
            EXECUTE format(
                'CREATE TABLE IF NOT EXISTS %I.%%I PARTITION OF %I.audit_log_keys FOR VALUES FROM (%%L) TO (%%L)',
                keys_part, month_start, month_start + interval '1 month');
        END;
        $fn$;
    $fmt$, s, s, s, s, s, s);
    EXECUTE format('REVOKE ALL ON FUNCTION %I.ensure_partitions(timestamptz) FROM PUBLIC', s);
END;
$$;
