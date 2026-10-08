package orgs

import (
	"context"
	stderrors "errors"
	"time"

	"go.opentelemetry.io/otel/attribute"

	"github.com/plat5dev/plat5/identity/internal/dbx"
)

// CreateOrganization inserts the org and its creator as an active member with role.
func (s *Store) CreateOrganization(ctx context.Context, org *Organization, userID string, role *string) (*Member, error) {
	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "create_organization", dbx.DefaultTimeout,
		attribute.String("organization.id", org.ID),
	)
	defer cancel()
	defer op.End()

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, op.Fail(err)
	}
	defer tx.Rollback(ctx)

	_, err = tx.Exec(ctx, `
		INSERT INTO organizations (id, name, slug, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5)
	`, org.ID, org.Name, org.Slug, org.CreatedAt, org.UpdatedAt)
	if err != nil {
		if dbx.IsUniqueViolation(err) {
			return nil, op.SoftFail("slug conflict", ErrConflict, ErrConflict)
		}
		return nil, op.Fail(err)
	}

	m := &Member{
		ID:             NewULID(),
		OrganizationID: org.ID,
		UserID:         &userID,
		Role:           role,
		Status:         StatusActive,
		CreatedAt:      org.CreatedAt,
		UpdatedAt:      org.UpdatedAt,
	}

	_, err = tx.Exec(ctx, `
		INSERT INTO members
			(id, organization_id, user_id, service_account_id, role, status, created_at, updated_at)
		VALUES ($1, $2, $3, NULL, $4, $5, $6, $7)
	`, m.ID, m.OrganizationID, userID, m.Role, m.Status, m.CreatedAt, m.UpdatedAt)
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
	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "get_organization", dbx.DefaultTimeout,
		attribute.String("organization.id", organizationID),
	)
	defer cancel()
	defer op.End()

	org, err := scanOrg(s.pool.QueryRow(ctx, `
		SELECT id, name, slug, created_at, updated_at
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

func (s *Store) OrganizationExists(ctx context.Context, organizationID string) (bool, error) {
	_, err := s.GetOrganization(ctx, organizationID)
	if err == nil {
		return true, nil
	}
	if stderrors.Is(err, ErrNotFound) {
		return false, nil
	}
	return false, err
}

func (s *Store) ListOrganizations(ctx context.Context, limit int, startingAfter string) ([]*Organization, bool, error) {
	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "list_organizations", dbx.DefaultTimeout)
	defer cancel()
	defer op.End()

	var after any
	if startingAfter != "" {
		after = startingAfter
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, name, slug, created_at, updated_at
		FROM organizations
		WHERE ($1::text IS NULL OR id > $1)
		ORDER BY id ASC
		LIMIT $2
	`, after, limit+1)
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

// UpdateOrganization sets name and slug where non-nil. It also returns the
// organization as it was, read under the same lock.
func (s *Store) UpdateOrganization(ctx context.Context, organizationID string, name, slug *string) (*Organization, Organization, error) {
	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "update_organization", dbx.DefaultTimeout,
		attribute.String("organization.id", organizationID),
	)
	defer cancel()
	defer op.End()

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, Organization{}, op.Fail(err)
	}
	defer tx.Rollback(ctx)

	org, err := scanOrg(tx.QueryRow(ctx, `
		SELECT id, name, slug, created_at, updated_at
		FROM organizations WHERE id = $1
		FOR UPDATE
	`, organizationID))
	if err != nil {
		if dbx.IsNoRows(err) {
			return nil, Organization{}, op.Expected("not found", ErrNotFound)
		}
		return nil, Organization{}, op.Fail(err)
	}

	prior := *org
	if name != nil {
		org.Name = *name
	}
	if slug != nil {
		org.Slug = *slug
	}
	org.UpdatedAt = time.Now().UTC()
	_, err = tx.Exec(ctx, `
		UPDATE organizations
		SET name = $2, slug = $3, updated_at = $4
		WHERE id = $1
	`, org.ID, org.Name, org.Slug, org.UpdatedAt)
	if err != nil {
		if dbx.IsUniqueViolation(err) {
			return nil, Organization{}, op.SoftFail("slug conflict", ErrConflict, ErrConflict)
		}
		return nil, Organization{}, op.Fail(err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, Organization{}, op.Fail(err)
	}
	op.OK("ok")
	return org, prior, nil
}

func (s *Store) DeleteOrganization(ctx context.Context, organizationID string) error {
	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "delete_organization", dbx.DefaultTimeout,
		attribute.String("organization.id", organizationID),
	)
	defer cancel()
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
