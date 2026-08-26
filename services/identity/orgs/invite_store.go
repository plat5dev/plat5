package orgs

import (
	"context"
	"time"

	"go.opentelemetry.io/otel/attribute"

	"github.com/plat5dev/plat5/identity/internal/dbx"
)

func (s *Store) CreateInvite(ctx context.Context, inv *Invite) error {
	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "create_invite", dbx.DefaultTimeout,
		attribute.String("organization.id", inv.OrganizationID),
		attribute.String("invite.id", inv.ID),
	)
	defer cancel()
	defer op.End()

	_, err := s.pool.Exec(ctx, `
		INSERT INTO organization_invites (
			id, organization_id, role, email, token_hash, token_prefix,
			created_by, expires_at, created_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
	`, inv.ID, inv.OrganizationID, inv.Role, inv.Email, inv.TokenHash, inv.TokenPrefix,
		inv.CreatedBy, inv.ExpiresAt, inv.CreatedAt)
	if err != nil {
		if dbx.IsUniqueViolation(err) {
			return op.Fail(err)
		}
		return op.Fail(err)
	}
	op.OK("created")
	return nil
}

func (s *Store) ListInvites(ctx context.Context, organizationID string, limit, offset int) ([]*Invite, bool, error) {
	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "list_invites", dbx.DefaultTimeout,
		attribute.String("organization.id", organizationID),
	)
	defer cancel()
	defer op.End()

	rows, err := s.pool.Query(ctx, `
		SELECT id, organization_id, role, email, token_hash, token_prefix,
			created_by, expires_at, redeemed_at, redeemed_by, revoked_at, created_at
		FROM organization_invites
		WHERE organization_id = $1
		ORDER BY created_at DESC
		LIMIT $2 OFFSET $3
	`, organizationID, limit+1, offset)
	if err != nil {
		return nil, false, op.Fail(err)
	}
	defer rows.Close()

	var out []*Invite
	for rows.Next() {
		inv, err := scanInvite(rows)
		if err != nil {
			return nil, false, op.Fail(err)
		}
		out = append(out, inv)
	}
	if err := rows.Err(); err != nil {
		return nil, false, op.Fail(err)
	}

	hasMore := len(out) > limit
	if hasMore {
		out = out[:limit]
	}
	op.Attr(attribute.Int("invites.count", len(out)))
	op.OK("ok")
	return out, hasMore, nil
}

// RevokeInvite soft-revokes idempotently (COALESCE keeps first revoked_at).
func (s *Store) RevokeInvite(ctx context.Context, organizationID, inviteID string) (*Invite, error) {
	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "revoke_invite", dbx.DefaultTimeout,
		attribute.String("organization.id", organizationID),
		attribute.String("invite.id", inviteID),
	)
	defer cancel()
	defer op.End()

	now := time.Now().UTC()
	inv, err := scanInvite(s.pool.QueryRow(ctx, `
		UPDATE organization_invites
		SET revoked_at = COALESCE(revoked_at, $1)
		WHERE id = $2 AND organization_id = $3
		RETURNING id, organization_id, role, email, token_hash, token_prefix,
			created_by, expires_at, redeemed_at, redeemed_by, revoked_at, created_at
	`, now, inviteID, organizationID))
	if err != nil {
		if dbx.IsNoRows(err) {
			return nil, op.Expected("not found", ErrNotFound)
		}
		return nil, op.Fail(err)
	}
	op.OK("revoked")
	return inv, nil
}

// RedeemInvite consumes a valid invite and inserts (or reactivates) an active
// user member. Already a member (non-removed) is idempotent success and still
// consumes the token. Unknown / expired / revoked / used → ErrNotFound.
func (s *Store) RedeemInvite(ctx context.Context, tokenHash, userID string) (*Member, error) {
	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "redeem_invite", dbx.DefaultTimeout,
		attribute.String("user.id", userID),
	)
	defer cancel()
	defer op.End()

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, op.Fail(err)
	}
	defer tx.Rollback(ctx)

	inv, err := scanInvite(tx.QueryRow(ctx, `
		SELECT id, organization_id, role, email, token_hash, token_prefix,
			created_by, expires_at, redeemed_at, redeemed_by, revoked_at, created_at
		FROM organization_invites
		WHERE token_hash = $1
		FOR UPDATE
	`, tokenHash))
	if err != nil {
		if dbx.IsNoRows(err) {
			return nil, op.Expected("not found", ErrNotFound)
		}
		return nil, op.Fail(err)
	}

	now := time.Now().UTC()
	if !InviteRedeemable(inv, now) {
		return nil, op.Expected("not found", ErrNotFound)
	}

	existing, err := scanMember(tx.QueryRow(ctx, `
		SELECT id, organization_id, user_id, service_account_id, role, status, added_by, created_at, updated_at
		FROM members
		WHERE organization_id = $1 AND user_id = $2
		FOR UPDATE
	`, inv.OrganizationID, userID))
	if err != nil && !dbx.IsNoRows(err) {
		return nil, op.Fail(err)
	}

	var member *Member
	if err == nil && existing.Status != StatusRemoved {
		member = existing
	} else if err == nil {
		_, err = tx.Exec(ctx, `
			UPDATE members
			SET role = $3, status = $4, added_by = $5, updated_at = $6
			WHERE organization_id = $1 AND user_id = $2 AND status = 'removed'
		`, inv.OrganizationID, userID, inv.Role, StatusActive, inv.CreatedBy, now)
		if err != nil {
			return nil, op.Fail(err)
		}
		existing.Role = inv.Role
		existing.Status = StatusActive
		existing.AddedBy = &inv.CreatedBy
		existing.UpdatedAt = now
		member = existing
	} else {
		m := &Member{
			ID:             NewULID(),
			OrganizationID: inv.OrganizationID,
			UserID:         &userID,
			Role:           inv.Role,
			Status:         StatusActive,
			AddedBy:        &inv.CreatedBy,
			CreatedAt:      now,
			UpdatedAt:      now,
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO members
				(id, organization_id, user_id, service_account_id, role, status, added_by, created_at, updated_at)
			VALUES ($1, $2, $3, NULL, $4, $5, $6, $7, $8)
		`, m.ID, m.OrganizationID, userID, m.Role, m.Status, m.AddedBy, m.CreatedAt, m.UpdatedAt)
		if err != nil {
			if dbx.IsUniqueViolation(err) {
				return nil, op.SoftFail("conflict", ErrConflict, ErrConflict)
			}
			return nil, op.Fail(err)
		}
		member = m
	}

	_, err = tx.Exec(ctx, `
		UPDATE organization_invites
		SET redeemed_at = $2, redeemed_by = $3
		WHERE id = $1 AND redeemed_at IS NULL AND revoked_at IS NULL
	`, inv.ID, now, userID)
	if err != nil {
		return nil, op.Fail(err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, op.Fail(err)
	}
	op.Attr(
		attribute.String("invite.id", inv.ID),
		attribute.String("organization.id", inv.OrganizationID),
		attribute.String("member.id", member.ID),
	)
	op.OK("redeemed")
	return member, nil
}
