-- Supports root-scoped node listings (/users/{id}/nodes) and custom-role deletion binding cleanup.
CREATE INDEX IF NOT EXISTS nodes_root_id_idx ON nodes (root_id, id);
