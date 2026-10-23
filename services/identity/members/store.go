// Package members is who can reach the tree: people and their IdP links, the
// directory member, members of each tree, bindings with expiry, service
// accounts, API keys and invites (docs/concepts/access.md,
// docs/concepts/lifecycle.md#members, docs/internals/identity-schema.md).
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
)

var (
	ErrNotFound = stderrors.New("not found")
	// ErrNotMember is a caller (X-Plat5-Member-Id) that isn't an active member
	// of the addressed tree: a malformed request.
	ErrNotMember = stderrors.New("caller is not a member of this tree")
)

// Store writes everything about who can reach a node. Writes that touch a
// tree's members or bindings hold that tree's lock (nodes.LockTrees), so "one
// active member per principal per tree" and "removed when left with no
// bindings" can't race.
type Store struct {
	pool     *pgxpool.Pool
	nodes    *nodes.Store
	signInID string
	brand    string
	tracer   trace.Tracer
	now      func() time.Time
}

// NewStore needs the sign-in node's id (made at boot) and APIKEY_BRAND.
func NewStore(pool *pgxpool.Pool, nodeStore *nodes.Store, signInID, brand string) *Store {
	return &Store{
		pool:     pool,
		nodes:    nodeStore,
		signInID: signInID,
		brand:    brand,
		tracer:   otel.Tracer(storeTracerName),
		now:      time.Now,
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
	memberID := id.New(id.Member)
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

// removeEmptyPeople removes each of memberIDs that is a person's active member
// outside the directory with no bindings left.
func (s *Store) removeEmptyPeople(ctx context.Context, w *write, memberIDs []string) error {
	if len(memberIDs) == 0 {
		return nil
	}
	rows, err := w.tx.Query(ctx, `
		UPDATE members m SET removed_at = $3
		WHERE m.id = ANY($1) AND m.principal = 'person' AND m.removed_at IS NULL AND m.root_id <> $2
		  AND NOT EXISTS (SELECT 1 FROM bindings b WHERE b.member_id = m.id)
		RETURNING m.id, m.root_id
	`, memberIDs, s.signInID, s.now().UTC())
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

// checkRole is the rule for any binding a write makes: the role exists, is
// bindable at the node's kind, and the expiry meets its max_duration
// (docs/concepts/access.md#assigning). field names the expiry in errors.
func (s *Store) checkRole(snap *feed.Snapshot, roleSlug, kind string, expiresAt *time.Time, field string) error {
	role, err := roleAt(snap, roleSlug, kind)
	if err != nil {
		return err
	}
	now := s.now()
	if expiresAt != nil && !expiresAt.After(now) {
		return errors.FieldError(field, errors.FallbackValidation)
	}
	if role.MaxDuration > 0 && (expiresAt == nil || expiresAt.After(now.Add(role.MaxDuration))) {
		return maxDurationError(role, field)
	}
	return nil
}

// checkDuration is checkRole for an invite's role_duration or a grant's
// duration: how long the binding lasts from redeem.
func checkDuration(snap *feed.Snapshot, roleSlug, kind string, d *time.Duration, field string) error {
	role, err := roleAt(snap, roleSlug, kind)
	if err != nil {
		return err
	}
	if role.MaxDuration > 0 && (d == nil || *d > role.MaxDuration) {
		return maxDurationError(role, field)
	}
	return nil
}

func roleAt(snap *feed.Snapshot, roleSlug, kind string) (feed.Role, error) {
	if roleSlug == "" {
		return feed.Role{}, errors.FieldError("role", "Choose a role.")
	}
	role, ok := snap.Role(roleSlug)
	if !ok {
		return feed.Role{}, errors.FieldError("role", "That role doesn't exist.")
	}
	if !role.BindableAtKind(kind) {
		return feed.Role{}, errors.FieldError("role", fmt.Sprintf("That role can't be given on a %s.", nodes.KindWord(snap, kind)))
	}
	return role, nil
}

func maxDurationError(role feed.Role, field string) error {
	return errors.FieldError(field, fmt.Sprintf("This role can be given for at most %s.", role.MaxDurationText))
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

// counts is the SQL condition for a binding b on node bn that counts at now
// ($n): unexpired, its role in the document and bindable at bn's kind. kinds
// and roles are the arrays from bindablePairs.
func counts(now, kinds, roles string) string {
	return `(b.expires_at IS NULL OR b.expires_at > ` + now + `)
		AND (bn.kind, b.role) IN (SELECT * FROM unnest(` + kinds + `::text[], ` + roles + `::text[]))`
}

// --- resolution ---

// Labels is the union of roles' labels a member holds at a node
// (docs/concepts/access.md#resolution). All is "*".
type Labels struct {
	All bool
	Set map[string]bool
}

// Resolve unions the labels of roles, which must already count (unexpired,
// bindable where they sit). A role gone from the document grants nothing.
func Resolve(snap *feed.Snapshot, roles []string) Labels {
	out := Labels{Set: map[string]bool{}}
	for _, slug := range roles {
		role, ok := snap.Role(slug)
		if !ok {
			continue
		}
		for _, l := range role.Labels {
			if l == "*" {
				out.All = true
			}
			out.Set[l] = true
		}
	}
	return out
}

// Allows is whether l meets a route's labels: open to any member, "*", or
// any one label in common.
func (l Labels) Allows(need feed.RouteLabels) bool {
	if need.Open || l.All {
		return true
	}
	for _, label := range need.Labels {
		if l.Set[label] {
			return true
		}
	}
	return false
}
