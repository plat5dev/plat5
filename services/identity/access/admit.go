package access

import (
	"context"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/plat5dev/plat5/identity/errors"
	"github.com/plat5dev/plat5/identity/feed"
	"github.com/plat5dev/plat5/identity/internal/dbx"
	"github.com/plat5dev/plat5/identity/internal/id"
	"github.com/plat5dev/plat5/identity/internal/secret"
	"github.com/plat5dev/plat5/identity/nodes"
)

// AdmitInput is POST /internal/admit (docs/reference/api/internal.md#admit):
// a credential, or the IdP's issuer and subject, and the route's node chain.
// An empty chain is a user route.
type AdmitInput struct {
	Credential   *string
	IdP          *IdPClaims
	Chain        []ChainRef
	AllowDeleted bool
}

// IdPClaims is what the gateway read from a verified JWT. Name is the first
// non-empty claim of AUTH_NAME_CLAIMS.
type IdPClaims struct {
	Issuer, Subject string
	Name            *string
}

// ChainRef is one chain param from the path: an id when it starts with a
// digit, else a slug.
type ChainRef struct {
	Kind, Ref string
}

// ChainNode is a resolved chain node.
type ChainNode struct {
	Kind, ID string
}

// Admitted is admit's answer. Empty strings are nulls on the wire.
type Admitted struct {
	Valid           bool
	Principal       string
	KeyID           string
	AllowedCIDRs    []string
	Nodes           []ChainNode
	MemberID        string
	UserID          string
	Permissions     []string
	InvalidationSeq int64
	ConfigRevision  int64
}

// admitPrincipal is the principal behind a credential.
type admitPrincipal struct {
	id, kind, userID, keyID string
	allowedCIDRs            []string
	invalidationSeq         int64
}

// Admit turns a credential and a node chain into the member and permissions
// for one request, in the steps of the contract. It writes only for a new IdP
// subject (the person) or a name seeded on first sign-in.
func (s *Store) Admit(ctx context.Context, snap *feed.Snapshot, in AdmitInput) (*Admitted, error) {
	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "admit", dbx.DefaultTimeout)
	defer cancel()
	defer op.End()

	if err := checkAdmit(in); err != nil {
		return nil, op.Expected("invalid", err)
	}
	userRoute := len(in.Chain) == 0

	// 1. Principal.
	p, seq, err := s.admitPrincipal(ctx, in)
	if err != nil {
		return nil, op.Fail(err)
	}
	var configRev int64
	if snap != nil {
		configRev = snap.Revision
	}
	if p == nil {
		op.OK("invalid")
		return &Admitted{InvalidationSeq: seq, ConfigRevision: configRev}, nil
	}
	out := &Admitted{
		Valid:           true,
		Principal:       p.kind,
		KeyID:           p.keyID,
		AllowedCIDRs:    p.allowedCIDRs,
		InvalidationSeq: p.invalidationSeq,
		ConfigRevision:  configRev,
	}
	if userRoute {
		// A person's user id. A service account has none: the gateway refuses
		// it on a user route.
		out.UserID = p.userID
		op.OK("admitted")
		return out, nil
	}

	// 2–4. Resolve the node chain, the principal's active member in its root,
	// and the member's counting roles along the chain in one query.
	chain, memberID, roles, chainSeq, err := s.resolveAdmitChain(ctx, snap, p.id, in.Chain, in.AllowDeleted)
	if err != nil {
		return nil, op.Fail(err)
	}
	// Use the earlier sequence of the two queries so a mutation between step 1
	// and step 2 can never be hidden behind a newer invalidation_seq.
	if chainSeq < out.InvalidationSeq {
		out.InvalidationSeq = chainSeq
	}
	if chain == nil {
		op.OK("not found")
		return out, nil
	}
	if memberID == "" {
		op.OK("no member")
		return out, nil
	}
	out.Nodes, out.MemberID = chain, memberID
	out.Permissions = Resolve(snap, roles).List()
	op.OK("admitted")
	return out, nil
}

// checkAdmit refuses a body identity can't read (422). The chain's kinds are
// matched by name only, never against the config: a gateway on another
// revision gets member_id null, not an error.
func checkAdmit(in AdmitInput) error {
	if (in.Credential == nil) == (in.IdP == nil) {
		return errors.FieldError("credential", "Send a credential or idp.")
	}
	if in.IdP != nil && (strings.TrimSpace(in.IdP.Issuer) == "" || strings.TrimSpace(in.IdP.Subject) == "") {
		return errors.FieldError("idp", errors.FallbackValidation)
	}
	return nil
}

// admitPrincipal finds the principal, or nil when the credential is invalid:
// a key identity can't read, an unknown or revoked key, a suspended person,
// or a service account whose node is deleted. A new IdP subject becomes
// a person.
func (s *Store) admitPrincipal(ctx context.Context, in AdmitInput) (*admitPrincipal, int64, error) {
	if in.Credential != nil {
		return s.keyPrincipalFor(ctx, *in.Credential)
	}
	return s.idpPrincipal(ctx, *in.IdP)
}

func (s *Store) keyPrincipalFor(ctx context.Context, key string) (*admitPrincipal, int64, error) {
	if !secret.Parse(secret.KeyPrefix(s.brand), key) {
		return nil, 0, nil
	}
	var p admitPrincipal
	var userID *string
	var status string
	var revoked bool
	var nodeStatus *string
	err := s.pool.QueryRow(ctx, `
		SELECT p.id, p.kind, p.user_id, k.id, k.allowed_cidrs, p.status, k.revoked_at IS NOT NULL, bn.status,
		       COALESCE((SELECT MAX(id) FROM admit_invalidations), 0)
		FROM api_keys k
		JOIN principals p ON p.id = k.principal_id
		LEFT JOIN nodes bn ON bn.id = p.node_id
		WHERE k.key_hash = $1
	`, secret.Hash(key)).Scan(&p.id, &p.kind, &userID, &p.keyID, &p.allowedCIDRs, &status, &revoked, &nodeStatus, &p.invalidationSeq)
	if dbx.IsNoRows(err) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, err
	}
	if revoked || !admissible(status, nodeStatus) {
		return nil, p.invalidationSeq, nil
	}
	if userID != nil {
		p.userID = *userID
	}
	return &p, p.invalidationSeq, nil
}

func (s *Store) idpPrincipal(ctx context.Context, c IdPClaims) (*admitPrincipal, int64, error) {
	issuer, subject := strings.TrimSpace(c.Issuer), strings.TrimSpace(c.Subject)
	var name string
	if c.Name != nil {
		name = claimName(*c.Name)
	}
	var principalID, userID, status string
	var current *string
	var seq int64
	err := s.pool.QueryRow(ctx, `
		SELECT p.id, p.user_id, p.status, p.name,
		       COALESCE((SELECT MAX(id) FROM admit_invalidations), 0)
		FROM idp_links l JOIN principals p ON p.id = l.principal_id
		WHERE l.issuer = $1 AND l.subject = $2
	`, issuer, subject).Scan(&principalID, &userID, &status, &current, &seq)
	switch {
	case dbx.IsNoRows(err):
		if s.registrar == nil {
			return nil, 0, errNoRegistrar
		}
		if principalID, userID, status, err = s.registrar.FirstSignIn(ctx, issuer, subject, name); err != nil {
			return nil, 0, err
		}
	case err != nil:
		return nil, 0, err
	case name != "" && current == nil:
		// Seed the person's name if it is still empty; preserve any concurrent edit.
		if _, err := s.pool.Exec(ctx, `
			UPDATE principals SET name = $2, updated_at = $3
			WHERE id = $1 AND name IS NULL
		`, principalID, name, s.now().UTC()); err != nil {
			return nil, 0, err
		}
	}
	if !admissible(status, nil) {
		return nil, seq, nil
	}
	return &admitPrincipal{id: principalID, kind: principalPerson, userID: userID, invalidationSeq: seq}, seq, nil
}

// claimName is a name claim as a person's name: trimmed, and cut to the
// longest name there is. Empty is no name.
func claimName(raw string) string {
	name := strings.TrimSpace(raw)
	if utf8.RuneCountInString(name) > secret.MaxNameLen {
		name = strings.TrimSpace(string([]rune(name)[:secret.MaxNameLen]))
	}
	return name
}

// admissible is whether admit accepts a principal, whatever its credential: a
// person who isn't suspended, or a service account whose node (saNodeStatus,
// nil for a person) is live.
func admissible(status string, saNodeStatus *string) bool {
	return status == statusActive && (saNodeStatus == nil || *saNodeStatus == nodes.StatusActive)
}

// resolveAdmitChain resolves the node chain, the principal's active member in
// the chain's root, and the roles of that member's counting bindings (direct
// and group) on every node in the chain, in a single Postgres round-trip.
// Each hop splits ID lookups (nodes_pkey) from slug lookups (nodes_slug_uidx)
// via a LATERAL UNION ALL so Postgres can use direct index scans instead of an
// OR filter over every child of the parent.
// Returns chain == nil when any node is missing, purged, or deleted (except a
// deleted last node with allowDeleted), and memberID == "" when the principal
// is not an active member of the resolved root.
func (s *Store) resolveAdmitChain(
	ctx context.Context,
	snap *feed.Snapshot,
	principalID string,
	refs []ChainRef,
	allowDeleted bool,
) ([]ChainNode, string, []string, int64, error) {
	n := len(refs)
	pos := make([]int32, n)
	kinds := make([]string, n)
	refVals := make([]string, n)
	isIDs := make([]bool, n)
	for i, c := range refs {
		isID := id.IsID(c.Ref)
		if isID && !id.Valid(c.Ref) {
			return nil, "", nil, 0, nil
		}
		pos[i] = int32(i + 1)
		kinds[i] = c.Kind
		refVals[i] = c.Ref
		isIDs[i] = isID
	}
	bindKinds, bindRoles := BindablePairs(snap)
	if bindKinds == nil {
		bindKinds, bindRoles = []string{}, []string{}
	}

	var nodeIDs []string
	var memberID *string
	var roles []string
	var invSeq int64
	err := s.pool.QueryRow(ctx, `
		WITH RECURSIVE
			reqs (pos, kind, ref, is_id) AS (
				SELECT * FROM unnest($1::int[], $2::text[], $3::text[], $4::bool[])
			),
			chain (pos, kind, id) AS (
				SELECT r.pos, n.kind, n.id
				FROM reqs r
				CROSS JOIN LATERAL (
					SELECT n.kind, n.id
					FROM nodes n
					WHERE r.is_id
					  AND n.id = r.ref
					  AND n.parent_id IS NULL
					  AND n.kind = r.kind
					  AND (n.status = 'active' OR (r.pos = $5 AND $6 AND n.status = 'deleted'))
					UNION ALL
					SELECT n.kind, n.id
					FROM nodes n
					WHERE NOT r.is_id
					  AND COALESCE(n.parent_id, '') = ''
					  AND n.kind = r.kind
					  AND n.slug = r.ref
					  AND n.slug IS NOT NULL
					  AND (n.status = 'active' OR (r.pos = $5 AND $6 AND n.status = 'deleted'))
				) n
				WHERE r.pos = 1
				UNION ALL
				SELECT r.pos, n.kind, n.id
				FROM chain c
				JOIN reqs r ON r.pos = c.pos + 1
				CROSS JOIN LATERAL (
					SELECT n.kind, n.id
					FROM nodes n
					WHERE r.is_id
					  AND n.id = r.ref
					  AND n.parent_id = c.id
					  AND n.kind = r.kind
					  AND (n.status = 'active' OR (r.pos = $5 AND $6 AND n.status = 'deleted'))
					UNION ALL
					SELECT n.kind, n.id
					FROM nodes n
					WHERE NOT r.is_id
					  AND COALESCE(n.parent_id, '') = c.id
					  AND n.kind = r.kind
					  AND n.slug = r.ref
					  AND n.slug IS NOT NULL
					  AND (n.status = 'active' OR (r.pos = $5 AND $6 AND n.status = 'deleted'))
				) n
			),
			resolved AS (
				SELECT pos, kind, id
				FROM chain
				WHERE (SELECT count(*) FROM chain) = $5
			),
			member AS (
				SELECT m.id
				FROM members m
				JOIN resolved r ON r.pos = 1 AND m.root_id = r.id
				WHERE m.principal_id = $7 AND m.removed_at IS NULL
			),
			roles AS (
				SELECT DISTINCT b.role
				FROM resolved bn
				JOIN member m ON true
				JOIN bindings b ON b.node_id = bn.id AND (
					b.member_id = m.id
					OR EXISTS (SELECT 1 FROM group_members gm WHERE gm.group_id = b.group_id AND gm.member_id = m.id)
				)
				WHERE `+CountsSQL("$8", "$9")+`
			)
		SELECT
			(SELECT array_agg(id ORDER BY pos) FROM resolved),
			(SELECT id FROM member),
			(SELECT array_agg(role) FROM roles),
			COALESCE((SELECT MAX(id) FROM admit_invalidations), 0)
	`, pos, kinds, refVals, isIDs, n, allowDeleted, principalID, bindKinds, bindRoles).Scan(&nodeIDs, &memberID, &roles, &invSeq)
	if err != nil {
		return nil, "", nil, 0, err
	}
	if len(nodeIDs) != n {
		return nil, "", nil, invSeq, nil
	}
	out := make([]ChainNode, n)
	for i, nodeID := range nodeIDs {
		out[i] = ChainNode{Kind: refs[i].Kind, ID: nodeID}
	}
	if memberID == nil {
		return out, "", nil, invSeq, nil
	}
	return out, *memberID, roles, invSeq, nil
}

// liveInChain is whether a node lets its chain resolve: live, or, with
// allowDeleted, a deleted last node.
func liveInChain(status string, last, allowDeleted bool) bool {
	return status == nodes.StatusActive || (last && allowDeleted && status == nodes.StatusDeleted)
}

// memberChain is one member on one resolved chain.
type memberChain struct {
	memberID string
	nodes    []ChainNode
}

// queryer is a pool or a transaction.
type queryer interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// heldOnChains is, for each pair, the roles of the member's bindings on the
// chain's nodes that count there (bindable at the node's kind): what
// Resolve unions. Evaluations and resource search share it.
func heldOnChains(ctx context.Context, q queryer, snap *feed.Snapshot, pairs []memberChain) ([][]string, error) {
	var idx []int32
	var memberIDs, nodeIDs []string
	for i, p := range pairs {
		for _, n := range p.nodes {
			idx = append(idx, int32(i))
			memberIDs = append(memberIDs, p.memberID)
			nodeIDs = append(nodeIDs, n.ID)
		}
	}
	out := make([][]string, len(pairs))
	if len(idx) == 0 {
		return out, nil
	}
	kinds, roles := BindablePairs(snap)
	rows, err := q.Query(ctx, `
		SELECT c.i, b.role
		FROM unnest($1::int[], $2::text[], $3::text[]) AS c (i, member_id, node_id)
		JOIN bindings b ON b.node_id = c.node_id AND (
			b.member_id = c.member_id
			OR EXISTS (SELECT 1 FROM group_members gm WHERE gm.group_id = b.group_id AND gm.member_id = c.member_id)
		)
		JOIN nodes bn ON bn.id = b.node_id
		WHERE `+CountsSQL("$4", "$5"),
		idx, memberIDs, nodeIDs, kinds, roles)
	if err != nil {
		return nil, err
	}
	var i int32
	var role string
	if _, err := pgx.ForEachRow(rows, []any{&i, &role}, func() error {
		out[i] = append(out[i], role)
		return nil
	}); err != nil {
		return nil, err
	}
	return out, nil
}
