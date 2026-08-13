package orgs

import (
	"context"
	"fmt"
	"time"

	"go.opentelemetry.io/otel/attribute"

	"github.com/plat5dev/plat5/identity/internal/dbx"
)

const saSelect = `
	SELECT
		sa.id, sa.home_organization_id, m.id, sa.name, sa.created_by_user_id,
		sa.disabled_at, sa.created_at, sa.updated_at
	FROM service_accounts sa
	INNER JOIN members m
		ON m.service_account_id = sa.id
		AND m.organization_id = sa.home_organization_id
		AND m.status <> 'removed'
`

func (s *Store) CreateServiceAccount(ctx context.Context, sa *ServiceAccount, role Role, addedBy *string) (*Member, error) {
	if role == RoleOwner {
		return nil, fmt.Errorf("create_service_account: service accounts cannot be owners")
	}
	if role == "" {
		role = RoleMember
	}

	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "create_service_account", dbx.DefaultTimeout,
		attribute.String("organization.id", sa.OrganizationID),
		attribute.String("service_account.id", sa.ID),
	)
	defer cancel()
	defer op.End()

	now := time.Now().UTC()
	sa.CreatedAt = now
	sa.UpdatedAt = now

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
		INSERT INTO service_accounts
			(id, home_organization_id, name, created_by_user_id, disabled_at, created_at, updated_at)
		VALUES ($1, $2, $3, $4, NULL, $5, $6)
	`, sa.ID, sa.OrganizationID, sa.Name, sa.CreatedByUserID, sa.CreatedAt, sa.UpdatedAt)
	if err != nil {
		return nil, op.Fail(err)
	}

	m := &Member{
		ID:               NewULID(),
		OrganizationID:   sa.OrganizationID,
		ServiceAccountID: &sa.ID,
		Role:             role,
		Status:           StatusActive,
		AddedBy:          addedBy,
		CreatedAt:        now,
		UpdatedAt:        now,
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO members
			(id, organization_id, user_id, service_account_id, role, status, added_by, created_at, updated_at)
		VALUES ($1, $2, NULL, $3, $4, $5, $6, $7, $8)
	`, m.ID, m.OrganizationID, sa.ID, m.Role, m.Status, m.AddedBy, m.CreatedAt, m.UpdatedAt)
	if err != nil {
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
		WHERE sa.id = $1 AND sa.home_organization_id = $2
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

func (s *Store) ListServiceAccounts(ctx context.Context, organizationID string, limit, offset int) ([]*ServiceAccount, bool, error) {
	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "list_service_accounts", dbx.DefaultTimeout,
		attribute.String("organization.id", organizationID),
	)
	defer cancel()
	defer op.End()

	rows, err := s.pool.Query(ctx, saSelect+`
		WHERE sa.home_organization_id = $1
		ORDER BY sa.created_at DESC
		LIMIT $2 OFFSET $3
	`, organizationID, limit+1, offset)
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

// UpdateServiceAccount updates name and/or disabled_at.
// Setting disabled=true also sets member status suspended; disabled=false → active.
func (s *Store) UpdateServiceAccount(ctx context.Context, organizationID, serviceAccountID string, name *string, disabled *bool) (*ServiceAccount, error) {
	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "update_service_account", dbx.DefaultTimeout,
		attribute.String("service_account.id", serviceAccountID),
	)
	defer cancel()
	defer op.End()

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, op.Fail(err)
	}
	defer tx.Rollback(ctx)

	sa, err := scanServiceAccount(tx.QueryRow(ctx, saSelect+`
		WHERE sa.id = $1 AND sa.home_organization_id = $2
		FOR UPDATE OF sa, m
	`, serviceAccountID, organizationID))
	if err != nil {
		if dbx.IsNoRows(err) {
			return nil, op.Expected("not found", ErrNotFound)
		}
		return nil, op.Fail(err)
	}

	now := time.Now().UTC()
	if name != nil {
		sa.Name = *name
	}
	if disabled != nil {
		if *disabled {
			sa.DisabledAt = &now
		} else {
			sa.DisabledAt = nil
		}
	}
	sa.UpdatedAt = now

	_, err = tx.Exec(ctx, `
		UPDATE service_accounts
		SET name = $3, disabled_at = $4, updated_at = $5
		WHERE id = $1 AND home_organization_id = $2
	`, serviceAccountID, organizationID, sa.Name, sa.DisabledAt, sa.UpdatedAt)
	if err != nil {
		return nil, op.Fail(err)
	}

	if disabled != nil {
		memberStatus := StatusActive
		if *disabled {
			memberStatus = StatusSuspended
		}
		_, err = tx.Exec(ctx, `
			UPDATE members
			SET status = $3, updated_at = $4
			WHERE id = $1 AND organization_id = $2 AND status <> 'removed'
		`, sa.MemberID, organizationID, memberStatus, now)
		if err != nil {
			return nil, op.Fail(err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, op.Fail(err)
	}
	op.OK("ok")
	return sa, nil
}

// DeleteServiceAccount disables the SA and soft-removes its member.
func (s *Store) DeleteServiceAccount(ctx context.Context, organizationID, serviceAccountID string) error {
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

	sa, err := scanServiceAccount(tx.QueryRow(ctx, saSelect+`
		WHERE sa.id = $1 AND sa.home_organization_id = $2
		FOR UPDATE OF sa, m
	`, serviceAccountID, organizationID))
	if err != nil {
		if dbx.IsNoRows(err) {
			return op.Expected("not found", ErrNotFound)
		}
		return op.Fail(err)
	}

	now := time.Now().UTC()
	_, err = tx.Exec(ctx, `
		UPDATE service_accounts
		SET disabled_at = COALESCE(disabled_at, $3), updated_at = $3
		WHERE id = $1 AND home_organization_id = $2
	`, serviceAccountID, organizationID, now)
	if err != nil {
		return op.Fail(err)
	}
	_, err = tx.Exec(ctx, `
		UPDATE members
		SET status = 'removed', updated_at = $3
		WHERE id = $1 AND organization_id = $2
	`, sa.MemberID, organizationID, now)
	if err != nil {
		return op.Fail(err)
	}

	if err := tx.Commit(ctx); err != nil {
		return op.Fail(err)
	}
	op.OK("ok")
	return nil
}
