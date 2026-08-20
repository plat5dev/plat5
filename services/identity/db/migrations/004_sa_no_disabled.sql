-- Repair: disabled SA whose member is still active → suspended (admission is members.status).
UPDATE members m
SET status = 'suspended', updated_at = now()
FROM service_accounts sa
WHERE m.service_account_id = sa.id
  AND m.organization_id = sa.organization_id
  AND sa.disabled_at IS NOT NULL
  AND m.status = 'active';

ALTER TABLE service_accounts DROP COLUMN disabled_at;
