// Package members is who can reach the tree: people and their IdP links,
// members of each tree, bindings, service accounts, API keys and invites
// (docs/concepts/access.md, docs/concepts/lifecycle.md#members,
// docs/internals/identity-schema.md).
package members

import (
	"context"
	stderrors "errors"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"

	"github.com/plat5dev/plat5/identity/access"
	"github.com/plat5dev/plat5/identity/errors"
	"github.com/plat5dev/plat5/identity/feed"
	"github.com/plat5dev/plat5/identity/internal/dbx"
	"github.com/plat5dev/plat5/identity/internal/id"
	"github.com/plat5dev/plat5/identity/lifecycle"
	"github.com/plat5dev/plat5/identity/nodes"
)

const (
	PrincipalPerson         = "person"
	PrincipalServiceAccount = "service_account"
	PrincipalGroup          = "group"

	StatusActive    = "active"
	StatusSuspended = "suspended"
	StatusRemoved   = "removed"

	storeTracerName = "identity.members.store"
	// personLockSpace keys the lock on one IdP subject while its person is
	// created. Taken after any tree lock.
	personLockSpace int32 = 0x706c6170 // "plap"
)

var (
	ErrNotFound = stderrors.New("not found")
	// ErrBadCaller is a caller header (X-Plat5-Member-Id or X-Plat5-User-Id)
	// naming no active member of the tree, or no active user: a malformed
	// request.
	ErrBadCaller = stderrors.New("the caller header names no active member or user")
	// errPrincipalGone is a member insert whose principal was deleted while
	// the insert waited on it: an account deletion committed first.
	errPrincipalGone = stderrors.New("the principal was deleted")
)

func errBuiltInRBACDisabled() error {
	return errors.FieldError("authorizer", "Built-in RBAC is disabled when an external authorizer is configured.")
}

// Store writes everything about who can reach a node. Writes that touch a
// tree's members or bindings hold that tree's lock (nodes.LockTrees), so "one
// active member per principal per tree" and "removed when left with no
// bindings" can't race.
type Store struct {
	pool         *pgxpool.Pool
	nodes        *nodes.Store
	brand        string
	pdp          access.PDP
	tracer       trace.Tracer
	now          func() time.Time
	onInvalidate func(int64)
}

// NewStore needs SECRET_BRAND for the keys and invite tokens it mints and reads.
// It also wires PurgeNodes into nodeStore so members owns cleanup of its own tables.
func NewStore(pool *pgxpool.Pool, nodeStore *nodes.Store, brand string) *Store {
	s := &Store{
		pool:   pool,
		nodes:  nodeStore,
		brand:  brand,
		pdp:    access.NewHTTPPDP(),
		tracer: otel.Tracer(storeTracerName),
		now:    time.Now,
	}
	if nodeStore != nil {
		nodeStore.WithPurgeHook(s.PurgeNodes)
	}
	return s
}

// WithPDP returns a copy of the store using a custom AuthZEN PDP client.
func (s *Store) WithPDP(pdp access.PDP) *Store {
	c := *s
	c.pdp = pdp
	return &c
}

// WithInvalidationNotifier registers a callback invoked after a transaction
// commits admit invalidations with the highest committed invalidation id.
func (s *Store) WithInvalidationNotifier(fn func(int64)) {
	s.onInvalidate = fn
}

// WithClock is the store with a fixed notion of now, for tests of expiry. The
// node store it creates nodes with gets the same clock.
func (s *Store) WithClock(now func() time.Time) *Store {
	c := *s
	c.now = now
	c.nodes = s.nodes.WithClock(now)
	c.nodes.WithPurgeHook(c.PurgeNodes)
	return &c
}

// PurgeNodes cleans up all member/RBAC rows hanging off purged nodes inside
// nodes.Store's purgeTree transaction, returning member.removed lifecycle events.
func (s *Store) PurgeNodes(ctx context.Context, tx pgx.Tx, rootID string, purgedIDs []string, rootPurged bool, now time.Time) ([]lifecycle.Event, error) {
	type removal struct{ memberID, principal string }
	var removed []removal
	removeMembers := func(sql string, args ...any) error {
		rows, err := tx.Query(ctx, sql, args...)
		if err != nil {
			return err
		}
		got, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (removal, error) {
			var m removal
			err := r.Scan(&m.memberID, &m.principal)
			return m, err
		})
		if err != nil {
			return err
		}
		removed = append(removed, got...)
		return nil
	}

	// Service accounts that belong to a purged node go with it.
	if err := removeMembers(`
		UPDATE members m SET removed_at = $2
		FROM principals p
		WHERE p.id = m.principal_id AND p.node_id = ANY($1) AND m.removed_at IS NULL
		RETURNING m.id, m.principal
	`, purgedIDs, now); err != nil {
		return nil, err
	}
	if rootPurged {
		if err := removeMembers(`
			UPDATE members m SET removed_at = $2
			WHERE m.root_id = $1 AND m.removed_at IS NULL
			RETURNING m.id, m.principal
		`, rootID, now); err != nil {
			return nil, err
		}
	}
	removedIDs := make([]string, len(removed))
	for i, m := range removed {
		removedIDs[i] = m.memberID
	}
	if len(removedIDs) > 0 {
		if _, err := tx.Exec(ctx, `DELETE FROM group_members WHERE member_id = ANY($1)`, removedIDs); err != nil {
			return nil, err
		}
	}
	if rootPurged {
		if _, err := tx.Exec(ctx, `DELETE FROM groups WHERE root_id = $1`, rootID); err != nil {
			return nil, err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM org_roles WHERE root_id = $1`, rootID); err != nil {
			return nil, err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM root_security_policies WHERE root_id = $1`, rootID); err != nil {
			return nil, err
		}
	}
	rows, err := tx.Query(ctx, `
		WITH deleted AS (
			DELETE FROM bindings WHERE node_id = ANY($1) OR member_id = ANY($2)
			RETURNING member_id
		)
		SELECT DISTINCT member_id FROM deleted WHERE member_id IS NOT NULL
	`, purgedIDs, removedIDs)
	if err != nil {
		return nil, err
	}
	touched, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	// A person's member left with no direct bindings and no group memberships is removed.
	if err := removeMembers(`
		UPDATE members m SET removed_at = $2
		WHERE m.principal = 'person' AND m.id = ANY($1) AND m.removed_at IS NULL
		  AND NOT EXISTS (SELECT 1 FROM bindings b WHERE b.member_id = m.id)
		  AND NOT EXISTS (SELECT 1 FROM group_members gm WHERE gm.member_id = m.id)
		RETURNING m.id, m.principal
	`, touched, now); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM invites WHERE node_id = ANY($1)`, purgedIDs); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM principals WHERE kind = 'service_account' AND node_id = ANY($1)`, purgedIDs); err != nil {
		return nil, err
	}

	sort.Slice(removed, func(i, j int) bool { return removed[i].memberID < removed[j].memberID })
	events := make([]lifecycle.Event, 0, len(removed))
	for _, m := range removed {
		events = append(events, lifecycle.MemberEvent(lifecycle.TypeMemberRemoved, m.memberID, rootID, m.principal))
	}
	return events, nil
}

// write is one transaction whose lifecycle events and admit invalidations are
// recorded last, after every other write, since recording holds the log's lock
// until commit.
type write struct {
	tx            pgx.Tx
	events        []lifecycle.Event
	invalidations []lifecycle.Invalidation
	onInvalidate  func(int64)
}

func (s *Store) begin(ctx context.Context) (*write, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return &write{tx: tx, onInvalidate: s.onInvalidate}, nil
}

func (w *write) event(e lifecycle.Event) {
	w.events = append(w.events, e)
}

func (w *write) invalidate(invs ...lifecycle.Invalidation) {
	w.invalidations = append(w.invalidations, invs...)
}

func (w *write) commit(ctx context.Context) error {
	invHead, err := lifecycle.RecordCommit(ctx, w.tx, w.events, w.invalidations)
	if err != nil {
		return err
	}
	if err := w.tx.Commit(ctx); err != nil {
		return err
	}
	if w.onInvalidate != nil && invHead > 0 {
		w.onInvalidate(invHead)
	}
	return nil
}

func (w *write) rollback(ctx context.Context) {
	_ = w.tx.Rollback(ctx)
}

// --- members inside a write ---

// memberRow is a member as the writes see it.
type memberRow struct {
	id          string
	rootID      string
	principalID *string
	principal   string
	suspended   bool
	removed     bool
}

func (s *Store) lockedMember(ctx context.Context, w *write, memberID string) (*memberRow, error) {
	if !id.Valid(memberID) {
		return nil, ErrNotFound
	}
	var m memberRow
	var suspendedAt, removedAt *time.Time
	err := w.tx.QueryRow(ctx, `
		SELECT id, root_id, principal_id, principal, suspended_at, removed_at FROM members WHERE id = $1 FOR UPDATE
	`, memberID).Scan(&m.id, &m.rootID, &m.principalID, &m.principal, &suspendedAt, &removedAt)
	if dbx.IsNoRows(err) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	m.suspended = suspendedAt != nil
	m.removed = removedAt != nil
	return &m, nil
}

// activeMemberID is the principal's active member in root, or "".
func activeMemberID(ctx context.Context, q dbx.Querier, principalID, rootID string) (string, error) {
	var memberID string
	err := q.QueryRow(ctx, `
		SELECT id FROM members WHERE principal_id = $1 AND root_id = $2 AND removed_at IS NULL
	`, principalID, rootID).Scan(&memberID)
	if dbx.IsNoRows(err) {
		return "", nil
	}
	return memberID, err
}

func (s *Store) insertMember(ctx context.Context, w *write, principalID, principal, rootID string) (string, error) {
	memberID := id.New()
	if _, err := w.tx.Exec(ctx, `
		INSERT INTO members (id, principal_id, principal, root_id, created_at) VALUES ($1, $2, $3, $4, $5)
	`, memberID, principalID, principal, rootID, s.now().UTC()); err != nil {
		// The tree is locked, so the foreign key that can fail is the
		// principal's: DeleteAccount held it and deleted it.
		if dbx.IsForeignKeyViolation(err) {
			return "", errPrincipalGone
		}
		return "", err
	}
	w.event(lifecycle.MemberEvent(lifecycle.TypeMemberCreated, memberID, rootID, principal))
	return memberID, nil
}

// removeMember removes an active member: its direct bindings and group
// memberships go, removed_at is set, and member.removed is written. Final.
func (s *Store) removeMember(ctx context.Context, w *write, m *memberRow) error {
	if _, err := w.tx.Exec(ctx, `DELETE FROM bindings WHERE member_id = $1`, m.id); err != nil {
		return err
	}
	if _, err := w.tx.Exec(ctx, `DELETE FROM group_members WHERE member_id = $1`, m.id); err != nil {
		return err
	}
	if _, err := w.tx.Exec(ctx, `UPDATE members SET removed_at = $2, suspended_at = NULL WHERE id = $1`, m.id, s.now().UTC()); err != nil {
		return err
	}
	w.event(lifecycle.MemberEvent(lifecycle.TypeMemberRemoved, m.id, m.rootID, m.principal))
	w.invalidate(lifecycle.InvalidateMember(m.id))
	return nil
}

// removeEmptyPeople removes each of memberIDs that is a person's active
// member with no direct bindings and no group memberships left.
func (s *Store) removeEmptyPeople(ctx context.Context, w *write, memberIDs []string) error {
	if len(memberIDs) == 0 {
		return nil
	}
	rows, err := w.tx.Query(ctx, `
		UPDATE members m SET removed_at = $2
		WHERE m.id = ANY($1) AND m.principal = 'person' AND m.removed_at IS NULL
		  AND NOT EXISTS (SELECT 1 FROM bindings b WHERE b.member_id = m.id)
		  AND NOT EXISTS (SELECT 1 FROM group_members gm WHERE gm.member_id = m.id)
		RETURNING m.id, m.root_id
	`, memberIDs, s.now().UTC())
	if err != nil {
		return err
	}
	removed, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) ([2]string, error) {
		var out [2]string
		err := r.Scan(&out[0], &out[1])
		return out, err
	})
	if err != nil {
		return err
	}
	sort.Slice(removed, func(i, j int) bool { return removed[i][0] < removed[j][0] })
	for _, m := range removed {
		w.event(lifecycle.MemberEvent(lifecycle.TypeMemberRemoved, m[0], m[1], PrincipalPerson))
		w.invalidate(lifecycle.InvalidateMember(m[0]))
	}
	return nil
}

// --- roles ---

// checkRoleTx is the rule for any binding a write makes: a role is given, is
// either a predefined role in snap or a custom role of rootID, and is bindable
// at kind (docs/concepts/access.md#assigning). field names the role in errors.
func checkRoleTx(ctx context.Context, q dbx.Querier, snap *feed.Snapshot, rootID, roleRef, kind, field string) error {
	if roleRef == "" {
		return errors.FieldError(field, "Choose a role.")
	}
	if role, ok := snap.Role(roleRef); ok {
		if !role.BindableAtKind(kind) {
			return errors.FieldError(field, fmt.Sprintf("That role can't be given on this %s.", kind))
		}
		return nil
	}
	if snap.CustomRoles && id.Valid(roleRef) && q != nil && rootID != "" {
		var bindableAt []string
		err := q.QueryRow(ctx, `SELECT bindable_at FROM org_roles WHERE id = $1 AND root_id = $2`, roleRef, rootID).Scan(&bindableAt)
		if err == nil {
			for _, k := range bindableAt {
				if k == kind {
					return nil
				}
			}
			return errors.FieldError(field, fmt.Sprintf("That role can't be given on this %s.", kind))
		}
		if !dbx.IsNoRows(err) {
			return err
		}
	}
	return errors.FieldError(field, "That role doesn't exist.")
}
