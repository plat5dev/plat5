-- The config, one revision per apply. The head is the current revision; an
-- apply locks its row, so writes are serialized and head order is commit order.
-- Readers long-poll on it.
CREATE TABLE IF NOT EXISTS config_head (
    id       BOOLEAN PRIMARY KEY DEFAULT true CHECK (id),
    revision BIGINT NOT NULL
);

INSERT INTO config_head (id, revision) VALUES (true, 0) ON CONFLICT (id) DO NOTHING;

-- `config` is the flat config as sent, so a pull gives back what was applied.
-- `summary` is the plan's summary.
CREATE TABLE IF NOT EXISTS config_revisions (
    revision   BIGINT PRIMARY KEY,
    config     JSONB NOT NULL,
    summary    JSONB NOT NULL,
    request_id TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- The latest report of each gateway replica, sent on its long poll: the
-- revision it serves, and the last one it refused with the reason. `GET /status`
-- lists the replicas seen in the last minute.
CREATE TABLE IF NOT EXISTS gateway_replicas (
    replica TEXT PRIMARY KEY,
    loaded  BIGINT NOT NULL,
    refused BIGINT,
    reason  TEXT,
    seen_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
