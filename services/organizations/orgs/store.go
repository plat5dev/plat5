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
		tracer: otel.Tracer("organizations.store"),
	}
}

func (s *Store) CreateOrganization(ctx context.Context, org *Organization, ownerUserID string) (*Membership, error) {
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

	m := &Membership{
		ID:             NewULID(),
		OrganizationID: org.ID,
		UserID:         ownerUserID,
		Role:           RoleOwner,
		Status:         StatusActive,
		CreatedAt:      org.CreatedAt,
		UpdatedAt:      org.UpdatedAt,
	}

	_, err = tx.Exec(ctx, `
		INSERT INTO organization_memberships
			(id, organization_id, user_id, role, status, invited_by, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, NULL, $6, $7)
	`, m.ID, m.OrganizationID, m.UserID, m.Role, m.Status, m.CreatedAt, m.UpdatedAt)
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
		INNER JOIN organization_memberships m ON m.organization_id = o.id
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

func (s *Store) GetMembership(ctx context.Context, organizationID, membershipID string) (*Membership, error) {
	ctx, span := s.tracer.Start(ctx, "db.get_membership")
	defer span.End()
	span.SetAttributes(
		attribute.String("db.system", "postgresql"),
		attribute.String("db.operation.name", "get_membership"),
		attribute.String("organization.id", organizationID),
		attribute.String("membership.id", membershipID),
	)

	start := time.Now()
	m, err := scanMembership(s.pool.QueryRow(ctx, `
		SELECT id, organization_id, user_id, role, status, invited_by, created_at, updated_at
		FROM organization_memberships
		WHERE organization_id = $1 AND id = $2
	`, organizationID, membershipID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			metrics.RecordDBOperation("get_membership", time.Since(start), nil)
			span.SetStatus(codes.Ok, "not found")
			return nil, ErrNotFound
		}
		return nil, s.fail(span, "get_membership", start, err)
	}

	metrics.RecordDBOperation("get_membership", time.Since(start), nil)
	span.SetStatus(codes.Ok, "ok")
	return m, nil
}

func (s *Store) GetActiveMembership(ctx context.Context, organizationID, userID string) (*Membership, error) {
	ctx, span := s.tracer.Start(ctx, "db.get_active_membership")
	defer span.End()
	span.SetAttributes(
		attribute.String("db.system", "postgresql"),
		attribute.String("db.operation.name", "get_active_membership"),
		attribute.String("organization.id", organizationID),
		attribute.String("user.id", userID),
	)

	start := time.Now()
	m, err := scanMembership(s.pool.QueryRow(ctx, `
		SELECT id, organization_id, user_id, role, status, invited_by, created_at, updated_at
		FROM organization_memberships
		WHERE organization_id = $1 AND user_id = $2 AND status = 'active'
	`, organizationID, userID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			metrics.RecordDBOperation("get_active_membership", time.Since(start), nil)
			span.SetStatus(codes.Ok, "not found")
			return nil, ErrNotFound
		}
		return nil, s.fail(span, "get_active_membership", start, err)
	}

	metrics.RecordDBOperation("get_active_membership", time.Since(start), nil)
	span.SetStatus(codes.Ok, "ok")
	return m, nil
}

// ResolveMembership returns any non-removed row for (user, org). Used by internal resolve.
func (s *Store) ResolveMembership(ctx context.Context, userID, organizationID string) (*Membership, error) {
	ctx, span := s.tracer.Start(ctx, "db.resolve_membership")
	defer span.End()
	span.SetAttributes(
		attribute.String("db.system", "postgresql"),
		attribute.String("db.operation.name", "resolve_membership"),
		attribute.String("organization.id", organizationID),
		attribute.String("user.id", userID),
	)

	start := time.Now()
	m, err := scanMembership(s.pool.QueryRow(ctx, `
		SELECT id, organization_id, user_id, role, status, invited_by, created_at, updated_at
		FROM organization_memberships
		WHERE organization_id = $1 AND user_id = $2 AND status <> 'removed'
	`, organizationID, userID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			metrics.RecordDBOperation("resolve_membership", time.Since(start), nil)
			span.SetStatus(codes.Ok, "not found")
			return nil, ErrNotFound
		}
		return nil, s.fail(span, "resolve_membership", start, err)
	}

	metrics.RecordDBOperation("resolve_membership", time.Since(start), nil)
	span.SetAttributes(
		attribute.String("membership.id", m.ID),
		attribute.String("membership.status", string(m.Status)),
	)
	span.SetStatus(codes.Ok, "ok")
	return m, nil
}

func (s *Store) ListMemberships(ctx context.Context, organizationID string, limit, offset int) ([]*Membership, bool, error) {
	ctx, span := s.tracer.Start(ctx, "db.list_memberships")
	defer span.End()
	span.SetAttributes(
		attribute.String("db.system", "postgresql"),
		attribute.String("db.operation.name", "list_memberships"),
		attribute.String("organization.id", organizationID),
	)

	start := time.Now()
	rows, err := s.pool.Query(ctx, `
		SELECT id, organization_id, user_id, role, status, invited_by, created_at, updated_at
		FROM organization_memberships
		WHERE organization_id = $1 AND status <> 'removed'
		ORDER BY created_at ASC
		LIMIT $2 OFFSET $3
	`, organizationID, limit+1, offset)
	if err != nil {
		return nil, false, s.fail(span, "list_memberships", start, err)
	}
	defer rows.Close()

	var out []*Membership
	for rows.Next() {
		m, err := scanMembership(rows)
		if err != nil {
			return nil, false, s.fail(span, "list_memberships", start, err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, false, s.fail(span, "list_memberships", start, err)
	}

	hasMore := len(out) > limit
	if hasMore {
		out = out[:limit]
	}

	metrics.RecordDBOperation("list_memberships", time.Since(start), nil)
	span.SetAttributes(attribute.Int("memberships.count", len(out)))
	span.SetStatus(codes.Ok, "ok")
	return out, hasMore, nil
}

// CreateMembership inserts a new membership, or reactivates a removed one.
// Reactivation keeps the existing membership id and created_at.
func (s *Store) CreateMembership(ctx context.Context, m *Membership) error {
	ctx, span := s.tracer.Start(ctx, "db.create_membership")
	defer span.End()
	span.SetAttributes(
		attribute.String("db.system", "postgresql"),
		attribute.String("db.operation.name", "create_membership"),
		attribute.String("organization.id", m.OrganizationID),
		attribute.String("membership.id", m.ID),
	)

	start := time.Now()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return s.fail(span, "create_membership", start, err)
	}
	defer tx.Rollback(ctx)

	existing, err := scanMembership(tx.QueryRow(ctx, `
		SELECT id, organization_id, user_id, role, status, invited_by, created_at, updated_at
		FROM organization_memberships
		WHERE organization_id = $1 AND user_id = $2
		FOR UPDATE
	`, m.OrganizationID, m.UserID))
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return s.fail(span, "create_membership", start, err)
	}

	if err == nil {
		if existing.Status != StatusRemoved {
			metrics.RecordDBOperation("create_membership", time.Since(start), ErrConflict)
			span.SetStatus(codes.Ok, "conflict")
			return ErrConflict
		}
		now := time.Now().UTC()
		_, err = tx.Exec(ctx, `
			UPDATE organization_memberships
			SET role = $3, status = $4, invited_by = $5, updated_at = $6
			WHERE organization_id = $1 AND user_id = $2 AND status = 'removed'
		`, m.OrganizationID, m.UserID, m.Role, m.Status, m.InvitedBy, now)
		if err != nil {
			return s.fail(span, "create_membership", start, err)
		}
		if err := tx.Commit(ctx); err != nil {
			return s.fail(span, "create_membership", start, err)
		}
		// Preserve stable id and original created_at from the DB row.
		m.ID = existing.ID
		m.CreatedAt = existing.CreatedAt
		m.UpdatedAt = now
		metrics.RecordDBOperation("create_membership", time.Since(start), nil)
		span.SetAttributes(attribute.String("membership.id", m.ID))
		span.SetStatus(codes.Ok, "reactivated")
		return nil
	}

	_, err = tx.Exec(ctx, `
		INSERT INTO organization_memberships
			(id, organization_id, user_id, role, status, invited_by, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
	`, m.ID, m.OrganizationID, m.UserID, m.Role, m.Status, m.InvitedBy, m.CreatedAt, m.UpdatedAt)
	if err != nil {
		if isUniqueViolation(err) {
			metrics.RecordDBOperation("create_membership", time.Since(start), ErrConflict)
			span.SetStatus(codes.Ok, "conflict")
			return ErrConflict
		}
		return s.fail(span, "create_membership", start, err)
	}

	if err := tx.Commit(ctx); err != nil {
		return s.fail(span, "create_membership", start, err)
	}

	metrics.RecordDBOperation("create_membership", time.Since(start), nil)
	span.SetStatus(codes.Ok, "ok")
	return nil
}

// MutateMembership locks all memberships for the org, loads the target row,
// invokes fn with the active-owner count, and persists role/status on success.
// Errors from fn are returned unchanged (authz/validation/domain).
func (s *Store) MutateMembership(ctx context.Context, organizationID, membershipID string, fn func(m *Membership, activeOwners int) error) (*Membership, error) {
	ctx, span := s.tracer.Start(ctx, "db.mutate_membership")
	defer span.End()
	span.SetAttributes(
		attribute.String("db.system", "postgresql"),
		attribute.String("db.operation.name", "mutate_membership"),
		attribute.String("organization.id", organizationID),
		attribute.String("membership.id", membershipID),
	)

	start := time.Now()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, s.fail(span, "mutate_membership", start, err)
	}
	defer tx.Rollback(ctx)

	rows, err := tx.Query(ctx, `
		SELECT id, organization_id, user_id, role, status, invited_by, created_at, updated_at
		FROM organization_memberships
		WHERE organization_id = $1
		FOR UPDATE
	`, organizationID)
	if err != nil {
		return nil, s.fail(span, "mutate_membership", start, err)
	}

	var target *Membership
	activeOwners := 0
	for rows.Next() {
		m, scanErr := scanMembership(rows)
		if scanErr != nil {
			rows.Close()
			return nil, s.fail(span, "mutate_membership", start, scanErr)
		}
		if m.ID == membershipID {
			target = m
		}
		if m.Role == RoleOwner && m.Status == StatusActive {
			activeOwners++
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, s.fail(span, "mutate_membership", start, err)
	}
	rows.Close()

	if target == nil || target.Status == StatusRemoved {
		metrics.RecordDBOperation("mutate_membership", time.Since(start), nil)
		span.SetStatus(codes.Ok, "not found")
		return nil, ErrNotFound
	}

	if err := fn(target, activeOwners); err != nil {
		// Domain and handler (authz/validation) errors pass through unwrapped.
		metrics.RecordDBOperation("mutate_membership", time.Since(start), nil)
		span.SetStatus(codes.Ok, "rejected")
		return nil, err
	}

	target.UpdatedAt = time.Now().UTC()
	tag, err := tx.Exec(ctx, `
		UPDATE organization_memberships
		SET role = $3, status = $4, updated_at = $5
		WHERE organization_id = $1 AND id = $2
	`, organizationID, membershipID, target.Role, target.Status, target.UpdatedAt)
	if err != nil {
		return nil, s.fail(span, "mutate_membership", start, err)
	}
	if tag.RowsAffected() == 0 {
		metrics.RecordDBOperation("mutate_membership", time.Since(start), nil)
		span.SetStatus(codes.Ok, "not found")
		return nil, ErrNotFound
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, s.fail(span, "mutate_membership", start, err)
	}

	metrics.RecordDBOperation("mutate_membership", time.Since(start), nil)
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

func scanMembership(row scannable) (*Membership, error) {
	var m Membership
	var role, status string
	err := row.Scan(&m.ID, &m.OrganizationID, &m.UserID, &role, &status, &m.InvitedBy, &m.CreatedAt, &m.UpdatedAt)
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
