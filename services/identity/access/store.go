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

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"

	"github.com/plat5dev/plat5/identity/feed"
	"github.com/plat5dev/plat5/identity/internal/dbx"
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

// WithPDP returns a copy of the store using a custom AuthZEN PDP client.
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

// MemberTarget is one (member, target node) pair whose effective roles along
// the node's ancestor chain should be resolved.
type MemberTarget struct {
	MemberID string
	NodeID   string
}

// CountsWithCustomSQL is the SQL condition for a binding b on node bn that
// counts when the caller has already LEFT JOINed org_roles as crAlias:
// its role is in the document and bindable at bn's kind, or matched crAlias.
func CountsWithCustomSQL(kinds, roles, crAlias string) string {
	return `((bn.kind, b.role) IN (SELECT * FROM unnest(` + kinds + `::text[], ` + roles + `::text[])) OR ` + crAlias + `.id IS NOT NULL)`
}

// HeldOnTargets resolves, for each target in targets, the predefined roles and
// custom-role permissions of the member's unexpired bindings (direct and via
// groups) on the target node and its ancestors. When allowDeletedSelf is true,
// bindings on a deleted target node itself count alongside live ancestors
// (used for ?status=deleted node lists). Custom roles count only when
// snap.CustomRoles is true.
func HeldOnTargets(
	ctx context.Context,
	q dbx.Querier,
	snap *feed.Snapshot,
	targets []MemberTarget,
	allowDeletedSelf bool,
	now time.Time,
) ([][]string, [][]string, error) {
	outRoles := make([][]string, len(targets))
	outCustom := make([][]string, len(targets))
	if len(targets) == 0 || snap == nil || snap.ExternalAuthz() {
		return outRoles, outCustom, nil
	}
	idx := make([]int32, len(targets))
	memberIDs := make([]string, len(targets))
	nodeIDs := make([]string, len(targets))
	for i, t := range targets {
		idx[i] = int32(i)
		memberIDs[i] = t.MemberID
		nodeIDs[i] = t.NodeID
	}
	kinds, roles := snap.BindablePairs()
	rows, err := q.Query(ctx, `
		SELECT c.i, b.role, COALESCE(cr.permissions, '{}')
		FROM unnest($1::int[], $2::text[], $3::text[]) AS c (i, member_id, node_id)
		JOIN node_ancestors a ON a.node_id = c.node_id
		JOIN bindings b ON b.node_id = a.ancestor_id AND (
			b.member_id = c.member_id
			OR EXISTS (SELECT 1 FROM group_members gm WHERE gm.group_id = b.group_id AND gm.member_id = c.member_id)
		)
		JOIN nodes bn ON bn.id = b.node_id
		LEFT JOIN org_roles cr
		  ON $7 AND cr.id = b.role AND cr.root_id = bn.root_id AND bn.kind = ANY(cr.bindable_at)
		WHERE (bn.status = 'active' OR ($8 AND bn.id = c.node_id))
		  AND (b.expires_at IS NULL OR b.expires_at > $6)
		  AND `+CountsWithCustomSQL("$4", "$5", "cr"),
		idx, memberIDs, nodeIDs, kinds, roles, now, snap.CustomRoles, allowDeletedSelf)
	if err != nil {
		return nil, nil, err
	}
	var i int32
	var role string
	var perms []string
	if _, err := pgx.ForEachRow(rows, []any{&i, &role, &perms}, func() error {
		outRoles[i] = append(outRoles[i], role)
		if len(perms) > 0 {
			outCustom[i] = append(outCustom[i], perms...)
		}
		return nil
	}); err != nil {
		return nil, nil, err
	}
	return outRoles, outCustom, nil
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
	if snap == nil {
		return out
	}
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
	if snap.CustomRoles {
		for _, p := range customPerms {
			if p == "" || p == "*" {
				continue
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
