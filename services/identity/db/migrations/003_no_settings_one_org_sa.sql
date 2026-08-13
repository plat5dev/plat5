ALTER TABLE organizations DROP COLUMN settings;

ALTER TABLE service_accounts RENAME COLUMN home_organization_id TO organization_id;

ALTER INDEX service_accounts_home_org_idx RENAME TO service_accounts_organization_id_idx;

DROP INDEX IF EXISTS members_org_sa_uidx;

CREATE UNIQUE INDEX members_sa_uidx
    ON members (service_account_id)
    WHERE service_account_id IS NOT NULL;
