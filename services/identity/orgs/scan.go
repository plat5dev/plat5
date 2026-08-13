package orgs

import (
	"github.com/plat5dev/plat5/identity/internal/dbx"
)

func scanServiceAccount(row dbx.Scannable) (*ServiceAccount, error) {
	var sa ServiceAccount
	err := row.Scan(
		&sa.ID,
		&sa.OrganizationID,
		&sa.MemberID,
		&sa.Name,
		&sa.CreatedByUserID,
		&sa.DisabledAt,
		&sa.CreatedAt,
		&sa.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	return &sa, nil
}

func scanOrg(row dbx.Scannable) (*Organization, error) {
	var o Organization
	err := row.Scan(&o.ID, &o.Name, &o.Slug, &o.Settings, &o.CreatedAt, &o.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &o, nil
}

func scanMember(row dbx.Scannable) (*Member, error) {
	var m Member
	var role, status string
	err := row.Scan(
		&m.ID,
		&m.OrganizationID,
		&m.UserID,
		&m.ServiceAccountID,
		&role,
		&status,
		&m.AddedBy,
		&m.CreatedAt,
		&m.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	m.Role = Role(role)
	m.Status = Status(status)
	return &m, nil
}
