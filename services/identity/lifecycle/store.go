// Package lifecycle is identity's feed of facts other services may act on
// (docs/concepts/lifecycle.md#lifecycle-events). Events are written in the transaction
// of the change they describe, and read by services that opt in.
package lifecycle

import (
	"context"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/plat5dev/plat5/identity/internal/dbx"
)

// Stored types. A node event is stored as node.<op> with its kind, and goes on
// the wire as <kind>.<op> (project.deleted).
const (
	TypeNodeCreated   = "node.created"
	TypeNodeDeleted   = "node.deleted"
	TypeNodeRestored  = "node.restored"
	TypeNodePurged    = "node.purged"
	TypeMemberCreated = "member.created"
	TypeMemberRemoved = "member.removed"
)

// lockKey serializes event writes. A sequence hands out ids at insert, not at
// commit, so without it a reader could see id 11 before id 10 commits and
// walk past 10.
const lockKey int64 = 0x706c617435_6c63 // "plat5" "lc"

const storeTracerName = "identity.lifecycle.store"

type Event struct {
	ID         int64
	Type       string
	Kind       *string
	NodeID     *string
	MemberID   *string
	RootID     string
	Principal  *string
	OccurredAt time.Time
}

// NodeEvent is a node.<op> event for one node.
func NodeEvent(typ, kind, nodeID, rootID string) Event {
	return Event{Type: typ, Kind: &kind, NodeID: &nodeID, RootID: rootID}
}

// MemberEvent is a member.<op> event. principal is person or service_account.
func MemberEvent(typ, memberID, rootID, principal string) Event {
	return Event{Type: typ, MemberID: &memberID, RootID: rootID, Principal: &principal}
}

// WireType is the type services read: the node's kind in place of "node".
func (e *Event) WireType() string {
	if op, ok := strings.CutPrefix(e.Type, "node."); ok && e.Kind != nil {
		return *e.Kind + "." + op
	}
	return e.Type
}

// Record writes e in tx. Call it after every other write of the change: the
// lock is held until tx ends, and other event writers wait on it.
func Record(ctx context.Context, tx pgx.Tx, e Event) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, lockKey); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO lifecycle_events (type, kind, node_id, member_id, root_id, principal)
		VALUES ($1, $2, $3, $4, $5, $6)
	`, e.Type, e.Kind, e.NodeID, e.MemberID, e.RootID, e.Principal)
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

// List returns events after startingAfter (0 = from the first), id ascending.
func (s *Store) List(ctx context.Context, limit int, startingAfter int64) ([]*Event, bool, error) {
	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "list_lifecycle_events", dbx.DefaultTimeout)
	defer cancel()
	defer op.End()

	rows, err := s.pool.Query(ctx, `
		SELECT id, type, kind, node_id, member_id, root_id, principal, occurred_at
		FROM lifecycle_events
		WHERE id > $1
		ORDER BY id ASC
		LIMIT $2
	`, startingAfter, limit+1)
	if err != nil {
		return nil, false, op.Fail(err)
	}
	defer rows.Close()

	var out []*Event
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.ID, &e.Type, &e.Kind, &e.NodeID, &e.MemberID, &e.RootID, &e.Principal, &e.OccurredAt); err != nil {
			return nil, false, op.Fail(err)
		}
		out = append(out, &e)
	}
	if err := rows.Err(); err != nil {
		return nil, false, op.Fail(err)
	}

	hasMore := len(out) > limit
	if hasMore {
		out = out[:limit]
	}
	op.Attr(attribute.Int("lifecycle_events.count", len(out)))
	op.OK("ok")
	return out, hasMore, nil
}
