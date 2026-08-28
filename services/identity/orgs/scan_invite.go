package orgs

import (
	"github.com/plat5dev/plat5/identity/internal/dbx"
)

func scanInvite(row dbx.Scannable) (*Invite, error) {
	var inv Invite
	var role string
	var status string
	err := row.Scan(
		&inv.ID,
		&inv.OrganizationID,
		&role,
		&inv.Email,
		&inv.TokenHash,
		&inv.TokenPrefix,
		&inv.CreatedBy,
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
	inv.Role = Role(role)
	inv.Status = InviteStatus(status)
	return &inv, nil
}

const inviteSelectCols = `id, organization_id, role, email, token_hash, token_prefix,
			created_by, expires_at, created_at, token, status, max_uses, use_count`
