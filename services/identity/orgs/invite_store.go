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

	status := inv.Status
	if status == "" {
		status = InviteStatusActive
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO organization_invites (
			id, organization_id, role, email, token_hash, token_prefix,
			created_by, expires_at, created_at, token, status, max_uses, use_count
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
	`, inv.ID, inv.OrganizationID, inv.Role, inv.Email, inv.TokenHash, inv.TokenPrefix,
		inv.CreatedBy, inv.ExpiresAt, inv.CreatedAt, inv.Token, status, inv.MaxUses, inv.UseCount)
	if err != nil {
		return op.Fail(err)
	}
	op.OK("created")
	return nil
}

func (s *Store) ListInvites(ctx context.Context, organizationID string, limit int, startingAfter string) ([]*Invite, *string, error) {
	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "list_invites", dbx.DefaultTimeout,
		attribute.String("organization.id", organizationID),
	)
	defer cancel()
	defer op.End()

	now := time.Now().UTC()
	_, err := s.pool.Exec(ctx, `
		UPDATE organization_invites
		SET status = 'expired', token = NULL
		WHERE organization_id = $1 AND status = 'active' AND expires_at <= $2
	`, organizationID, now)
	if err != nil {
		return nil, nil, op.Fail(err)
	}

	var after any
	if startingAfter != "" {
		after = startingAfter
	}
	rows, err := s.pool.Query(ctx, `
		SELECT `+inviteSelectCols+`
		FROM organization_invites
		WHERE organization_id = $1
		AND ($2::text IS NULL OR id > $2)
		ORDER BY id ASC
		LIMIT $3
	`, organizationID, after, limit+1)
	if err != nil {
		return nil, nil, op.Fail(err)
	}
	defer rows.Close()

	var out []*Invite
	for rows.Next() {
		inv, err := scanInvite(rows)
		if err != nil {
			return nil, nil, op.Fail(err)
		}
		out = append(out, inv)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, op.Fail(err)
	}

	var last *string
	if len(out) > limit {
		out = out[:limit]
		n := out[len(out)-1].ID
		last = &n
	}
	op.Attr(attribute.Int("invites.count", len(out)))
	op.OK("ok")
	return out, last, nil
}

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
		SET
			status = CASE
				WHEN status = 'active' AND expires_at <= $1 THEN 'expired'
				WHEN status = 'active' THEN 'revoked'
				ELSE status
			END,
			token = CASE
				WHEN status = 'active' THEN NULL
				ELSE token
			END
		WHERE id = $2 AND organization_id = $3
		RETURNING `+inviteSelectCols+`
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
		SELECT `+inviteSelectCols+`
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
	if inv.Status == InviteStatusActive && !now.Before(inv.ExpiresAt) {
		_, err = tx.Exec(ctx, `
			UPDATE organization_invites
			SET status = 'expired', token = NULL
			WHERE id = $1 AND status = 'active'
		`, inv.ID)
		if err != nil {
			return nil, op.Fail(err)
		}
		if err := tx.Commit(ctx); err != nil {
			return nil, op.Fail(err)
		}
		return nil, op.Expected("expired", &InviteDeadError{Status: InviteStatusExpired})
	}
	if inv.Status != InviteStatusActive {
		return nil, op.Expected("dead", &InviteDeadError{Status: inv.Status})
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

	next := inv.UseCount + 1
	spent := inv.MaxUses != nil && next >= *inv.MaxUses
	if spent {
		_, err = tx.Exec(ctx, `
			UPDATE organization_invites
			SET use_count = $2, status = 'redeemed', token = NULL
			WHERE id = $1 AND status = 'active'
		`, inv.ID, next)
	} else {
		_, err = tx.Exec(ctx, `
			UPDATE organization_invites
			SET use_count = $2
			WHERE id = $1 AND status = 'active'
		`, inv.ID, next)
	}
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
