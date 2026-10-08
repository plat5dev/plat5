-- Routes say `required_labels`, not `required_scopes` (docs/routes.md). The
-- route config refuses unknown keys, so rename the key in every stored
-- revision. jsonb text renders a key as `"key": `; a string value is never
-- followed by a colon, so only keys match.
UPDATE revisions
SET config = replace(config::text, '"required_scopes": ', '"required_labels": ')::jsonb
WHERE config IS NOT NULL AND config::text LIKE '%"required_scopes": %';
