UPDATE members SET status = 'removed' WHERE status = 'pending';

ALTER TABLE members RENAME COLUMN invited_by TO added_by;

ALTER TABLE members DROP CONSTRAINT IF EXISTS members_status_check;
ALTER TABLE members ADD CONSTRAINT members_status_check
    CHECK (status IN ('active', 'suspended', 'removed'));
