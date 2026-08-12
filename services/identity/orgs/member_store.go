package orgs

import (
	"context"
	"fmt"
	"time"

	"go.opentelemetry.io/otel/attribute"

	"github.com/plat5dev/plat5/identity/internal/dbx"
)

func (s *Store) GetMember(ctx context.Context, organizationID, memberID string) (*Member, error) {
	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "get_member", dbx.DefaultTimeout,
		attribute.String("organization.id", organizationID),
		attribute.String("member.id", memberID),
	)
	defer cancel()
	defer op.End()

	m, err := scanMember(s.pool.QueryRow(ctx, `
		SELECT id, organization_id, user_id, service_account_id, role, status, invited_by, created_at, updated_at
		FROM members
		WHERE organization_id = $1 AND id = $2
	`, organizationID, memberID))
	if err != nil {
		if dbx.IsNoRows(err) {
			return nil, op.Expected("not found", ErrNotFound)
		}
		return nil, op.Fail(err)
	}
	op.OK("ok")
	return m, nil
}

func (s *Store) GetActiveMemberForUser(ctx context.Context, organizationID, userID string) (*Member, error) {
	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "get_active_member_for_user", dbx.DefaultTimeout,
		attribute.String("organization.id", organizationID),
		attribute.String("user.id", userID),
	)
	defer cancel()
	defer op.End()

	m, err := scanMember(s.pool.QueryRow(ctx, `
		SELECT id, organization_id, user_id, service_account_id, role, status, invited_by, created_at, updated_at
		FROM members
		WHERE organization_id = $1 AND user_id = $2 AND status = 'active'
	`, organizationID, userID))
	if err != nil {
		if dbx.IsNoRows(err) {
			return nil, op.Expected("not found", ErrNotFound)
		}
		return nil, op.Fail(err)
	}
	op.OK("ok")
	return m, nil
}

// ResolveMember returns any non-removed user member for (user, org). Used by internal resolve.
func (s *Store) ResolveMember(ctx context.Context, userID, organizationID string) (*Member, error) {
	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "resolve_member", dbx.DefaultTimeout,
		attribute.String("organization.id", organizationID),
		attribute.String("user.id", userID),
	)
	defer cancel()
	defer op.End()

	m, err := scanMember(s.pool.QueryRow(ctx, `
		SELECT id, organization_id, user_id, service_account_id, role, status, invited_by, created_at, updated_at
		FROM members
		WHERE organization_id = $1 AND user_id = $2 AND status <> 'removed'
	`, organizationID, userID))
	if err != nil {
		if dbx.IsNoRows(err) {
			return nil, op.Expected("not found", ErrNotFound)
		}
		return nil, op.Fail(err)
	}
	op.Attr(
		attribute.String("member.id", m.ID),
		attribute.String("member.status", string(m.Status)),
	)
	op.OK("ok")
	return m, nil
}

func (s *Store) ListMembers(ctx context.Context, organizationID string, limit, offset int) ([]*Member, bool, error) {
	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "list_members", dbx.DefaultTimeout,
		attribute.String("organization.id", organizationID),
	)
	defer cancel()
	defer op.End()

	rows, err := s.pool.Query(ctx, `
		SELECT id, organization_id, user_id, service_account_id, role, status, invited_by, created_at, updated_at
		FROM members
		WHERE organization_id = $1 AND status <> 'removed'
		ORDER BY created_at ASC
		LIMIT $2 OFFSET $3
	`, organizationID, limit+1, offset)
	if err != nil {
		return nil, false, op.Fail(err)
	}
	defer rows.Close()

	var out []*Member
	for rows.Next() {
		m, err := scanMember(rows)
		if err != nil {
			return nil, false, op.Fail(err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, false, op.Fail(err)
	}

	hasMore := len(out) > limit
	if hasMore {
		out = out[:limit]
	}
	op.Attr(attribute.Int("members.count", len(out)))
	op.OK("ok")
	return out, hasMore, nil
}

// CreateUserMember inserts a new user member, or reactivates a removed one.
// Reactivation keeps the existing member id and created_at.
func (s *Store) CreateUserMember(ctx context.Context, m *Member) error {
	if m.UserID == nil || m.ServiceAccountID != nil {
		return fmt.Errorf("create_user_member: user_id required")
	}

	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "create_user_member", dbx.DefaultTimeout,
		attribute.String("organization.id", m.OrganizationID),
		attribute.String("member.id", m.ID),
	)
	defer cancel()
	defer op.End()

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return op.Fail(err)
	}
	defer tx.Rollback(ctx)

	existing, err := scanMember(tx.QueryRow(ctx, `
		SELECT id, organization_id, user_id, service_account_id, role, status, invited_by, created_at, updated_at
		FROM members
		WHERE organization_id = $1 AND user_id = $2
		FOR UPDATE
	`, m.OrganizationID, *m.UserID))
	if err != nil && !dbx.IsNoRows(err) {
		return op.Fail(err)
	}

	if err == nil {
		if existing.Status != StatusRemoved {
			return op.SoftFail("conflict", ErrConflict, ErrConflict)
		}
		now := time.Now().UTC()
		_, err = tx.Exec(ctx, `
			UPDATE members
			SET role = $3, status = $4, invited_by = $5, updated_at = $6
			WHERE organization_id = $1 AND user_id = $2 AND status = 'removed'
		`, m.OrganizationID, *m.UserID, m.Role, m.Status, m.InvitedBy, now)
		if err != nil {
			return op.Fail(err)
		}
		if err := tx.Commit(ctx); err != nil {
			return op.Fail(err)
		}
		m.ID = existing.ID
		m.CreatedAt = existing.CreatedAt
		m.UpdatedAt = now
		op.Attr(attribute.String("member.id", m.ID))
		op.OK("reactivated")
		return nil
	}

	_, err = tx.Exec(ctx, `
		INSERT INTO members
			(id, organization_id, user_id, service_account_id, role, status, invited_by, created_at, updated_at)
		VALUES ($1, $2, $3, NULL, $4, $5, $6, $7, $8)
	`, m.ID, m.OrganizationID, *m.UserID, m.Role, m.Status, m.InvitedBy, m.CreatedAt, m.UpdatedAt)
	if err != nil {
		if dbx.IsUniqueViolation(err) {
			return op.SoftFail("conflict", ErrConflict, ErrConflict)
		}
		return op.Fail(err)
	}

	if err := tx.Commit(ctx); err != nil {
		return op.Fail(err)
	}
	op.OK("ok")
	return nil
}

// MutateMember locks all members for the org, loads the target row,
// invokes fn with the active-owner count, and persists role/status on success.
func (s *Store) MutateMember(ctx context.Context, organizationID, memberID string, fn func(m *Member, activeOwners int) error) (*Member, error) {
	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "mutate_member", dbx.DefaultTimeout,
		attribute.String("organization.id", organizationID),
		attribute.String("member.id", memberID),
	)
	defer cancel()
	defer op.End()

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, op.Fail(err)
	}
	defer tx.Rollback(ctx)

	rows, err := tx.Query(ctx, `
		SELECT id, organization_id, user_id, service_account_id, role, status, invited_by, created_at, updated_at
		FROM members
		WHERE organization_id = $1
		FOR UPDATE
	`, organizationID)
	if err != nil {
		return nil, op.Fail(err)
	}

	var target *Member
	activeOwners := 0
	for rows.Next() {
		m, scanErr := scanMember(rows)
		if scanErr != nil {
			rows.Close()
			return nil, op.Fail(scanErr)
		}
		if m.ID == memberID {
			target = m
		}
		if m.Role == RoleOwner && m.Status == StatusActive {
			activeOwners++
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, op.Fail(err)
	}
	rows.Close()

	if target == nil || target.Status == StatusRemoved {
		return nil, op.Expected("not found", ErrNotFound)
	}

	if err := fn(target, activeOwners); err != nil {
		return nil, op.Expected("rejected", err)
	}

	target.UpdatedAt = time.Now().UTC()
	tag, err := tx.Exec(ctx, `
		UPDATE members
		SET role = $3, status = $4, updated_at = $5
		WHERE organization_id = $1 AND id = $2
	`, organizationID, memberID, target.Role, target.Status, target.UpdatedAt)
	if err != nil {
		return nil, op.Fail(err)
	}
	if tag.RowsAffected() == 0 {
		return nil, op.Expected("not found", ErrNotFound)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, op.Fail(err)
	}
	op.OK("ok")
	return target, nil
}

// CreateServiceAccount inserts SA + active member in one transaction.
// Role defaults to member; SA cannot be owner (DB check + code).
