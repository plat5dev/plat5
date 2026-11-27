-- Verified caller attribution (set solely from the validated credential, never
-- from client headers) and optional caller-supplied change metadata.
ALTER TABLE config_revisions
    ADD COLUMN IF NOT EXISTS actor_type        TEXT,
    ADD COLUMN IF NOT EXISTS actor_issuer      TEXT,
    ADD COLUMN IF NOT EXISTS actor_issuer_name TEXT,
    ADD COLUMN IF NOT EXISTS actor_subject     TEXT,
    ADD COLUMN IF NOT EXISTS change_reason     TEXT,
    ADD COLUMN IF NOT EXISTS change_ref        TEXT;
