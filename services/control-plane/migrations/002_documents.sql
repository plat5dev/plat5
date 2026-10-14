-- One revision for the whole control plane. Every write bumps it in the same
-- transaction as the change; the row lock serializes writers, so revision
-- order is commit order. Readers long-poll on it.
CREATE TABLE IF NOT EXISTS head (
    id       BOOLEAN PRIMARY KEY DEFAULT true CHECK (id),
    revision BIGINT NOT NULL
);

INSERT INTO head (id, revision) VALUES (true, 0) ON CONFLICT (id) DO NOTHING;

-- Deployment-wide documents other than routes: `roles`, `cors`. One current
-- revision per kind, like services. A document has no delete.
CREATE TABLE IF NOT EXISTS documents (
    kind             TEXT PRIMARY KEY,
    current_revision BIGINT NOT NULL,
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS document_revisions (
    kind       TEXT NOT NULL,
    revision   BIGINT NOT NULL,
    body       JSONB NOT NULL,
    request_id TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (kind, revision)
);
