CREATE TABLE IF NOT EXISTS services (
    name             TEXT PRIMARY KEY,
    current_revision BIGINT NOT NULL,
    deleted          BOOLEAN NOT NULL DEFAULT false,
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS revisions (
    service_name TEXT NOT NULL,
    revision     BIGINT NOT NULL,
    config       JSONB,
    request_id   TEXT NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (service_name, revision)
);

CREATE INDEX IF NOT EXISTS revisions_service_created_idx
    ON revisions (service_name, revision DESC);
