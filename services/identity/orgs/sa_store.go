package orgs

import (
	"context"
	"time"

	"go.opentelemetry.io/otel/attribute"

	"github.com/plat5dev/plat5/identity/internal/dbx"
)

// saSelect reads a service account identity joined to its non-removed member.
// Alias sa is the identity.
const saSelect = `
	SELECT
		sa.id, sa.organization_id, m.id, sa.name,
		m.role, m.status, sa.created_at, sa.updated_at
	FROM identities sa
	INNER JOIN memberships m
		ON m.identity_id = sa.id
		AND m.organization_id = sa.organization_id
		AND m.status <> 'removed'
`

func (s *Store) CreateServiceAccount(ctx context.Context, sa *ServiceAccount) (*Member, error) {
	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "create_service_account", dbx.DefaultTimeout,
		attribute.String("organization.id", sa.OrganizationID),
		attribute.String("service_account.id", sa.ID),
	)
	defer cancel()
	defer op.End()

	now := time.Now().UTC()
	sa.CreatedAt = now
	sa.UpdatedAt = now
	sa.Status = StatusActive

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, op.Fail(err)
	}
	defer tx.Rollback(ctx)

	var exists string
	err = tx.QueryRow(ctx, `SELECT id FROM organizations WHERE id = $1 FOR SHARE`, sa.OrganizationID).Scan(&exists)
	if err != nil {
		if dbx.IsNoRows(err) {
			return nil, op.Expected("org not found", ErrNotFound)
		}
		return nil, op.Fail(err)
	}

	_, err = tx.Exec(ctx, `
		INSERT INTO identities
			(id, kind, organization_id, name, created_at, updated_at)
		VALUES ($1, 'service_account', $2, $3, $4, $5)
	`, sa.ID, sa.OrganizationID, sa.Name, sa.CreatedAt, sa.UpdatedAt)
	if err != nil {
		return nil, op.Fail(err)
	}

	m := &Member{
		ID:               NewULID(),
		OrganizationID:   sa.OrganizationID,
		ServiceAccountID: &sa.ID,
		Role:             sa.Role,
		Status:           StatusActive,
		CreatedAt:        now,
		UpdatedAt:        now,
	}
	if err := insertMember(ctx, tx, m); err != nil {
		return nil, op.Fail(err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, op.Fail(err)
	}

	sa.MemberID = m.ID
	op.Attr(attribute.String("member.id", m.ID))
	op.OK("ok")
	return m, nil
}

func (s *Store) GetServiceAccount(ctx context.Context, organizationID, serviceAccountID string) (*ServiceAccount, error) {
	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "get_service_account", dbx.DefaultTimeout,
		attribute.String("service_account.id", serviceAccountID),
	)
	defer cancel()
	defer op.End()

	sa, err := scanServiceAccount(s.pool.QueryRow(ctx, saSelect+`
		WHERE sa.kind = 'service_account' AND sa.id = $1 AND sa.organization_id = $2
	`, serviceAccountID, organizationID))
	if err != nil {
		if dbx.IsNoRows(err) {
			return nil, op.Expected("not found", ErrNotFound)
		}
		return nil, op.Fail(err)
	}
	op.OK("ok")
	return sa, nil
}

func (s *Store) ListServiceAccounts(ctx context.Context, organizationID string, limit int, startingAfter string) ([]*ServiceAccount, bool, error) {
	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "list_service_accounts", dbx.DefaultTimeout,
		attribute.String("organization.id", organizationID),
	)
	defer cancel()
	defer op.End()

	var after any
	if startingAfter != "" {
		after = startingAfter
	}
	rows, err := s.pool.Query(ctx, saSelect+`
		WHERE sa.kind = 'service_account' AND sa.organization_id = $1
		AND ($2::text IS NULL OR sa.id > $2)
		ORDER BY sa.id ASC
		LIMIT $3
	`, organizationID, after, limit+1)
	if err != nil {
		return nil, false, op.Fail(err)
	}
	defer rows.Close()

	var out []*ServiceAccount
	for rows.Next() {
		sa, err := scanServiceAccount(rows)
		if err != nil {
			return nil, false, op.Fail(err)
		}
		out = append(out, sa)
	}
	if err := rows.Err(); err != nil {
		return nil, false, op.Fail(err)
	}

	hasMore := len(out) > limit
	if hasMore {
		out = out[:limit]
	}
	op.Attr(attribute.Int("service_accounts.count", len(out)))
	op.OK("ok")
	return out, hasMore, nil
}

// UpdateServiceAccount renames a service account. It also returns the name it
// had, read under the same lock.
func (s *Store) UpdateServiceAccount(ctx context.Context, organizationID, serviceAccountID string, name string) (*ServiceAccount, string, error) {
	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "update_service_account", dbx.DefaultTimeout,
		attribute.String("service_account.id", serviceAccountID),
	)
	defer cancel()
	defer op.End()

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, "", op.Fail(err)
	}
	defer tx.Rollback(ctx)

	sa, err := scanServiceAccount(tx.QueryRow(ctx, saSelect+`
		WHERE sa.kind = 'service_account' AND sa.id = $1 AND sa.organization_id = $2
		FOR UPDATE OF sa, m
	`, serviceAccountID, organizationID))
	if err != nil {
		if dbx.IsNoRows(err) {
			return nil, "", op.Expected("not found", ErrNotFound)
		}
		return nil, "", op.Fail(err)
	}

	priorName := sa.Name
	now := time.Now().UTC()
	sa.Name = name
	sa.UpdatedAt = now

	_, err = tx.Exec(ctx, `
		UPDATE identities
		SET name = $3, updated_at = $4
		WHERE id = $1 AND organization_id = $2 AND kind = 'service_account'
	`, serviceAccountID, organizationID, sa.Name, sa.UpdatedAt)
	if err != nil {
		return nil, "", op.Fail(err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, "", op.Fail(err)
	}
	op.OK("ok")
	return sa, priorName, nil
}

// DeleteServiceAccount removes the SA member (same as DELETE member).
// change sees the SA's member and every member of the org, locked.
func (s *Store) DeleteServiceAccount(ctx context.Context, organizationID, serviceAccountID string, change MemberChange) error {
	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "delete_service_account", dbx.DefaultTimeout,
		attribute.String("service_account.id", serviceAccountID),
	)
	defer cancel()
	defer op.End()

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return op.Fail(err)
	}
	defer tx.Rollback(ctx)

	var exists string
	err = tx.QueryRow(ctx, `
		SELECT id FROM identities
		WHERE id = $1 AND organization_id = $2 AND kind = 'service_account'
	`, serviceAccountID, organizationID).Scan(&exists)
	if err != nil {
		if dbx.IsNoRows(err) {
			return op.Expected("not found", ErrNotFound)
		}
		return op.Fail(err)
	}

	members, err := lockOrgMembers(ctx, tx, organizationID)
	if err != nil {
		return op.Fail(err)
	}
	var target *Member
	for _, m := range members {
		if m.ServiceAccountID != nil && *m.ServiceAccountID == serviceAccountID && m.Status != StatusRemoved {
			target = m
			break
		}
	}
	if target == nil {
		return op.Expected("not found", ErrNotFound)
	}
	if err := change(target, members); err != nil {
		return op.Expected("rejected", err)
	}

	target.Status = StatusRemoved
	target.UpdatedAt = time.Now().UTC()
	_, err = tx.Exec(ctx, `
		UPDATE memberships
		SET status = 'removed', updated_at = $3
		WHERE id = $1 AND organization_id = $2
	`, target.ID, organizationID, target.UpdatedAt)
	if err != nil {
		return op.Fail(err)
	}
	if err := finishRemove(ctx, tx, target); err != nil {
		return op.Fail(err)
	}

	if err := tx.Commit(ctx); err != nil {
		return op.Fail(err)
	}
	op.OK("ok")
	return nil
}
