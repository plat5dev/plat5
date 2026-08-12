package orgs

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/plat5dev/plat5/identity/internal/dbx"
)

var (
	ErrNotFound = errors.New("not found")
	ErrConflict = errors.New("conflict")
)

type Store struct {
	pool   *pgxpool.Pool
	tracer trace.Tracer
}

func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{
		pool:   pool,
		tracer: otel.Tracer("identity.store"),
	}
}

func (s *Store) CreateOrganization(ctx context.Context, org *Organization, ownerUserID string) (*Member, error) {
	ctx, op := dbx.Begin(ctx, s.tracer, "create_organization",
		attribute.String("organization.id", org.ID),
	)
	defer op.End()

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, op.Fail(err)
	}
	defer tx.Rollback(ctx)

	if org.Settings == nil {
		org.Settings = []byte("{}")
	}

	_, err = tx.Exec(ctx, `
		INSERT INTO organizations (id, name, slug, settings, created_at, updated_at)
		VALUES ($1, $2, $3, $4::jsonb, $5, $6)
	`, org.ID, org.Name, org.Slug, org.Settings, org.CreatedAt, org.UpdatedAt)
	if err != nil {
		if dbx.IsUniqueViolation(err) {
			return nil, op.SoftFail("slug conflict", ErrConflict, ErrConflict)
		}
		return nil, op.Fail(err)
	}

	m := &Member{
		ID:             NewULID(),
		OrganizationID: org.ID,
		UserID:         &ownerUserID,
		Role:           RoleOwner,
		Status:         StatusActive,
		CreatedAt:      org.CreatedAt,
		UpdatedAt:      org.UpdatedAt,
	}

	_, err = tx.Exec(ctx, `
		INSERT INTO members
			(id, organization_id, user_id, service_account_id, role, status, invited_by, created_at, updated_at)
		VALUES ($1, $2, $3, NULL, $4, $5, NULL, $6, $7)
	`, m.ID, m.OrganizationID, ownerUserID, m.Role, m.Status, m.CreatedAt, m.UpdatedAt)
	if err != nil {
		return nil, op.Fail(err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, op.Fail(err)
	}

	op.OK("created")
	return m, nil
}

func (s *Store) GetOrganization(ctx context.Context, organizationID string) (*Organization, error) {
	ctx, op := dbx.Begin(ctx, s.tracer, "get_organization",
		attribute.String("organization.id", organizationID),
	)
	defer op.End()

	org, err := scanOrg(s.pool.QueryRow(ctx, `
		SELECT id, name, slug, settings, created_at, updated_at
		FROM organizations WHERE id = $1
	`, organizationID))
	if err != nil {
		if dbx.IsNoRows(err) {
			return nil, op.Expected("not found", ErrNotFound)
		}
		return nil, op.Fail(err)
	}
	op.OK("ok")
	return org, nil
}

func (s *Store) ListOrganizationsForUser(ctx context.Context, userID string, limit, offset int) ([]*Organization, bool, error) {
	ctx, op := dbx.Begin(ctx, s.tracer, "list_organizations_for_user",
		attribute.String("user.id", userID),
	)
	defer op.End()

	rows, err := s.pool.Query(ctx, `
		SELECT o.id, o.name, o.slug, o.settings, o.created_at, o.updated_at
		FROM organizations o
		INNER JOIN members m ON m.organization_id = o.id
		WHERE m.user_id = $1 AND m.status = 'active'
		ORDER BY o.created_at DESC
		LIMIT $2 OFFSET $3
	`, userID, limit+1, offset)
	if err != nil {
		return nil, false, op.Fail(err)
	}
	defer rows.Close()

	var out []*Organization
	for rows.Next() {
		org, err := scanOrg(rows)
		if err != nil {
			return nil, false, op.Fail(err)
		}
		out = append(out, org)
	}
	if err := rows.Err(); err != nil {
		return nil, false, op.Fail(err)
	}

	hasMore := len(out) > limit
	if hasMore {
		out = out[:limit]
	}
	op.Attr(attribute.Int("organizations.count", len(out)))
	op.OK("ok")
	return out, hasMore, nil
}

func (s *Store) UpdateOrganization(ctx context.Context, org *Organization) error {
	ctx, op := dbx.Begin(ctx, s.tracer, "update_organization",
		attribute.String("organization.id", org.ID),
	)
	defer op.End()

	org.UpdatedAt = time.Now().UTC()
	tag, err := s.pool.Exec(ctx, `
		UPDATE organizations
		SET name = $2, slug = $3, settings = $4::jsonb, updated_at = $5
		WHERE id = $1
	`, org.ID, org.Name, org.Slug, org.Settings, org.UpdatedAt)
	if err != nil {
		if dbx.IsUniqueViolation(err) {
			return op.SoftFail("slug conflict", ErrConflict, ErrConflict)
		}
		return op.Fail(err)
	}
	if tag.RowsAffected() == 0 {
		return op.Expected("not found", ErrNotFound)
	}
	op.OK("ok")
	return nil
}

func (s *Store) DeleteOrganization(ctx context.Context, organizationID string) error {
	ctx, op := dbx.Begin(ctx, s.tracer, "delete_organization",
		attribute.String("organization.id", organizationID),
	)
	defer op.End()

	tag, err := s.pool.Exec(ctx, `DELETE FROM organizations WHERE id = $1`, organizationID)
	if err != nil {
		return op.Fail(err)
	}
	if tag.RowsAffected() == 0 {
		return op.Expected("not found", ErrNotFound)
	}
	op.OK("ok")
	return nil
}

func (s *Store) GetMember(ctx context.Context, organizationID, memberID string) (*Member, error) {
	ctx, op := dbx.Begin(ctx, s.tracer, "get_member",
		attribute.String("organization.id", organizationID),
		attribute.String("member.id", memberID),
	)
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
	ctx, op := dbx.Begin(ctx, s.tracer, "get_active_member_for_user",
		attribute.String("organization.id", organizationID),
		attribute.String("user.id", userID),
	)
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
	ctx, op := dbx.Begin(ctx, s.tracer, "resolve_member",
		attribute.String("organization.id", organizationID),
		attribute.String("user.id", userID),
	)
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
	ctx, op := dbx.Begin(ctx, s.tracer, "list_members",
		attribute.String("organization.id", organizationID),
	)
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

	ctx, op := dbx.Begin(ctx, s.tracer, "create_user_member",
		attribute.String("organization.id", m.OrganizationID),
		attribute.String("member.id", m.ID),
	)
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
	ctx, op := dbx.Begin(ctx, s.tracer, "mutate_member",
		attribute.String("organization.id", organizationID),
		attribute.String("member.id", memberID),
	)
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
func (s *Store) CreateServiceAccount(ctx context.Context, sa *ServiceAccount, role Role, invitedBy *string) (*Member, error) {
	if role == RoleOwner {
		return nil, fmt.Errorf("create_service_account: service accounts cannot be owners")
	}
	if role == "" {
		role = RoleMember
	}

	ctx, op := dbx.Begin(ctx, s.tracer, "create_service_account",
		attribute.String("organization.id", sa.OrganizationID),
		attribute.String("service_account.id", sa.ID),
	)
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
		InvitedBy:        invitedBy,
		CreatedAt:        now,
		UpdatedAt:        now,
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO members
			(id, organization_id, user_id, service_account_id, role, status, invited_by, created_at, updated_at)
		VALUES ($1, $2, NULL, $3, $4, $5, $6, $7, $8)
	`, m.ID, m.OrganizationID, sa.ID, m.Role, m.Status, m.InvitedBy, m.CreatedAt, m.UpdatedAt)
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

	sa, err := scanServiceAccount(s.pool.QueryRow(ctx, `
		SELECT
			sa.id, sa.home_organization_id, m.id, sa.name, sa.created_by_user_id,
			sa.disabled_at, sa.created_at, sa.updated_at
		FROM service_accounts sa
		INNER JOIN members m
			ON m.service_account_id = sa.id
			AND m.organization_id = sa.home_organization_id
			AND m.status <> 'removed'
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

	rows, err := s.pool.Query(ctx, `
		SELECT
			sa.id, sa.home_organization_id, m.id, sa.name, sa.created_by_user_id,
			sa.disabled_at, sa.created_at, sa.updated_at
		FROM service_accounts sa
		INNER JOIN members m
			ON m.service_account_id = sa.id
			AND m.organization_id = sa.home_organization_id
			AND m.status <> 'removed'
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
	ctx, op := dbx.Begin(ctx, s.tracer, "update_service_account",
		attribute.String("service_account.id", serviceAccountID),
	)
	defer op.End()

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, op.Fail(err)
	}
	defer tx.Rollback(ctx)

	sa, err := scanServiceAccount(tx.QueryRow(ctx, `
		SELECT
			sa.id, sa.home_organization_id, m.id, sa.name, sa.created_by_user_id,
			sa.disabled_at, sa.created_at, sa.updated_at
		FROM service_accounts sa
		INNER JOIN members m
			ON m.service_account_id = sa.id
			AND m.organization_id = sa.home_organization_id
			AND m.status <> 'removed'
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
	ctx, op := dbx.Begin(ctx, s.tracer, "delete_service_account",
		attribute.String("service_account.id", serviceAccountID),
	)
	defer op.End()

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return op.Fail(err)
	}
	defer tx.Rollback(ctx)

	sa, err := scanServiceAccount(tx.QueryRow(ctx, `
		SELECT
			sa.id, sa.home_organization_id, m.id, sa.name, sa.created_by_user_id,
			sa.disabled_at, sa.created_at, sa.updated_at
		FROM service_accounts sa
		INNER JOIN members m
			ON m.service_account_id = sa.id
			AND m.organization_id = sa.home_organization_id
			AND m.status <> 'removed'
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
		&m.InvitedBy,
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
