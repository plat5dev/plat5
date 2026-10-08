package orgs

import (
	"github.com/plat5dev/plat5/identity/internal/dbx"
)

func scanInvite(row dbx.Scannable) (*Invite, error) {
	var inv Invite
	var status string
	err := row.Scan(
		&inv.ID,
		&inv.OrganizationID,
		&inv.Email,
		&inv.Role,
		&inv.TokenHash,
		&inv.TokenPrefix,
		&inv.ExpiresAt,
		&inv.CreatedAt,
		&inv.Token,
		&status,
		&inv.MaxUses,
		&inv.UseCount,
	)
	if err != nil {
		return nil, err
	}
	inv.Status = InviteStatus(status)
	return &inv, nil
}

const inviteSelectCols = `id, organization_id, email, role, token_hash, token_prefix,
			expires_at, created_at, token, status, max_uses, use_count`
