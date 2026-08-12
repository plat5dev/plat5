package orgs

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/plat5dev/plat5/organizations/metrics"
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
	ctx, span := s.tracer.Start(ctx, "db.create_organization")
	defer span.End()
	span.SetAttributes(
		attribute.String("db.system", "postgresql"),
		attribute.String("db.operation.name", "create_organization"),
		attribute.String("organization.id", org.ID),
	)

	start := time.Now()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, s.fail(span, "create_organization", start, err)
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
		if isUniqueViolation(err) {
			metrics.RecordDBOperation("create_organization", time.Since(start), ErrConflict)
			span.SetStatus(codes.Ok, "slug conflict")
			return nil, ErrConflict
		}
		return nil, s.fail(span, "create_organization", start, err)
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
		return nil, s.fail(span, "create_organization", start, err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, s.fail(span, "create_organization", start, err)
	}

	metrics.RecordDBOperation("create_organization", time.Since(start), nil)
	span.SetStatus(codes.Ok, "created")
	return m, nil
}

func (s *Store) GetOrganization(ctx context.Context, organizationID string) (*Organization, error) {
	ctx, span := s.tracer.Start(ctx, "db.get_organization")
	defer span.End()
	span.SetAttributes(
		attribute.String("db.system", "postgresql"),
		attribute.String("db.operation.name", "get_organization"),
		attribute.String("organization.id", organizationID),
	)

	start := time.Now()
	org, err := scanOrg(s.pool.QueryRow(ctx, `
		SELECT id, name, slug, settings, created_at, updated_at
		FROM organizations WHERE id = $1
	`, organizationID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			metrics.RecordDBOperation("get_organization", time.Since(start), nil)
			span.SetStatus(codes.Ok, "not found")
			return nil, ErrNotFound
		}
		return nil, s.fail(span, "get_organization", start, err)
	}

	metrics.RecordDBOperation("get_organization", time.Since(start), nil)
	span.SetStatus(codes.Ok, "ok")
	return org, nil
}

func (s *Store) ListOrganizationsForUser(ctx context.Context, userID string, limit, offset int) ([]*Organization, bool, error) {
	ctx, span := s.tracer.Start(ctx, "db.list_organizations_for_user")
	defer span.End()
	span.SetAttributes(
		attribute.String("db.system", "postgresql"),
		attribute.String("db.operation.name", "list_organizations_for_user"),
		attribute.String("user.id", userID),
	)

	start := time.Now()
	rows, err := s.pool.Query(ctx, `
		SELECT o.id, o.name, o.slug, o.settings, o.created_at, o.updated_at
		FROM organizations o
		INNER JOIN members m ON m.organization_id = o.id
		WHERE m.user_id = $1 AND m.status = 'active'
		ORDER BY o.created_at DESC
		LIMIT $2 OFFSET $3
	`, userID, limit+1, offset)
	if err != nil {
		return nil, false, s.fail(span, "list_organizations_for_user", start, err)
	}
	defer rows.Close()

	var out []*Organization
	for rows.Next() {
		org, err := scanOrg(rows)
		if err != nil {
			return nil, false, s.fail(span, "list_organizations_for_user", start, err)
		}
		out = append(out, org)
	}
	if err := rows.Err(); err != nil {
		return nil, false, s.fail(span, "list_organizations_for_user", start, err)
	}

	hasMore := len(out) > limit
	if hasMore {
		out = out[:limit]
	}

	metrics.RecordDBOperation("list_organizations_for_user", time.Since(start), nil)
	span.SetAttributes(attribute.Int("organizations.count", len(out)))
	span.SetStatus(codes.Ok, "ok")
	return out, hasMore, nil
}

func (s *Store) UpdateOrganization(ctx context.Context, org *Organization) error {
	ctx, span := s.tracer.Start(ctx, "db.update_organization")
	defer span.End()
	span.SetAttributes(
		attribute.String("db.system", "postgresql"),
		attribute.String("db.operation.name", "update_organization"),
		attribute.String("organization.id", org.ID),
	)

	start := time.Now()
	org.UpdatedAt = time.Now().UTC()
	tag, err := s.pool.Exec(ctx, `
		UPDATE organizations
		SET name = $2, slug = $3, settings = $4::jsonb, updated_at = $5
		WHERE id = $1
	`, org.ID, org.Name, org.Slug, org.Settings, org.UpdatedAt)
	if err != nil {
		if isUniqueViolation(err) {
			metrics.RecordDBOperation("update_organization", time.Since(start), ErrConflict)
			span.SetStatus(codes.Ok, "slug conflict")
			return ErrConflict
		}
		return s.fail(span, "update_organization", start, err)
	}
	if tag.RowsAffected() == 0 {
		metrics.RecordDBOperation("update_organization", time.Since(start), nil)
		span.SetStatus(codes.Ok, "not found")
		return ErrNotFound
	}

	metrics.RecordDBOperation("update_organization", time.Since(start), nil)
	span.SetStatus(codes.Ok, "ok")
	return nil
}

func (s *Store) DeleteOrganization(ctx context.Context, organizationID string) error {
	ctx, span := s.tracer.Start(ctx, "db.delete_organization")
	defer span.End()
	span.SetAttributes(
		attribute.String("db.system", "postgresql"),
		attribute.String("db.operation.name", "delete_organization"),
		attribute.String("organization.id", organizationID),
	)

	start := time.Now()
	tag, err := s.pool.Exec(ctx, `DELETE FROM organizations WHERE id = $1`, organizationID)
	if err != nil {
		return s.fail(span, "delete_organization", start, err)
	}
	if tag.RowsAffected() == 0 {
		metrics.RecordDBOperation("delete_organization", time.Since(start), nil)
		span.SetStatus(codes.Ok, "not found")
		return ErrNotFound
	}

	metrics.RecordDBOperation("delete_organization", time.Since(start), nil)
	span.SetStatus(codes.Ok, "ok")
	return nil
}

func (s *Store) GetMember(ctx context.Context, organizationID, memberID string) (*Member, error) {
	ctx, span := s.tracer.Start(ctx, "db.get_member")
	defer span.End()
	span.SetAttributes(
		attribute.String("db.system", "postgresql"),
		attribute.String("db.operation.name", "get_member"),
		attribute.String("organization.id", organizationID),
		attribute.String("member.id", memberID),
	)

	start := time.Now()
	m, err := scanMember(s.pool.QueryRow(ctx, `
		SELECT id, organization_id, user_id, service_account_id, role, status, invited_by, created_at, updated_at
		FROM members
		WHERE organization_id = $1 AND id = $2
	`, organizationID, memberID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			metrics.RecordDBOperation("get_member", time.Since(start), nil)
			span.SetStatus(codes.Ok, "not found")
			return nil, ErrNotFound
		}
		return nil, s.fail(span, "get_member", start, err)
	}

	metrics.RecordDBOperation("get_member", time.Since(start), nil)
	span.SetStatus(codes.Ok, "ok")
	return m, nil
}

func (s *Store) GetActiveMemberForUser(ctx context.Context, organizationID, userID string) (*Member, error) {
	ctx, span := s.tracer.Start(ctx, "db.get_active_member_for_user")
	defer span.End()
	span.SetAttributes(
		attribute.String("db.system", "postgresql"),
		attribute.String("db.operation.name", "get_active_member_for_user"),
		attribute.String("organization.id", organizationID),
		attribute.String("user.id", userID),
	)

	start := time.Now()
	m, err := scanMember(s.pool.QueryRow(ctx, `
		SELECT id, organization_id, user_id, service_account_id, role, status, invited_by, created_at, updated_at
		FROM members
		WHERE organization_id = $1 AND user_id = $2 AND status = 'active'
	`, organizationID, userID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			metrics.RecordDBOperation("get_active_member_for_user", time.Since(start), nil)
			span.SetStatus(codes.Ok, "not found")
			return nil, ErrNotFound
		}
		return nil, s.fail(span, "get_active_member_for_user", start, err)
	}

	metrics.RecordDBOperation("get_active_member_for_user", time.Since(start), nil)
	span.SetStatus(codes.Ok, "ok")
	return m, nil
}

// ResolveMember returns any non-removed user member for (user, org). Used by internal resolve.
func (s *Store) ResolveMember(ctx context.Context, userID, organizationID string) (*Member, error) {
	ctx, span := s.tracer.Start(ctx, "db.resolve_member")
	defer span.End()
	span.SetAttributes(
		attribute.String("db.system", "postgresql"),
		attribute.String("db.operation.name", "resolve_member"),
		attribute.String("organization.id", organizationID),
		attribute.String("user.id", userID),
	)

	start := time.Now()
	m, err := scanMember(s.pool.QueryRow(ctx, `
		SELECT id, organization_id, user_id, service_account_id, role, status, invited_by, created_at, updated_at
		FROM members
		WHERE organization_id = $1 AND user_id = $2 AND status <> 'removed'
	`, organizationID, userID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			metrics.RecordDBOperation("resolve_member", time.Since(start), nil)
			span.SetStatus(codes.Ok, "not found")
			return nil, ErrNotFound
		}
		return nil, s.fail(span, "resolve_member", start, err)
	}

	metrics.RecordDBOperation("resolve_member", time.Since(start), nil)
	span.SetAttributes(
		attribute.String("member.id", m.ID),
		attribute.String("member.status", string(m.Status)),
	)
	span.SetStatus(codes.Ok, "ok")
	return m, nil
}

func (s *Store) ListMembers(ctx context.Context, organizationID string, limit, offset int) ([]*Member, bool, error) {
	ctx, span := s.tracer.Start(ctx, "db.list_members")
	defer span.End()
	span.SetAttributes(
		attribute.String("db.system", "postgresql"),
		attribute.String("db.operation.name", "list_members"),
		attribute.String("organization.id", organizationID),
	)

	start := time.Now()
	rows, err := s.pool.Query(ctx, `
		SELECT id, organization_id, user_id, service_account_id, role, status, invited_by, created_at, updated_at
		FROM members
		WHERE organization_id = $1 AND status <> 'removed'
		ORDER BY created_at ASC
		LIMIT $2 OFFSET $3
	`, organizationID, limit+1, offset)
	if err != nil {
		return nil, false, s.fail(span, "list_members", start, err)
	}
	defer rows.Close()

	var out []*Member
	for rows.Next() {
		m, err := scanMember(rows)
		if err != nil {
			return nil, false, s.fail(span, "list_members", start, err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, false, s.fail(span, "list_members", start, err)
	}

	hasMore := len(out) > limit
	if hasMore {
		out = out[:limit]
	}

	metrics.RecordDBOperation("list_members", time.Since(start), nil)
	span.SetAttributes(attribute.Int("members.count", len(out)))
	span.SetStatus(codes.Ok, "ok")
	return out, hasMore, nil
}

// CreateUserMember inserts a new user member, or reactivates a removed one.
// Reactivation keeps the existing member id and created_at.
func (s *Store) CreateUserMember(ctx context.Context, m *Member) error {
	if m.UserID == nil || m.ServiceAccountID != nil {
		return fmt.Errorf("create_user_member: user_id required")
	}

	ctx, span := s.tracer.Start(ctx, "db.create_user_member")
	defer span.End()
	span.SetAttributes(
		attribute.String("db.system", "postgresql"),
		attribute.String("db.operation.name", "create_user_member"),
		attribute.String("organization.id", m.OrganizationID),
		attribute.String("member.id", m.ID),
	)

	start := time.Now()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return s.fail(span, "create_user_member", start, err)
	}
	defer tx.Rollback(ctx)

	existing, err := scanMember(tx.QueryRow(ctx, `
		SELECT id, organization_id, user_id, service_account_id, role, status, invited_by, created_at, updated_at
		FROM members
		WHERE organization_id = $1 AND user_id = $2
		FOR UPDATE
	`, m.OrganizationID, *m.UserID))
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return s.fail(span, "create_user_member", start, err)
	}

	if err == nil {
		if existing.Status != StatusRemoved {
			metrics.RecordDBOperation("create_user_member", time.Since(start), ErrConflict)
			span.SetStatus(codes.Ok, "conflict")
			return ErrConflict
		}
		now := time.Now().UTC()
		_, err = tx.Exec(ctx, `
			UPDATE members
			SET role = $3, status = $4, invited_by = $5, updated_at = $6
			WHERE organization_id = $1 AND user_id = $2 AND status = 'removed'
		`, m.OrganizationID, *m.UserID, m.Role, m.Status, m.InvitedBy, now)
		if err != nil {
			return s.fail(span, "create_user_member", start, err)
		}
		if err := tx.Commit(ctx); err != nil {
			return s.fail(span, "create_user_member", start, err)
		}
		m.ID = existing.ID
		m.CreatedAt = existing.CreatedAt
		m.UpdatedAt = now
		metrics.RecordDBOperation("create_user_member", time.Since(start), nil)
		span.SetAttributes(attribute.String("member.id", m.ID))
		span.SetStatus(codes.Ok, "reactivated")
		return nil
	}

	_, err = tx.Exec(ctx, `
		INSERT INTO members
			(id, organization_id, user_id, service_account_id, role, status, invited_by, created_at, updated_at)
		VALUES ($1, $2, $3, NULL, $4, $5, $6, $7, $8)
	`, m.ID, m.OrganizationID, *m.UserID, m.Role, m.Status, m.InvitedBy, m.CreatedAt, m.UpdatedAt)
	if err != nil {
		if isUniqueViolation(err) {
			metrics.RecordDBOperation("create_user_member", time.Since(start), ErrConflict)
			span.SetStatus(codes.Ok, "conflict")
			return ErrConflict
		}
		return s.fail(span, "create_user_member", start, err)
	}

	if err := tx.Commit(ctx); err != nil {
		return s.fail(span, "create_user_member", start, err)
	}

	metrics.RecordDBOperation("create_user_member", time.Since(start), nil)
	span.SetStatus(codes.Ok, "ok")
	return nil
}

// MutateMember locks all members for the org, loads the target row,
// invokes fn with the active-owner count, and persists role/status on success.
func (s *Store) MutateMember(ctx context.Context, organizationID, memberID string, fn func(m *Member, activeOwners int) error) (*Member, error) {
	ctx, span := s.tracer.Start(ctx, "db.mutate_member")
	defer span.End()
	span.SetAttributes(
		attribute.String("db.system", "postgresql"),
		attribute.String("db.operation.name", "mutate_member"),
		attribute.String("organization.id", organizationID),
		attribute.String("member.id", memberID),
	)

	start := time.Now()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, s.fail(span, "mutate_member", start, err)
	}
	defer tx.Rollback(ctx)

	rows, err := tx.Query(ctx, `
		SELECT id, organization_id, user_id, service_account_id, role, status, invited_by, created_at, updated_at
		FROM members
		WHERE organization_id = $1
		FOR UPDATE
	`, organizationID)
	if err != nil {
		return nil, s.fail(span, "mutate_member", start, err)
	}

	var target *Member
	activeOwners := 0
	for rows.Next() {
		m, scanErr := scanMember(rows)
		if scanErr != nil {
			rows.Close()
			return nil, s.fail(span, "mutate_member", start, scanErr)
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
		return nil, s.fail(span, "mutate_member", start, err)
	}
	rows.Close()

	if target == nil || target.Status == StatusRemoved {
		metrics.RecordDBOperation("mutate_member", time.Since(start), nil)
		span.SetStatus(codes.Ok, "not found")
		return nil, ErrNotFound
	}

	if err := fn(target, activeOwners); err != nil {
		metrics.RecordDBOperation("mutate_member", time.Since(start), nil)
		span.SetStatus(codes.Ok, "rejected")
		return nil, err
	}

	target.UpdatedAt = time.Now().UTC()
	tag, err := tx.Exec(ctx, `
		UPDATE members
		SET role = $3, status = $4, updated_at = $5
		WHERE organization_id = $1 AND id = $2
	`, organizationID, memberID, target.Role, target.Status, target.UpdatedAt)
	if err != nil {
		return nil, s.fail(span, "mutate_member", start, err)
	}
	if tag.RowsAffected() == 0 {
		metrics.RecordDBOperation("mutate_member", time.Since(start), nil)
		span.SetStatus(codes.Ok, "not found")
		return nil, ErrNotFound
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, s.fail(span, "mutate_member", start, err)
	}

	metrics.RecordDBOperation("mutate_member", time.Since(start), nil)
	span.SetStatus(codes.Ok, "ok")
	return target, nil
}

func (s *Store) fail(span trace.Span, op string, start time.Time, err error) error {
	metrics.RecordDBOperation(op, time.Since(start), err)
	span.SetStatus(codes.Error, err.Error())
	span.RecordError(err)
	return fmt.Errorf("%s: %w", op, err)
}

type scannable interface {
	Scan(dest ...any) error
}

func scanOrg(row scannable) (*Organization, error) {
	var o Organization
	err := row.Scan(&o.ID, &o.Name, &o.Slug, &o.Settings, &o.CreatedAt, &o.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &o, nil
}

func scanMember(row scannable) (*Member, error) {
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

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
