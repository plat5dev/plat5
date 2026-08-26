package orgs

import (
	"github.com/plat5dev/plat5/identity/internal/dbx"
)

func scanInvite(row dbx.Scannable) (*Invite, error) {
	var inv Invite
	var role string
	err := row.Scan(
		&inv.ID,
		&inv.OrganizationID,
		&role,
		&inv.Email,
		&inv.TokenHash,
		&inv.TokenPrefix,
		&inv.CreatedBy,
		&inv.ExpiresAt,
		&inv.RedeemedAt,
		&inv.RedeemedBy,
		&inv.RevokedAt,
		&inv.CreatedAt,
	)
	if err != nil {
		return nil, err
	}
	inv.Role = Role(role)
	return &inv, nil
}
