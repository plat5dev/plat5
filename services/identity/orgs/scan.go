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
	)
	if err != nil {
		return nil, err
	}
	m.Status = Status(status)
	return &m, nil
}

func scanOrg(row dbx.Scannable) (*Organization, error) {
	var o Organization
	err := row.Scan(&o.ID, &o.Name, &o.CreatedAt, &o.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &o, nil
}

// memberSelect reads org memberships as members, with the display name a
// roster shows. Alias m is the membership.
const memberSelect = `
	SELECT
		m.id, m.organization_id, m.principal, m.identity_id, m.display_name,
		COALESCE(m.display_name, CASE WHEN m.principal = 'service_account' THEN i.name ELSE d.display_name END),
		m.role, m.status, m.created_at, m.updated_at
	FROM memberships m
	LEFT JOIN identities i ON i.id = m.identity_id
	LEFT JOIN memberships d ON d.id = m.identity_id AND d.organization_id IS NULL
`

func scanMember(row dbx.Scannable) (*Member, error) {
	var m Member
	var identityID *string
	var status string
	err := row.Scan(
		&m.ID,
		&m.OrganizationID,
		&m.Kind,
		&identityID,
		&m.DisplayOverride,
		&m.DisplayName,
		&m.Role,
		&status,
		&m.CreatedAt,
		&m.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	if m.Kind == PrincipalServiceAccount {
		m.ServiceAccountID = identityID
	} else {
		m.UserID = identityID
	}
	m.Status = Status(status)
	return &m, nil
}
