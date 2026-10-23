package members

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/plat5dev/plat5/identity/internal/dbx"
	"github.com/plat5dev/plat5/identity/nodes"
)

const expireBatch = 100

// ExpireDue deletes bindings past expires_at, one tree at a time, removing
// each person's member left with no bindings, then marks invites past their
// expiry expired. It returns how many bindings it deleted.
func (s *Store) ExpireDue(ctx context.Context) (int, error) {
	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "expire_due", 4*dbx.DefaultTimeout)
	defer cancel()
	defer op.End()

	total := 0
	for {
		now := s.now().UTC()
		rows, err := s.pool.Query(ctx, `
			SELECT DISTINCT m.root_id FROM bindings b JOIN members m ON m.id = b.member_id
			WHERE b.expires_at <= $1 LIMIT $2
		`, now, expireBatch)
		if err != nil {
			return total, op.Fail(err)
		}
		roots, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return total, op.Fail(err)
		}
		for _, root := range roots {
			n, err := s.expireTree(ctx, root)
			if err != nil {
				return total, op.Fail(err)
			}
			total += n
		}
		if len(roots) < expireBatch {
			break
		}
	}
	if _, err := s.pool.Exec(ctx, `
		UPDATE invites SET status = 'expired' WHERE status = 'active' AND expires_at <= $1
	`, s.now().UTC()); err != nil {
		return total, op.Fail(err)
	}
	op.OK("ok")
	return total, nil
}

func (s *Store) expireTree(ctx context.Context, rootID string) (int, error) {
	w, err := s.begin(ctx)
	if err != nil {
		return 0, err
	}
	defer w.rollback(ctx)
	if err := nodes.LockTrees(ctx, w.tx, []string{rootID}); err != nil {
		return 0, err
	}
	rows, err := w.tx.Query(ctx, `
		DELETE FROM bindings b USING members m
		WHERE m.id = b.member_id AND m.root_id = $1 AND b.expires_at <= $2
		RETURNING b.member_id
	`, rootID, s.now().UTC())
	if err != nil {
		return 0, err
	}
	touched, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return 0, err
	}
	if err := s.removeEmptyPeople(ctx, w, touched); err != nil {
		return 0, err
	}
	return len(touched), w.commit(ctx)
}
