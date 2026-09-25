package orgs

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel/attribute"

	"github.com/plat5dev/plat5/identity/internal/dbx"
)

func (s *Store) GetMember(ctx context.Context, memberID string) (*Member, error) {
	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "get_member", dbx.DefaultTimeout,
		attribute.String("member.id", memberID),
	)
	defer cancel()
	defer op.End()

	m, err := scanMember(s.pool.QueryRow(ctx, `
		SELECT `+memberCols+`
		FROM members
		WHERE id = $1
	`, memberID))
	if err != nil {
		if dbx.IsNoRows(err) {
			return nil, op.Expected("not found", ErrNotFound)
		}
		return nil, op.Fail(err)
	}
	op.OK("ok")
	return m, nil
}

// ResolveMember returns any non-removed user member for (user, org). Used by session mint.
func (s *Store) ResolveMember(ctx context.Context, userID, organizationID string) (*Member, error) {
	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "resolve_member", dbx.DefaultTimeout,
		attribute.String("organization.id", organizationID),
		attribute.String("user.id", userID),
	)
	defer cancel()
	defer op.End()

	m, err := scanMember(s.pool.QueryRow(ctx, `
		SELECT `+memberCols+`
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

func (s *Store) ListMembers(ctx context.Context, organizationID string, limit int, startingAfter string) ([]*Member, bool, error) {
	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "list_members", dbx.DefaultTimeout,
		attribute.String("organization.id", organizationID),
	)
	defer cancel()
	defer op.End()

	var after any
	if startingAfter != "" {
		after = startingAfter
	}
	rows, err := s.pool.Query(ctx, `
		SELECT `+memberCols+`
		FROM members
		WHERE organization_id = $1 AND status <> 'removed'
		AND ($2::text IS NULL OR id > $2)
		ORDER BY id ASC
		LIMIT $3
	`, organizationID, after, limit+1)
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

func (s *Store) ListMemberships(ctx context.Context, userID string, limit int, startingAfter string) ([]*Membership, bool, error) {
	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "list_memberships", dbx.DefaultTimeout,
		attribute.String("user.id", userID),
	)
	defer cancel()
	defer op.End()

	var after any
	if startingAfter != "" {
		after = startingAfter
	}
	rows, err := s.pool.Query(ctx, `
		SELECT m.id, m.status, o.id, o.name, o.slug
		FROM members m
		INNER JOIN organizations o ON o.id = m.organization_id
		WHERE m.user_id = $1 AND m.status = 'active'
		AND ($2::text IS NULL OR m.id > $2)
		ORDER BY m.id ASC
		LIMIT $3
	`, userID, after, limit+1)
	if err != nil {
		return nil, false, op.Fail(err)
	}
	defer rows.Close()

	var out []*Membership
	for rows.Next() {
		m, err := scanMembership(rows)
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
	op.Attr(attribute.Int("memberships.count", len(out)))
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
		SELECT `+memberCols+`
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
			SET status = $3, added_by = $4, updated_at = $5
			WHERE organization_id = $1 AND user_id = $2 AND status = 'removed'
		`, m.OrganizationID, *m.UserID, m.Status, m.AddedBy, now)
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
			(id, organization_id, user_id, service_account_id, status, added_by, created_at, updated_at)
		VALUES ($1, $2, $3, NULL, $4, $5, $6, $7)
	`, m.ID, m.OrganizationID, *m.UserID, m.Status, m.AddedBy, m.CreatedAt, m.UpdatedAt)
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

// MutateMember locks every member in the target's org, invokes fn with the
// non-removed count, and persists status on success. Removed is not an address.
func (s *Store) MutateMember(ctx context.Context, memberID string, fn func(m *Member, nonRemoved int) error) (*Member, error) {
	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "mutate_member", dbx.DefaultTimeout,
		attribute.String("member.id", memberID),
	)
	defer cancel()
	defer op.End()

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, op.Fail(err)
	}
	defer tx.Rollback(ctx)

	var organizationID string
	err = tx.QueryRow(ctx, `SELECT organization_id FROM members WHERE id = $1`, memberID).Scan(&organizationID)
	if err != nil {
		if dbx.IsNoRows(err) {
			return nil, op.Expected("not found", ErrNotFound)
		}
		return nil, op.Fail(err)
	}
	op.Attr(attribute.String("organization.id", organizationID))

	members, err := lockOrgMembers(ctx, tx, organizationID)
	if err != nil {
		return nil, op.Fail(err)
	}

	var target *Member
	for _, m := range members {
		if m.ID == memberID {
			target = m
			break
		}
	}
	if target == nil || target.Status == StatusRemoved {
		return nil, op.Expected("not found", ErrNotFound)
	}

	if err := fn(target, countNonRemoved(members)); err != nil {
		return nil, op.Expected("rejected", err)
	}

	target.UpdatedAt = time.Now().UTC()
	tag, err := tx.Exec(ctx, `
		UPDATE members
		SET status = $3, updated_at = $4
		WHERE organization_id = $1 AND id = $2
	`, organizationID, memberID, target.Status, target.UpdatedAt)
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

func lockOrgMembers(ctx context.Context, tx pgx.Tx, organizationID string) ([]*Member, error) {
	rows, err := tx.Query(ctx, `
		SELECT `+memberCols+`
		FROM members
		WHERE organization_id = $1
		ORDER BY id
		FOR UPDATE
	`, organizationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*Member
	for rows.Next() {
		m, err := scanMember(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func countNonRemoved(members []*Member) int {
	n := 0
	for _, m := range members {
		if m.Status != StatusRemoved {
			n++
		}
	}
	return n
}
