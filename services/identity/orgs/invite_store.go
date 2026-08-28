package orgs

import (
	"context"
	stderrors "errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

func (s *Store) CreateInvite(ctx context.Context, inv *Invite) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO organization_invites (
			id, organization_id, role, email, token, token_hash, token_prefix,
			status, max_uses, use_count, created_by, expires_at, created_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`,
		inv.ID, inv.OrganizationID, inv.Role, inv.Email, inv.Token, inv.TokenHash, inv.TokenPrefix,
		inv.Status, inv.MaxUses, inv.UseCount, inv.CreatedBy, inv.ExpiresAt, inv.CreatedAt,
	)
	return err
}

func (s *Store) ListInvites(ctx context.Context, organizationID string, limit, offset int) ([]*Invite, bool, error) {
	_, err := s.pool.Exec(ctx, `
		UPDATE organization_invites
		SET status = 'expired', token = NULL
		WHERE organization_id = $1 AND status = 'active' AND expires_at <= now()`,
		organizationID,
	)
	if err != nil {
		return nil, false, err
	}

	rows, err := s.pool.Query(ctx, `
		SELECT `+inviteSelectCols+`
		FROM organization_invites
		WHERE organization_id = $1
		ORDER BY created_at DESC
		LIMIT $2 OFFSET $3`,
		organizationID, limit+1, offset,
	)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()

	var invites []*Invite
	for rows.Next() {
		inv, err := scanInvite(rows)
		if err != nil {
			return nil, false, err
		}
		invites = append(invites, inv)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}

	hasMore := len(invites) > limit
	if hasMore {
		invites = invites[:limit]
	}
	return invites, hasMore, nil
}

func (s *Store) RevokeInvite(ctx context.Context, organizationID, inviteID string) (*Invite, error) {
	var inv Invite
	err := s.pool.QueryRow(ctx, `
		UPDATE organization_invites
		SET status = CASE
			WHEN status = 'active' AND expires_at <= now() THEN 'expired'
			WHEN status = 'active' THEN 'revoked'
			ELSE status
		END,
		token = CASE WHEN status = 'active' THEN NULL ELSE token END,
		revoked_at = CASE
			WHEN status = 'active' AND expires_at > now() THEN now()
			ELSE revoked_at
		END
		WHERE id = $1 AND organization_id = $2 AND revoked_at IS NULL AND redeemed_at IS NULL
		RETURNING `+inviteSelectCols,
		inviteID, organizationID,
	).Scan(
		&inv.ID, &inv.OrganizationID, &inv.Role, &inv.Email, &inv.TokenHash, &inv.TokenPrefix,
		&inv.CreatedBy, &inv.ExpiresAt, &inv.CreatedAt, &inv.RevokedAt, &inv.RedeemedAt, &inv.RedeemedBy,
		&inv.Token, &inv.Status, &inv.MaxUses, &inv.UseCount,
	)
	if stderrors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &inv, nil
}

func (s *Store) RedeemInvite(ctx context.Context, tokenHash, userID string) (*Member, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	var inv Invite
	err = tx.QueryRow(ctx, `
		SELECT `+inviteSelectCols+`
		FROM organization_invites
		WHERE token_hash = $1
		FOR UPDATE`,
		tokenHash,
	).Scan(
		&inv.ID, &inv.OrganizationID, &inv.Role, &inv.Email, &inv.TokenHash, &inv.TokenPrefix,
		&inv.CreatedBy, &inv.ExpiresAt, &inv.CreatedAt, &inv.RevokedAt, &inv.RedeemedAt, &inv.RedeemedBy,
		&inv.Token, &inv.Status, &inv.MaxUses, &inv.UseCount,
	)
	if stderrors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	if inv.Status == InviteStatusActive && !inv.ExpiresAt.After(now) {
		_, err = tx.Exec(ctx, `
			UPDATE organization_invites
			SET status = 'expired', token = NULL
			WHERE id = $1`, inv.ID)
		if err != nil {
			return nil, err
		}
		if err := tx.Commit(ctx); err != nil {
			return nil, err
		}
		return nil, &InviteDeadError{Status: InviteStatusExpired}
	}
	if inv.Status != InviteStatusActive {
		return nil, &InviteDeadError{Status: inv.Status}
	}

	var existing Member
	err = tx.QueryRow(ctx, `
		SELECT id, organization_id, user_id, role, status, created_by, created_at, updated_at
		FROM organization_members
		WHERE organization_id = $1 AND user_id = $2`,
		inv.OrganizationID, userID,
	).Scan(
		&existing.ID, &existing.OrganizationID, &existing.UserID, &existing.Role,
		&existing.Status, &existing.CreatedBy, &existing.CreatedAt, &existing.UpdatedAt,
	)
	switch {
	case err == nil:
		if existing.Status == MemberStatusRemoved {
			return nil, errors.ForbiddenError("You cannot rejoin this organization. Ask an admin to restore your membership.")
		}
		if err := tx.Commit(ctx); err != nil {
			return nil, err
		}
		return &existing, nil
	case !stderrors.Is(err, pgx.ErrNoRows):
		return nil, err
	}

	member := &Member{
		ID:             NewULID(),
		OrganizationID: inv.OrganizationID,
		UserID:         userID,
		Role:           inv.Role,
		Status:         MemberStatusActive,
		CreatedBy:      inv.CreatedBy,
		CreatedAt:      now,
		UpdatedAt:      now,
	}

	_, err = tx.Exec(ctx, `
		INSERT INTO organization_members (
			id, organization_id, user_id, role, status, created_by, created_at, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		member.ID, member.OrganizationID, member.UserID, member.Role,
		member.Status, member.CreatedBy, member.CreatedAt, member.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}

	newCount := inv.UseCount + 1
	spent := inv.MaxUses != nil && newCount >= *inv.MaxUses
	newStatus := InviteStatusActive
	var token any
	if inv.Token != nil {
		token = *inv.Token
	}
	var redeemedAt any
	var redeemedBy any
	if spent {
		newStatus = InviteStatusRedeemed
		token = nil
		redeemedAt = now
		redeemedBy = userID
	}

	_, err = tx.Exec(ctx, `
		UPDATE organization_invites
		SET use_count = $2, status = $3, token = $4, redeemed_at = $5, redeemed_by = $6
		WHERE id = $1`,
		inv.ID, newCount, newStatus, token, redeemedAt, redeemedBy,
	)
	if err != nil {
		return nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return member, nil
}
