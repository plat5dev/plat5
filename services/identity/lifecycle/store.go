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
	return RecordBatch(ctx, tx, []Event{e})
}

// RecordBatch writes events in order inside tx, taking the log's lock once and
// pipelining the lock and insert in a single round-trip. Call it right before
// commit so the global lock is held for as little time as possible.
func RecordBatch(ctx context.Context, tx pgx.Tx, events []Event) error {
	_, err := RecordCommit(ctx, tx, events, nil)
	return err
}

// RecordCommit writes lifecycle events and admit invalidations in one
// pipelined batch under the single commit-ordering advisory lock. Taking one
// lock in one place keeps both sequences commit-ordered and prevents lock-order
// deadlocks between the two logs. Returns the highest inserted invalidation id
// (0 when no invalidations were written).
func RecordCommit(ctx context.Context, tx pgx.Tx, events []Event, invalidations []Invalidation) (int64, error) {
	if len(events) == 0 && len(invalidations) == 0 {
		return 0, nil
	}
	batch := &pgx.Batch{}
	execs := 1
	batch.Queue(`SELECT pg_advisory_xact_lock($1)`, lockKey)
	if len(events) == 1 {
		execs++
		e := events[0]
		batch.Queue(`
			INSERT INTO events (type, kind, node_id, member_id, root_id, principal, user_id)
			VALUES ($1, $2, $3, $4, $5, $6, $7)
		`, e.Type, e.Kind, e.NodeID, e.MemberID, e.RootID, e.Principal, e.UserID)
	} else if len(events) > 1 {
		execs++
		n := len(events)
		types := make([]string, n)
		kinds := make([]*string, n)
		nodeIDs := make([]*string, n)
		memberIDs := make([]*string, n)
		rootIDs := make([]*string, n)
		principals := make([]*string, n)
		userIDs := make([]*string, n)
		for i, e := range events {
			types[i] = e.Type
			kinds[i] = e.Kind
			nodeIDs[i] = e.NodeID
			memberIDs[i] = e.MemberID
			rootIDs[i] = e.RootID
			principals[i] = e.Principal
			userIDs[i] = e.UserID
		}
		batch.Queue(`
			INSERT INTO events (type, kind, node_id, member_id, root_id, principal, user_id)
			SELECT u.type, u.kind, u.node_id, u.member_id, u.root_id, u.principal, u.user_id
			FROM unnest($1::text[], $2::text[], $3::text[], $4::text[], $5::text[], $6::text[], $7::text[])
				WITH ORDINALITY AS u (type, kind, node_id, member_id, root_id, principal, user_id, ord)
			ORDER BY u.ord
		`, types, kinds, nodeIDs, memberIDs, rootIDs, principals, userIDs)
	}
	hasInvs := queueInvalidations(batch, invalidations)
	br := tx.SendBatch(ctx, batch)
	for range execs {
		if _, err := br.Exec(); err != nil {
			_ = br.Close()
			return 0, err
		}
	}
	var maxInvID int64
	if hasInvs {
		if err := br.QueryRow().Scan(&maxInvID); err != nil {
			_ = br.Close()
			return 0, err
		}
	}
	return maxInvID, br.Close()
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
