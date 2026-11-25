package access

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/plat5dev/plat5/identity/errors"
	"github.com/plat5dev/plat5/identity/feed"
	"github.com/plat5dev/plat5/identity/internal/dbx"
	"github.com/plat5dev/plat5/identity/internal/id"
	"github.com/plat5dev/plat5/identity/internal/secret"
	"github.com/plat5dev/plat5/identity/lifecycle"
	"github.com/plat5dev/plat5/identity/nodes"
)

const principalAnonymous = "anonymous"

// AdmitInput is POST /internal/admit (docs/reference/api/internal.md#admit):
// a credential, the IdP's issuer and subject, or anonymous: true (on an
// allow_public kind route), plus the route's node chain. An empty chain is a
// user route.
type AdmitInput struct {
	Credential   *string
	IdP          *IdPClaims
	Anonymous    bool
	Chain        []ChainRef
	AllowDeleted bool
	AllowPublic  bool
}

// IdPClaims is what the gateway read from a verified JWT. Name is the first
// non-empty claim of AUTH_NAME_CLAIMS. When HasScopes is true, effective
// permissions on kind routes are intersected with Scopes. PolicyClaims carries
// the discrete claims configured via AUTH_POLICY_CLAIMS for per-root policy
// enforcement.
type IdPClaims struct {
	Issuer, Subject string
	Name            *string
	HasScopes       bool
	Scopes          []string
	PolicyClaims    map[string][]string
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
	Valid            bool
	Principal        string
	KeyID            string
	AllowedCIDRs     []string
	OrgAllowedCIDRs  []string
	Nodes            []ChainNode
	MemberID         string
	UserID           string
	Permissions      []string
	CredentialScopes *[]string
	RateLimitTier    *string
	ExpiresAt        *time.Time
	InvalidationSeq  int64
	ConfigRevision   int64
}

type rootSecurityPolicy struct {
	allowPersonalKeys bool
	allowedCIDRs      []string
	requiredIdPClaims map[string][]string
}

func matchesRequiredClaims(have, required map[string][]string) bool {
	for claim, wantVals := range required {
		if len(wantVals) == 0 {
			continue
		}
		gotVals := have[claim]
		matched := false
		for _, g := range gotVals {
			if slices.Contains(wantVals, g) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	return true
}

// admitPrincipal is the principal behind a credential.
type admitPrincipal struct {
	id, kind, userID, keyID string
	allowedCIDRs            []string
	scopes                  []string
	allowedRoots            []string
	expiresAt               *time.Time
	invalidationSeq         int64
}

func earliestTime(a, b *time.Time) *time.Time {
	switch {
	case a == nil:
		return b
	case b == nil:
		return a
	case b.Before(*a):
		return b
	default:
		return a
	}
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
	var configRev int64
	if snap != nil {
		configRev = snap.Revision
	}

	if in.Anonymous {
		chain, _, roles, customPerms, rootTier, minBindingExp, policy, chainSeq, err := s.resolveAdmitChain(
			ctx, snap, "", in.Chain, in.AllowDeleted, true, true,
		)
		if err != nil {
			return nil, op.Fail(err)
		}
		out := &Admitted{
			Valid:           true,
			Principal:       principalAnonymous,
			OrgAllowedCIDRs: policy.allowedCIDRs,
			InvalidationSeq: chainSeq,
			ConfigRevision:  configRev,
		}
		if chain == nil {
			op.OK("not found")
			return out, nil
		}
		perms := ResolveWithCustom(snap, roles, customPerms).List()
		out.Nodes = chain
		out.Permissions = perms
		out.RateLimitTier = rootTier
		out.ExpiresAt = minBindingExp
		if len(perms) == 0 {
			op.OK("not public")
			return out, nil
		}
		op.OK("admitted")
		return out, nil
	}

	userRoute := len(in.Chain) == 0

	// 1. Principal.
	p, seq, err := s.admitPrincipal(ctx, in)
	if err != nil {
		return nil, op.Fail(err)
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
		ExpiresAt:       p.expiresAt,
		InvalidationSeq: p.invalidationSeq,
		ConfigRevision:  configRev,
	}
	if userRoute {
		// A person's user id. A service account, or a personal key narrowed by
		// scopes or allowed_roots, gets no user_id and is refused with 403.
		if len(p.scopes) == 0 && len(p.allowedRoots) == 0 {
			out.UserID = p.userID
		}
		op.OK("admitted")
		return out, nil
	}

	// 2–4. Resolve the node chain, the principal's active unsuspended member in
	// its root, the root's rate_limit_tier and security policy, and the
	// unexpired counting roles (predefined + custom + public when allow_public)
	// along the chain in one query.
	chain, memberID, roles, customPerms, rootTier, minBindingExp, policy, chainSeq, err := s.resolveAdmitChain(
		ctx, snap, p.id, in.Chain, in.AllowDeleted, false, in.AllowPublic,
	)
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
	out.OrgAllowedCIDRs = policy.allowedCIDRs

	if len(p.allowedRoots) > 0 && !slices.Contains(p.allowedRoots, chain[0].ID) {
		op.OK("root disallowed")
		return out, nil
	}
	// Enforce per-root auth & security policies.
	if in.Credential != nil && p.kind == principalPerson && !policy.allowPersonalKeys {
		op.OK("personal keys disallowed")
		return out, nil
	}
	if in.IdP != nil && len(policy.requiredIdPClaims) > 0 && !matchesRequiredClaims(in.IdP.PolicyClaims, policy.requiredIdPClaims) {
		op.OK("idp policy denied")
		return out, nil
	}

	resolved := ResolveWithCustom(snap, roles, customPerms)
	var perms []string
	switch {
	case len(p.scopes) > 0:
		perms = IntersectScopes(resolved, p.scopes)
		sc := append([]string(nil), p.scopes...)
		out.CredentialScopes = &sc
	case in.IdP != nil && in.IdP.HasScopes:
		perms = IntersectScopes(resolved, in.IdP.Scopes)
		sc := append([]string(nil), in.IdP.Scopes...)
		if sc == nil {
			sc = []string{}
		}
		out.CredentialScopes = &sc
	default:
		perms = resolved.List()
	}

	if memberID == "" {
		if !in.AllowPublic {
			op.OK("no member")
			return out, nil
		}
		out.Nodes = chain
		out.UserID = p.userID
		out.RateLimitTier = rootTier
		out.ExpiresAt = earliestTime(p.expiresAt, minBindingExp)
		out.Permissions = perms
		if len(perms) == 0 {
			op.OK("no member")
			return out, nil
		}
		op.OK("admitted public")
		return out, nil
	}
	out.Nodes, out.MemberID, out.RateLimitTier = chain, memberID, rootTier
	out.ExpiresAt = earliestTime(p.expiresAt, minBindingExp)
	out.Permissions = perms
	op.OK("admitted")
	return out, nil
}

// checkAdmit refuses a body identity can't read (422). The chain's kinds are
// matched by name only, never against the config: a gateway on another
// revision gets member_id null, not an error.
func checkAdmit(in AdmitInput) error {
	count := 0
	if in.Credential != nil {
		count++
	}
	if in.IdP != nil {
		count++
	}
	if in.Anonymous {
		count++
	}
	if count != 1 {
		return errors.FieldError("credential", "Send exactly one of credential, idp, or anonymous.")
	}
	if in.IdP != nil && (strings.TrimSpace(in.IdP.Issuer) == "" || strings.TrimSpace(in.IdP.Subject) == "") {
		return errors.FieldError("idp", errors.FallbackValidation)
	}
	if in.Anonymous && (len(in.Chain) == 0 || !in.AllowPublic) {
		return errors.FieldError("anonymous", "Anonymous admit requires allow_public on a kind route.")
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
		SELECT p.id, p.kind, p.user_id, k.id, k.allowed_cidrs, k.scopes, k.allowed_roots, k.expires_at,
		       p.status, k.revoked_at IS NOT NULL, bn.status,
		       `+lifecycle.InvalidationHeadSQL+`
		FROM api_keys k
		JOIN principals p ON p.id = k.principal_id
		LEFT JOIN nodes bn ON bn.id = p.node_id
		WHERE k.key_hash = $1
	`, secret.Hash(key)).Scan(
		&p.id, &p.kind, &userID, &p.keyID, &p.allowedCIDRs, &p.scopes, &p.allowedRoots, &p.expiresAt,
		&status, &revoked, &nodeStatus, &p.invalidationSeq,
	)
	if dbx.IsNoRows(err) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, err
	}
	if revoked || (p.expiresAt != nil && !p.expiresAt.After(s.now().UTC())) || !admissible(status, nodeStatus) {
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
		       `+lifecycle.InvalidationHeadSQL+`
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
		if err := s.pool.QueryRow(ctx, `SELECT `+lifecycle.InvalidationHeadSQL).Scan(&seq); err != nil {
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
	anonymous bool,
	allowPublic bool,
) ([]ChainNode, string, []string, []string, *string, *time.Time, rootSecurityPolicy, int64, error) {
	defaultPolicy := rootSecurityPolicy{allowPersonalKeys: true}
	n := len(refs)
	pos := make([]int32, n)
	kinds := make([]string, n)
	refVals := make([]string, n)
	isIDs := make([]bool, n)
	for i, c := range refs {
		isID := id.IsID(c.Ref)
		if isID && !id.Valid(c.Ref) {
			return nil, "", nil, nil, nil, nil, defaultPolicy, 0, nil
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
	now := s.now().UTC()

	var nodeIDs []string
	var memberID *string
	var roles []string
	var customPerms []string
	var rootTier *string
	var minExpiresAt *time.Time
	var allowPersonalKeys *bool
	var orgAllowedCIDRs []string
	var requiredClaimsRaw []byte
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
				SELECT c.pos, c.kind, c.id, (SELECT id FROM chain WHERE pos = 1) AS root_id
				FROM chain c
				WHERE (SELECT count(*) FROM chain) = $5
			),
			member_row AS (
				SELECT m.id, (m.suspended_at IS NOT NULL) AS is_suspended
				FROM members m
				JOIN resolved r ON r.pos = 1 AND m.root_id = r.id
				WHERE NOT $11 AND m.principal_id = $7 AND m.removed_at IS NULL
			),
			policy AS (
				SELECT rsp.allow_personal_keys, rsp.allowed_cidrs, rsp.required_idp_claims
				FROM resolved r
				JOIN root_security_policies rsp ON rsp.root_id = r.id
				WHERE r.pos = 1
			),
			roles AS (
				SELECT b.role, b.expires_at, cr.permissions AS custom_perms
				FROM resolved bn
				JOIN bindings b ON b.node_id = bn.id
				LEFT JOIN org_roles cr ON cr.id = b.role AND cr.root_id = bn.root_id AND bn.kind = ANY(cr.bindable_at)
				WHERE (b.expires_at IS NULL OR b.expires_at > $10)
				  AND `+CountsWithCustomSQL("$8", "$9", "cr")+`
				  AND (
					(NOT $11 AND (
						b.member_id = (SELECT id FROM member_row WHERE NOT is_suspended)
						OR EXISTS (
							SELECT 1 FROM group_members gm
							WHERE gm.group_id = b.group_id AND gm.member_id = (SELECT id FROM member_row WHERE NOT is_suspended)
						)
					))
					OR ($12 AND NOT COALESCE((SELECT is_suspended FROM member_row), false) AND (
						b.public_principal = 'all_users'
						OR (NOT $11 AND b.public_principal = 'all_authenticated_users')
					))
				  )
			)
		SELECT
			(SELECT array_agg(id ORDER BY pos) FROM resolved),
			(SELECT id FROM member_row WHERE NOT is_suspended),
			(SELECT array_agg(DISTINCT role) FROM roles),
			(SELECT array_agg(DISTINCT p) FROM roles, unnest(COALESCE(custom_perms, '{}')) AS p),
			(SELECT n.rate_limit_tier FROM resolved r JOIN nodes n ON n.id = r.id WHERE r.pos = 1),
			(SELECT MIN(expires_at) FROM roles WHERE expires_at IS NOT NULL),
			(SELECT allow_personal_keys FROM policy),
			(SELECT allowed_cidrs FROM policy),
			(SELECT required_idp_claims FROM policy),
			`+lifecycle.InvalidationHeadSQL+`
	`, pos, kinds, refVals, isIDs, n, allowDeleted, principalID, bindKinds, bindRoles, now, anonymous, allowPublic).Scan(
		&nodeIDs, &memberID, &roles, &customPerms, &rootTier, &minExpiresAt,
		&allowPersonalKeys, &orgAllowedCIDRs, &requiredClaimsRaw, &invSeq,
	)
	if err != nil {
		return nil, "", nil, nil, nil, nil, defaultPolicy, 0, err
	}
	if len(nodeIDs) != n {
		return nil, "", nil, nil, nil, nil, defaultPolicy, invSeq, nil
	}
	out := make([]ChainNode, n)
	for i, nodeID := range nodeIDs {
		out[i] = ChainNode{Kind: refs[i].Kind, ID: nodeID}
	}
	policy := defaultPolicy
	if allowPersonalKeys != nil {
		policy.allowPersonalKeys = *allowPersonalKeys
	}
	if len(orgAllowedCIDRs) > 0 {
		policy.allowedCIDRs = orgAllowedCIDRs
	}
	if len(requiredClaimsRaw) > 0 {
		var req map[string][]string
		if err := json.Unmarshal(requiredClaimsRaw, &req); err != nil {
			return nil, "", nil, nil, nil, nil, defaultPolicy, 0, err
		}
		if len(req) > 0 {
			policy.requiredIdPClaims = req
		}
	}
	mID := ""
	if memberID != nil {
		mID = *memberID
	}
	return out, mID, roles, customPerms, rootTier, minExpiresAt, policy, invSeq, nil
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

// heldOnChains is, for each pair, the roles and custom-role permissions of the
// member's unexpired bindings on the chain's nodes that count there (bindable
// at the node's kind): what ResolveWithCustom unions. Evaluations and resource
// search share it.
func heldOnChains(ctx context.Context, q queryer, snap *feed.Snapshot, pairs []memberChain, now time.Time) ([][]string, [][]string, error) {
	var idx []int32
	var memberIDs, nodeIDs []string
	for i, p := range pairs {
		for _, n := range p.nodes {
			idx = append(idx, int32(i))
			memberIDs = append(memberIDs, p.memberID)
			nodeIDs = append(nodeIDs, n.ID)
		}
	}
	outRoles := make([][]string, len(pairs))
	outCustom := make([][]string, len(pairs))
	if len(idx) == 0 {
		return outRoles, outCustom, nil
	}
	kinds, roles := BindablePairs(snap)
	rows, err := q.Query(ctx, `
		SELECT c.i, b.role, COALESCE(cr.permissions, '{}')
		FROM unnest($1::int[], $2::text[], $3::text[]) AS c (i, member_id, node_id)
		JOIN bindings b ON b.node_id = c.node_id AND (
			b.member_id = c.member_id
			OR EXISTS (SELECT 1 FROM group_members gm WHERE gm.group_id = b.group_id AND gm.member_id = c.member_id)
		)
		JOIN nodes bn ON bn.id = b.node_id
		LEFT JOIN org_roles cr ON cr.id = b.role AND cr.root_id = bn.root_id AND bn.kind = ANY(cr.bindable_at)
		WHERE (b.expires_at IS NULL OR b.expires_at > $6)
		  AND `+CountsWithCustomSQL("$4", "$5", "cr"),
		idx, memberIDs, nodeIDs, kinds, roles, now)
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
