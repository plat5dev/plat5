// Package lifecycle is identity's one log of what's created and what ends,
// for other services to act on (docs/concepts/lifecycle.md#lifecycle-events).
// Events are written in the transaction of the change they describe, and read
// by services that opt in, long-polling GET /internal/events
// (docs/reference/api/internal.md#lifecycle-events).
package lifecycle

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/plat5dev/plat5/identity/internal/dbx"
)

// The event types: a fixed set, whatever kinds the deployment declares.
const (
	TypeNodeCreated   = "node.created"
	TypeNodeDeleted   = "node.deleted"
	TypeNodeRestored  = "node.restored"
	TypeNodePurged    = "node.purged"
	TypeMemberCreated = "member.created"
	TypeMemberRemoved = "member.removed"
	TypeUserCreated   = "user.created"
	TypeUserDeleted   = "user.deleted"
)

// Types is every event type, in the docs' order.
var Types = []string{
	TypeNodeCreated, TypeNodeDeleted, TypeNodeRestored, TypeNodePurged,
	TypeMemberCreated, TypeMemberRemoved,
	TypeUserCreated, TypeUserDeleted,
}

// lockKey serializes the log's writes. A sequence hands out ids at insert, not
// at commit, so without it a reader could see id 11 before id 10 commits and
// walk past 10.
const lockKey int64 = 0x706c617435_6c63 // "plat5" "lc"

// Event is one event in the log. A field the event's type doesn't carry is
// nil: node events carry Kind, NodeID and RootID; member events MemberID,
// RootID and Principal; user events only UserID.
type Event struct {
	ID         int64
	Type       string
	Kind       *string
	NodeID     *string
	MemberID   *string
	RootID     *string
	Principal  *string
	UserID     *string
	OccurredAt time.Time
}

// NodeEvent is a node.<op> event for one node.
func NodeEvent(typ, kind, nodeID, rootID string) Event {
	return Event{Type: typ, Kind: &kind, NodeID: &nodeID, RootID: &rootID}
}

// MemberEvent is a member.<op> event. principal is person or service_account.
func MemberEvent(typ, memberID, rootID, principal string) Event {
	return Event{Type: typ, MemberID: &memberID, RootID: &rootID, Principal: &principal}
}

// UserCreated is user.created: a person came to exist.
func UserCreated(userID string) Event {
	return Event{Type: TypeUserCreated, UserID: &userID}
}

// UserDeleted is user.deleted: the person's account was deleted.
func UserDeleted(userID string) Event {
	return Event{Type: TypeUserDeleted, UserID: &userID}
}

// Record writes e in tx. Call it after every other write of the change: it
// takes the log's lock, held until tx ends, and other event writers wait on
// it. That keeps ids in commit order and a transaction's events adjacent.
func Record(ctx context.Context, tx pgx.Tx, e Event) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, lockKey); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO events (type, kind, node_id, member_id, root_id, principal, user_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
	`, e.Type, e.Kind, e.NodeID, e.MemberID, e.RootID, e.Principal, e.UserID)
	return err
}

// Store reads the log. Its reads are long-polled, over and over, per reader,
// so like health checks they stay off traces and metrics.
type Store struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// Head is the newest event's id, 0 when the log is empty. Every event up to it
// has committed: the lock commits ids in order.
func (s *Store) Head(ctx context.Context) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, dbx.DefaultTimeout)
	defer cancel()
	var head int64
	err := s.pool.QueryRow(ctx, `SELECT COALESCE(max(id), 0) FROM events`).Scan(&head)
	return head, err
}

// List returns up to limit events after after (0 = from the first), id
// ascending, of types (nil = every type), and whether more such events follow.
func (s *Store) List(ctx context.Context, after int64, limit int, types []string) ([]*Event, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, dbx.DefaultTimeout)
	defer cancel()
	rows, err := s.pool.Query(ctx, `
		SELECT id, type, kind, node_id, member_id, root_id, principal, user_id, occurred_at
		FROM events
		WHERE id > $1 AND ($2::text[] IS NULL OR type = ANY($2))
		ORDER BY id ASC
		LIMIT $3
	`, after, types, limit+1)
	if err != nil {
		return nil, false, err
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (*Event, error) {
		var e Event
		err := r.Scan(&e.ID, &e.Type, &e.Kind, &e.NodeID, &e.MemberID, &e.RootID, &e.Principal, &e.UserID, &e.OccurredAt)
		return &e, err
	})
	if err != nil {
		return nil, false, err
	}
	if len(out) > limit {
		return out[:limit], true, nil
	}
	return out, false, nil
}
