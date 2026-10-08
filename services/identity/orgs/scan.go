package orgs

import (
	"github.com/plat5dev/plat5/identity/internal/dbx"
)

func scanServiceAccount(row dbx.Scannable) (*ServiceAccount, error) {
	var sa ServiceAccount
	var status string
	err := row.Scan(
		&sa.ID,
		&sa.OrganizationID,
		&sa.MemberID,
		&sa.Name,
		&sa.Role,
		&status,
		&sa.CreatedAt,
		&sa.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	sa.Status = Status(status)
	return &sa, nil
}

func scanMembership(row dbx.Scannable) (*Membership, error) {
	var m Membership
	var status string
	err := row.Scan(
		&m.ID,
		&m.Role,
		&status,
		&m.OrganizationID,
		&m.OrganizationName,
		&m.OrganizationSlug,
	)
	if err != nil {
		return nil, err
	}
	m.Status = Status(status)
	return &m, nil
}

func scanOrg(row dbx.Scannable) (*Organization, error) {
	var o Organization
	err := row.Scan(&o.ID, &o.Name, &o.Slug, &o.CreatedAt, &o.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &o, nil
}

const memberCols = `id, organization_id, user_id, service_account_id, role, status, created_at, updated_at`

func scanMember(row dbx.Scannable) (*Member, error) {
	var m Member
	var status string
	err := row.Scan(
		&m.ID,
		&m.OrganizationID,
		&m.UserID,
		&m.ServiceAccountID,
		&m.Role,
		&status,
		&m.CreatedAt,
		&m.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	m.Status = Status(status)
	return &m, nil
}
