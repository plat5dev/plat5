// Package lifecycle is identity's two feeds of facts other services may act on
// (docs/concepts/lifecycle.md#lifecycle-events): tree events for tree data,
// user events for user-route data, each with its own sequence. Events are
// written in the transaction of the change they describe, and read by
// services that opt in.
package lifecycle

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/plat5dev/plat5/identity/internal/dbx"
)

// The tree event types: a fixed set, whatever kinds the deployment declares.
// Node events carry the node's kind.
const (
	TypeNodeCreated   = "node.created"
	TypeNodeDeleted   = "node.deleted"
	TypeNodeRestored  = "node.restored"
	TypeNodePurged    = "node.purged"
	TypeMemberCreated = "member.created"
	TypeMemberRemoved = "member.removed"
)

// The user event types.
const (
	TypeUserCreated = "user.created"
	TypeUserDeleted = "user.deleted"
)

// treeLockKey and userLockKey serialize each feed's writes. A sequence hands
// out ids at insert, not at commit, so without them a reader could see id 11
// before id 10 commits and walk past 10. A transaction that writes both takes
// treeLockKey first.
const (
	treeLockKey int64 = 0x706c617435_6c63 // "plat5" "lc"
	userLockKey int64 = 0x706c617435_7573 // "plat5" "us"
)

const storeTracerName = "identity.lifecycle.store"

// TreeEvent is a node or member event. It never carries a user id.
type TreeEvent struct {
	ID         int64
	Type       string
	Kind       *string
	NodeID     *string
	MemberID   *string
	RootID     string
	Principal  *string
	OccurredAt time.Time
}

// UserEvent is a person's event: only their user id.
type UserEvent struct {
	ID         int64
	Type       string
	UserID     string
	OccurredAt time.Time
}

// NodeEvent is a node.<op> event for one node.
func NodeEvent(typ, kind, nodeID, rootID string) TreeEvent {
	return TreeEvent{Type: typ, Kind: &kind, NodeID: &nodeID, RootID: rootID}
}

// MemberEvent is a member.<op> event. principal is person or service_account.
func MemberEvent(typ, memberID, rootID, principal string) TreeEvent {
	return TreeEvent{Type: typ, MemberID: &memberID, RootID: rootID, Principal: &principal}
}

// UserCreated is user.created: a person came to exist.
func UserCreated(userID string) UserEvent {
	return UserEvent{Type: TypeUserCreated, UserID: userID}
}

// UserDeleted is user.deleted: the person's account was deleted.
func UserDeleted(userID string) UserEvent {
	return UserEvent{Type: TypeUserDeleted, UserID: userID}
}

// RecordTree writes e in tx. Call it after every other write of the change:
// the lock is held until tx ends, and other tree event writers wait on it.
func RecordTree(ctx context.Context, tx pgx.Tx, e TreeEvent) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, treeLockKey); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO tree_events (type, kind, node_id, member_id, root_id, principal)
		VALUES ($1, $2, $3, $4, $5, $6)
	`, e.Type, e.Kind, e.NodeID, e.MemberID, e.RootID, e.Principal)
	return err
}

// RecordUser writes e in tx. Call it last, after any tree events: the lock is
// held until tx ends, and other user event writers wait on it.
func RecordUser(ctx context.Context, tx pgx.Tx, e UserEvent) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, userLockKey); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `INSERT INTO user_events (type, user_id) VALUES ($1, $2)`, e.Type, e.UserID)
	return err
}

type Store struct {
	pool   *pgxpool.Pool
	tracer trace.Tracer
}

func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{
		pool:   pool,
		tracer: otel.Tracer(storeTracerName),
	}
}

// ListTree returns tree events after startingAfter (0 = from the first), id
// ascending.
func (s *Store) ListTree(ctx context.Context, limit int, startingAfter int64) ([]*TreeEvent, bool, error) {
	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "list_tree_events", dbx.DefaultTimeout)
	defer cancel()
	defer op.End()

	rows, err := s.pool.Query(ctx, `
		SELECT id, type, kind, node_id, member_id, root_id, principal, occurred_at
		FROM tree_events
		WHERE id > $1
		ORDER BY id ASC
		LIMIT $2
	`, startingAfter, limit+1)
	if err != nil {
		return nil, false, op.Fail(err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (*TreeEvent, error) {
		var e TreeEvent
		err := r.Scan(&e.ID, &e.Type, &e.Kind, &e.NodeID, &e.MemberID, &e.RootID, &e.Principal, &e.OccurredAt)
		return &e, err
	})
	if err != nil {
		return nil, false, op.Fail(err)
	}
	out, hasMore := page(out, limit)
	op.Attr(attribute.Int("tree_events.count", len(out)))
	op.OK("ok")
	return out, hasMore, nil
}

// ListUsers returns user events after startingAfter (0 = from the first), id
// ascending.
func (s *Store) ListUsers(ctx context.Context, limit int, startingAfter int64) ([]*UserEvent, bool, error) {
	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "list_user_events", dbx.DefaultTimeout)
	defer cancel()
	defer op.End()

	rows, err := s.pool.Query(ctx, `
		SELECT id, type, user_id, occurred_at
		FROM user_events
		WHERE id > $1
		ORDER BY id ASC
		LIMIT $2
	`, startingAfter, limit+1)
	if err != nil {
		return nil, false, op.Fail(err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (*UserEvent, error) {
		var e UserEvent
		err := r.Scan(&e.ID, &e.Type, &e.UserID, &e.OccurredAt)
		return &e, err
	})
	if err != nil {
		return nil, false, op.Fail(err)
	}
	out, hasMore := page(out, limit)
	op.Attr(attribute.Int("user_events.count", len(out)))
	op.OK("ok")
	return out, hasMore, nil
}

// page trims a limit+1 read to limit and says whether there was more.
func page[T any](items []T, limit int) ([]T, bool) {
	if len(items) > limit {
		return items[:limit], true
	}
	return items, false
}
