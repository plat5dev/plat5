package orgs

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel/attribute"

	"github.com/plat5dev/plat5/identity/errors"
	"github.com/plat5dev/plat5/identity/internal/dbx"
	"github.com/plat5dev/plat5/identity/roles"
)

// DeleteAccount deletes a person, in one transaction (docs/identity.md#account-deletion):
// every non-removed member of theirs is removed as a member remove does, then
// their directory membership (and with it their user keys), IdP links, and the
// person. Removed members stay, with no display name. Refused while the person
// is the last non-removed member, or the last creator_role holder, of an org.
func (s *Store) DeleteAccount(ctx context.Context, userID string, set *roles.Set) error {
	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "delete_account", dbx.DefaultTimeout,
		attribute.String("user.id", userID),
	)
	defer cancel()
	defer op.End()

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return op.Fail(err)
	}
	defer tx.Rollback(ctx)

	var found string
	err = tx.QueryRow(ctx, `
		SELECT id FROM identities WHERE id = $1 AND kind = 'user' FOR UPDATE
	`, userID).Scan(&found)
	if err != nil {
		if dbx.IsNoRows(err) {
			return op.Expected("not found", ErrUserNotFound)
		}
		return op.Fail(err)
	}

	rows, err := tx.Query(ctx, `
		SELECT DISTINCT organization_id
		FROM memberships
		WHERE identity_id = $1 AND organization_id IS NOT NULL AND status <> 'removed'
		ORDER BY organization_id
	`, userID)
	if err != nil {
		return op.Fail(err)
	}
	var orgIDs []string
	for rows.Next() {
		var orgID string
		if err := rows.Scan(&orgID); err != nil {
			rows.Close()
			return op.Fail(err)
		}
		orgIDs = append(orgIDs, orgID)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return op.Fail(err)
	}

	// Lock each org's members in id order, as every member write does.
	var targets []*Member
	var blocked []string
	for _, orgID := range orgIDs {
		members, err := lockOrgMembers(ctx, tx, orgID)
		if err != nil {
			return op.Fail(err)
		}
		var target *Member
		for _, m := range members {
			if m.UserID != nil && *m.UserID == userID && m.Status != StatusRemoved {
				target = m
				break
			}
		}
		if target == nil {
			continue
		}
		if blocksDeletion(set, members, target) {
			blocked = append(blocked, orgID)
			continue
		}
		targets = append(targets, target)
	}
	if len(blocked) > 0 {
		refs, err := orgRefs(ctx, tx, blocked)
		if err != nil {
			return op.Fail(err)
		}
		return op.Expected("handoff required", errors.HandoffRequiredError(refs))
	}

	now := time.Now().UTC()
	for _, m := range targets {
		m.Status = StatusRemoved
		m.UpdatedAt = now
		if _, err := tx.Exec(ctx, `
			UPDATE memberships SET status = 'removed', updated_at = $2 WHERE id = $1
		`, m.ID, now); err != nil {
			return op.Fail(err)
		}
		if err := finishRemove(ctx, tx, m); err != nil {
			return op.Fail(err)
		}
	}

	// The person's name goes with them, including any org's override.
	if _, err := tx.Exec(ctx, `
		UPDATE memberships SET display_name = NULL WHERE identity_id = $1 AND organization_id IS NOT NULL
	`, userID); err != nil {
		return op.Fail(err)
	}
	// The directory membership cascades to the person's user keys. Deleting
	// the person cascades to their IdP links and clears identity_id on their
	// removed members.
	if _, err := tx.Exec(ctx, `
		DELETE FROM memberships WHERE id = $1 AND organization_id IS NULL
	`, userID); err != nil {
		return op.Fail(err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM identities WHERE id = $1`, userID); err != nil {
		return op.Fail(err)
	}

	if err := tx.Commit(ctx); err != nil {
		return op.Fail(err)
	}
	op.Attr(attribute.Int("members.removed", len(targets)))
	op.OK("deleted")
	return nil
}

// blocksDeletion is true when removing target would leave its org with no
// non-removed member, or with no creator_role holder.
func blocksDeletion(set *roles.Set, members []*Member, target *Member) bool {
	if countNonRemoved(members) <= 1 {
		return true
	}
	creator := set.Creator()
	return creator != nil && holdsRole(target, *creator) && countHolders(members, *creator) <= 1
}

func orgRefs(ctx context.Context, tx pgx.Tx, ids []string) ([]errors.OrgRef, error) {
	rows, err := tx.Query(ctx, `
		SELECT id, name FROM organizations WHERE id = ANY($1) ORDER BY id
	`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]errors.OrgRef, 0, len(ids))
	for rows.Next() {
		var ref errors.OrgRef
		if err := rows.Scan(&ref.ID, &ref.Name); err != nil {
			return nil, err
		}
		out = append(out, ref)
	}
	return out, rows.Err()
}
