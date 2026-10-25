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
)

// Store writes everything about who can reach a node. Writes that touch a
// tree's members or bindings hold that tree's lock (nodes.LockTrees), so "one
// active member per principal per tree" and "removed when left with no
// bindings" can't race.
type Store struct {
	pool   *pgxpool.Pool
	nodes  *nodes.Store
	brand  string
	tracer trace.Tracer
	now    func() time.Time
}

// NewStore needs APIKEY_BRAND for the keys it mints and reads.
func NewStore(pool *pgxpool.Pool, nodeStore *nodes.Store, brand string) *Store {
	return &Store{
		pool:   pool,
		nodes:  nodeStore,
		brand:  brand,
		tracer: otel.Tracer(storeTracerName),
		now:    time.Now,
	}
}

// WithClock is the store with a fixed notion of now, for tests of expiry. The
// node store it creates nodes with gets the same clock.
func (s *Store) WithClock(now func() time.Time) *Store {
	c := *s
	c.now = now
	c.nodes = s.nodes.WithClock(now)
	return &c
}

// write is one transaction whose lifecycle events are recorded last, after
// every other write, since recording holds the events lock until commit.
type write struct {
	tx     pgx.Tx
	events []lifecycle.Event
}

func (s *Store) begin(ctx context.Context) (*write, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return &write{tx: tx}, nil
}

func (w *write) event(e lifecycle.Event) {
	w.events = append(w.events, e)
}

func (w *write) commit(ctx context.Context) error {
	for _, e := range w.events {
		if err := lifecycle.Record(ctx, w.tx, e); err != nil {
			return err
		}
	}
	return w.tx.Commit(ctx)
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
	removed     bool
}

func (s *Store) lockedMember(ctx context.Context, w *write, memberID string) (*memberRow, error) {
	if !id.Valid(memberID) {
		return nil, ErrNotFound
	}
	var m memberRow
	var removedAt *time.Time
	err := w.tx.QueryRow(ctx, `
		SELECT id, root_id, principal_id, principal, removed_at FROM members WHERE id = $1 FOR UPDATE
	`, memberID).Scan(&m.id, &m.rootID, &m.principalID, &m.principal, &removedAt)
	if dbx.IsNoRows(err) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
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
		return "", err
	}
	w.event(lifecycle.MemberEvent(lifecycle.TypeMemberCreated, memberID, rootID, principal))
	return memberID, nil
}

// removeMember removes an active member: its bindings go, removed_at is set,
// and member.removed is written. Final.
func (s *Store) removeMember(ctx context.Context, w *write, m *memberRow) error {
	if _, err := w.tx.Exec(ctx, `DELETE FROM bindings WHERE member_id = $1`, m.id); err != nil {
		return err
	}
	if _, err := w.tx.Exec(ctx, `UPDATE members SET removed_at = $2 WHERE id = $1`, m.id, s.now().UTC()); err != nil {
		return err
	}
	w.event(lifecycle.MemberEvent(lifecycle.TypeMemberRemoved, m.id, m.rootID, m.principal))
	return nil
}

// removeEmptyPeople removes each of memberIDs that is a person's active
// member with no bindings left.
func (s *Store) removeEmptyPeople(ctx context.Context, w *write, memberIDs []string) error {
	if len(memberIDs) == 0 {
		return nil
	}
	rows, err := w.tx.Query(ctx, `
		UPDATE members m SET removed_at = $2
		WHERE m.id = ANY($1) AND m.principal = 'person' AND m.removed_at IS NULL
		  AND NOT EXISTS (SELECT 1 FROM bindings b WHERE b.member_id = m.id)
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
	}
	return nil
}

// --- roles ---

// checkRole is the rule for any binding a write makes: a role is given, is in
// the document, and is bindable at the node's kind
// (docs/concepts/access.md#assigning). field names the role in errors.
func checkRole(snap *feed.Snapshot, roleSlug, kind, field string) error {
	if roleSlug == "" {
		return errors.FieldError(field, "Choose a role.")
	}
	role, ok := snap.Role(roleSlug)
	if !ok {
		return errors.FieldError(field, "That role doesn't exist.")
	}
	if !role.BindableAtKind(kind) {
		return errors.FieldError(field, fmt.Sprintf("That role can't be given on this %s.", kind))
	}
	return nil
}

// bindablePairs is every (kind, role) where the role can be bound, as two
// arrays for SQL: a binding counts only when its node's kind and role are one
// of these pairs.
func bindablePairs(snap *feed.Snapshot) (kinds, roles []string) {
	for _, slug := range snap.RoleSlugs() {
		for _, k := range snap.Roles[slug].BindableAt {
			kinds = append(kinds, k)
			roles = append(roles, slug)
		}
	}
	return kinds, roles
}

// counts is the SQL condition for a binding b on node bn that counts: its
// role in the document and bindable at bn's kind. kinds and roles are the
// arrays from bindablePairs.
func counts(kinds, roles string) string {
	return `(bn.kind, b.role) IN (SELECT * FROM unnest(` + kinds + `::text[], ` + roles + `::text[]))`
}

// --- resolution ---

// Permissions is the union of the permissions of roles a member holds at a
// node (docs/concepts/access.md#resolution). All is "*".
type Permissions struct {
	All bool
	Set map[string]bool
}

// Resolve unions the permissions of roles, which must already count
// (bindable where they sit). A role gone from the document grants nothing.
func Resolve(snap *feed.Snapshot, roles []string) Permissions {
	out := Permissions{Set: map[string]bool{}}
	for _, slug := range roles {
		role, ok := snap.Role(slug)
		if !ok {
			continue
		}
		for _, p := range role.Permissions {
			if p == "*" {
				out.All = true
			}
			out.Set[p] = true
		}
	}
	return out
}

// Has is whether p grants permission: "*", or permission itself.
func (p Permissions) Has(permission string) bool {
	return p.All || p.Set[permission]
}

// List is the permissions, sorted, with * as written.
func (p Permissions) List() []string {
	out := make([]string, 0, len(p.Set))
	for perm := range p.Set {
		out = append(out, perm)
	}
	sort.Strings(out)
	return out
}

// page trims a limit+1 read to limit and says whether there was more.
func page[T any](items []T, limit int) ([]T, bool) {
	if len(items) > limit {
		return items[:limit], true
	}
	return items, false
}
