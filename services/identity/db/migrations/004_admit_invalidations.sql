-- Commit-ordered admit cache invalidations for gateway replicas
-- (docs/reference/api/internal.md#admit-invalidations).
CREATE TABLE admit_invalidations (
    id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    scope       TEXT NOT NULL CHECK (scope IN ('key', 'user', 'member', 'root')),
    target      TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX admit_invalidations_created_idx ON admit_invalidations (created_at);

-- Tracks the highest invalidation id removed by pruning so readers can tell
-- whether their cursor fell behind the retained window.
CREATE TABLE admit_invalidation_watermark (
    id             INT PRIMARY KEY CHECK (id = 1),
    pruned_through BIGINT NOT NULL DEFAULT 0
);
INSERT INTO admit_invalidation_watermark (id, pruned_through) VALUES (1, 0);
