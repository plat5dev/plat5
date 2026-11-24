// Package access is identity's read/decision plane: gateway admission
// (POST /internal/admit), AuthZEN access evaluation (POST /access/v1/evaluation,
// /access/v1/evaluations), AuthZEN resource search (POST /access/v1/search/resource),
// and the shared role-to-permission resolution rules (docs/concepts/access.md,
// docs/reference/api/internal.md).
package access

import (
	"context"
	stderrors "errors"
	"sort"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"

	"github.com/plat5dev/plat5/identity/feed"
)

const (
	principalPerson = "person"
	statusActive    = "active"
	storeTracerName = "identity.access.store"
)

// PersonRegistrar creates a person on first sign-in for a new IdP subject,
// seeding their display name when present. *members.Store implements it.
type PersonRegistrar interface {
	FirstSignIn(ctx context.Context, issuer, subject, name string) (principalID, userID, status string, err error)
}

// Store decides access questions against identity's Postgres tables and the
// control plane's live snapshot. Its only write path is delegating first sign-in
// to PersonRegistrar and seeding an empty person's display name from IdP claims.
type Store struct {
	pool      *pgxpool.Pool
	brand     string
	registrar PersonRegistrar
	pdp       PDP
	tracer    trace.Tracer
	now       func() time.Time
}

// NewStore needs SECRET_BRAND to validate API key prefixes and registrar to
// create a person on first IdP sign-in.
func NewStore(pool *pgxpool.Pool, brand string, registrar PersonRegistrar) *Store {
	return &Store{
		pool:      pool,
		brand:     brand,
		registrar: registrar,
		pdp:       NewHTTPPDP(),
		tracer:    otel.Tracer(storeTracerName),
		now:       time.Now,
	}
}

// WithPDP returns a copy of the store using a custom AuthZEN PDP client, for tests.
func (s *Store) WithPDP(pdp PDP) *Store {
	c := *s
	c.pdp = pdp
	return &c
}

// WithClock returns a copy of the store using a custom clock, for tests.
func (s *Store) WithClock(now func() time.Time) *Store {
	c := *s
	c.now = now
	return &c
}

var errNoRegistrar = stderrors.New("access: no PersonRegistrar configured for first sign-in")

// BindablePairs is every (kind, role) where the role can be bound, as two
// parallel arrays for SQL: a binding counts only when its node's kind and role
// are one of these pairs.
func BindablePairs(snap *feed.Snapshot) (kinds, roles []string) {
	for _, slug := range snap.RoleSlugs() {
		for _, k := range snap.Roles[slug].BindableAt {
			kinds = append(kinds, k)
			roles = append(roles, slug)
		}
	}
	return kinds, roles
}

// CountsSQL is the SQL condition for a binding b on node bn that counts: its
// role is in the document and bindable at bn's kind, or is an org custom role
// of bn's root bindable at bn's kind. kinds and roles are the parameter
// placeholders for BindablePairs's slices.
func CountsSQL(kinds, roles string) string {
	return `((bn.kind, b.role) IN (SELECT * FROM unnest(` + kinds + `::text[], ` + roles + `::text[])) OR EXISTS (SELECT 1 FROM org_roles cr WHERE cr.id = b.role AND cr.root_id = bn.root_id AND bn.kind = ANY(cr.bindable_at)))`
}

// Permissions is the union of the permissions of roles a member holds at a
// node (docs/concepts/access.md#resolution). All is "*".
type Permissions struct {
	All bool
	Set map[string]bool
}

// Resolve unions the permissions of roles, which must already count
// (bindable where they sit). A role gone from the document grants nothing.
func Resolve(snap *feed.Snapshot, roles []string) Permissions {
	return ResolveWithCustom(snap, roles, nil)
}

// ResolveWithCustom unions predefined role permissions from snap with
// customPerms collected from matching org_roles rows. "*" in customPerms is
// ignored: custom roles never grant wildcard access.
func ResolveWithCustom(snap *feed.Snapshot, roles []string, customPerms []string) Permissions {
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
	for _, p := range customPerms {
		if p == "" || p == "*" {
			continue
		}
		out.Set[p] = true
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

// IntersectScopes narrows resolved permissions to those also present in scopes,
// returning a sorted, deduplicated list. A role granting "*" narrows to scopes
// itself rather than keeping "*".
func IntersectScopes(p Permissions, scopes []string) []string {
	seen := make(map[string]struct{}, len(scopes))
	out := make([]string, 0, len(scopes))
	for _, s := range scopes {
		if s == "" || s == "*" {
			continue
		}
		if _, dup := seen[s]; dup {
			continue
		}
		if p.Has(s) {
			seen[s] = struct{}{}
			out = append(out, s)
		}
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
