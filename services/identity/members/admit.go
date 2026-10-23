package members

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/plat5dev/plat5/identity/errors"
	"github.com/plat5dev/plat5/identity/feed"
	"github.com/plat5dev/plat5/identity/internal/apikey"
	"github.com/plat5dev/plat5/identity/internal/dbx"
	"github.com/plat5dev/plat5/identity/internal/id"
	"github.com/plat5dev/plat5/identity/nodes"
)

// AdmitInput is POST /internal/admit (docs/reference/api/internal.md#admit):
// a credential, or the IdP's issuer and subject, and the route's node chain.
type AdmitInput struct {
	Credential     *string
	IdP            *IdPClaims
	Scope          string
	Chain          []ChainRef
	ResolveDeleted bool
}

// IdPClaims is what the gateway read from a verified JWT.
type IdPClaims struct {
	Issuer, Subject string
	Name            *string
}

// ChainRef is one chain param from the path: a slug, or an id when it holds _.
type ChainRef struct {
	Kind, Ref string
}

// ChainNode is a resolved chain node.
type ChainNode struct {
	Kind, ID string
}

// Admitted is admit's answer. MemberID empty means no member in the tree.
type Admitted struct {
	Valid     bool
	Found     bool
	Principal string
	Nodes     []ChainNode
	MemberID  string
	Labels    []string
	GoodUntil *time.Time
}

var invalid = &Admitted{}

// admitPrincipal is the principal behind a credential.
type admitPrincipal struct {
	id, kind string
}

// Admit turns a credential and a node chain into the member and labels for
// one request, in the five steps of the contract. It writes only for a new
// IdP subject (the person and their directory member) or a changed name.
func (s *Store) Admit(ctx context.Context, snap *feed.Snapshot, in AdmitInput) (*Admitted, error) {
	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "admit", dbx.DefaultTimeout)
	defer cancel()
	defer op.End()

	signIn := in.Scope == snap.SignInKind().Name
	if err := checkAdmit(in, signIn); err != nil {
		return nil, op.Expected("invalid", err)
	}

	// 1. Principal.
	p, err := s.admitPrincipal(ctx, in)
	if err != nil {
		return nil, op.Fail(err)
	}
	if p == nil || (signIn && p.kind == PrincipalServiceAccount) {
		op.OK("invalid")
		return invalid, nil
	}
	out := &Admitted{Valid: true, Found: true, Principal: p.kind}

	if signIn {
		memberID, err := activeMemberID(ctx, s.pool, p.id, s.signInID)
		if err != nil {
			return nil, op.Fail(err)
		}
		out.MemberID = memberID
		op.OK("admitted")
		return out, nil
	}

	// 2. Nodes.
	out.Nodes, err = s.resolveChain(ctx, in.Chain, in.ResolveDeleted)
	if err != nil {
		return nil, op.Fail(err)
	}
	if out.Nodes == nil {
		op.OK("not found")
		return &Admitted{Valid: true, Principal: p.kind}, nil
	}

	// 3. Member.
	out.MemberID, err = activeMemberID(ctx, s.pool, p.id, out.Nodes[0].ID)
	if err != nil {
		return nil, op.Fail(err)
	}
	if out.MemberID == "" {
		op.OK("no member")
		return out, nil
	}

	// 4 and 5. Labels and good until.
	if err := s.chainLabels(ctx, snap, out); err != nil {
		return nil, op.Fail(err)
	}
	op.OK("admitted")
	return out, nil
}

// checkAdmit refuses a body identity can't read (422). The chain's kinds are
// matched by name only, never against the config: a gateway on another
// revision gets found: false, not an error.
func checkAdmit(in AdmitInput, signIn bool) error {
	if (in.Credential == nil) == (in.IdP == nil) {
		return errors.FieldError("credential", "Send a credential or idp.")
	}
	if in.IdP != nil && (strings.TrimSpace(in.IdP.Issuer) == "" || strings.TrimSpace(in.IdP.Subject) == "") {
		return errors.FieldError("idp", errors.FallbackValidation)
	}
	if in.Scope == "" || in.Scope == "public" {
		return errors.FieldError("scope", errors.FallbackValidation)
	}
	if signIn {
		if len(in.Chain) > 0 {
			return errors.FieldError("chain", "A sign-in route has no chain.")
		}
		return nil
	}
	if len(in.Chain) == 0 || in.Chain[len(in.Chain)-1].Kind != in.Scope {
		return errors.FieldError("chain", "The chain ends at the route's scope.")
	}
	for _, c := range in.Chain {
		if c.Kind == "" {
			return errors.FieldError("chain", errors.FallbackValidation)
		}
	}
	return nil
}

// admitPrincipal finds the principal, or nil when the credential is invalid:
// an unknown or revoked key, a suspended principal, or a service account
// whose born node is deleted. A new IdP subject becomes a person.
func (s *Store) admitPrincipal(ctx context.Context, in AdmitInput) (*admitPrincipal, error) {
	if in.Credential != nil {
		return s.keyPrincipalFor(ctx, *in.Credential)
	}
	return s.idpPrincipal(ctx, *in.IdP)
}

func (s *Store) keyPrincipalFor(ctx context.Context, key string) (*admitPrincipal, error) {
	if _, ok := apikey.Parse(s.brand, key); !ok {
		return nil, nil
	}
	var p admitPrincipal
	var status string
	var revoked bool
	var bornStatus *string
	err := s.pool.QueryRow(ctx, `
		SELECT p.id, p.kind, p.status, k.revoked_at IS NOT NULL, bn.status
		FROM api_keys k
		JOIN principals p ON p.id = k.principal_id
		LEFT JOIN nodes bn ON bn.id = p.born_node_id
		WHERE k.key_hash = $1
	`, apikey.Hash(key)).Scan(&p.id, &p.kind, &status, &revoked, &bornStatus)
	if dbx.IsNoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if revoked || status != StatusActive || (bornStatus != nil && *bornStatus != nodes.StatusActive) {
		return nil, nil
	}
	return &p, nil
}

func (s *Store) idpPrincipal(ctx context.Context, c IdPClaims) (*admitPrincipal, error) {
	issuer, subject := strings.TrimSpace(c.Issuer), strings.TrimSpace(c.Subject)
	var name string
	if c.Name != nil {
		name = strings.TrimSpace(*c.Name)
	}
	var principalID, status string
	err := s.pool.QueryRow(ctx, `
		SELECT p.id, p.status FROM idp_links l JOIN principals p ON p.id = l.principal_id
		WHERE l.issuer = $1 AND l.subject = $2
	`, issuer, subject).Scan(&principalID, &status)
	switch {
	case dbx.IsNoRows(err):
		if principalID, status, err = s.firstSignIn(ctx, issuer, subject, name); err != nil {
			return nil, err
		}
	case err != nil:
		return nil, err
	case name != "":
		// A missing or empty claim leaves the name as it was (docs/reference/idp.md).
		if _, err := s.pool.Exec(ctx, `
			UPDATE principals SET name = $2, updated_at = $3 WHERE id = $1 AND name IS DISTINCT FROM $2
		`, principalID, name, s.now().UTC()); err != nil {
			return nil, err
		}
	}
	if status != StatusActive {
		return nil, nil
	}
	return &admitPrincipal{id: principalID, kind: PrincipalPerson}, nil
}

// firstSignIn creates the person behind a new IdP subject, with their
// directory member, under the sign-in tree's lock. A concurrent admit for the
// same subject finds the link once it has the lock. It returns the
// principal and its status.
func (s *Store) firstSignIn(ctx context.Context, issuer, subject, name string) (string, string, error) {
	w, err := s.begin(ctx)
	if err != nil {
		return "", "", err
	}
	defer w.rollback(ctx)
	if err := nodes.LockTrees(ctx, w.tx, []string{s.signInID}); err != nil {
		return "", "", err
	}
	principalID, err := s.ensurePerson(ctx, w, issuer, subject)
	if err != nil {
		return "", "", err
	}
	var status string
	if err := w.tx.QueryRow(ctx, `
		UPDATE principals SET name = COALESCE(NULLIF($2, ''), name) WHERE id = $1 RETURNING status
	`, principalID, name).Scan(&status); err != nil {
		return "", "", err
	}
	return principalID, status, w.commit(ctx)
}

// resolveChain resolves each ref under the previous node, by kind; the first
// is a root. Nil when a node is missing, purged, or deleted, except a deleted
// last node with resolveDeleted.
func (s *Store) resolveChain(ctx context.Context, chain []ChainRef, resolveDeleted bool) ([]ChainNode, error) {
	out := make([]ChainNode, 0, len(chain))
	parent := ""
	for i, c := range chain {
		var nodeID, status string
		var err error
		if strings.Contains(c.Ref, "_") {
			if !id.Valid(id.Node, c.Ref) {
				return nil, nil
			}
			err = s.pool.QueryRow(ctx, `
				SELECT id, status FROM nodes WHERE id = $1 AND kind = $2 AND COALESCE(parent_id, '') = $3
			`, c.Ref, c.Kind, parent).Scan(&nodeID, &status)
		} else {
			err = s.pool.QueryRow(ctx, `
				SELECT id, status FROM nodes WHERE COALESCE(parent_id, '') = $3 AND kind = $2 AND slug = $1
			`, c.Ref, c.Kind, parent).Scan(&nodeID, &status)
		}
		if dbx.IsNoRows(err) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		last := i == len(chain)-1
		if status != nodes.StatusActive && !(last && resolveDeleted && status == nodes.StatusDeleted) {
			return nil, nil
		}
		out = append(out, ChainNode{Kind: c.Kind, ID: nodeID})
		parent = nodeID
	}
	return out, nil
}

// chainLabels sets the union of the roles of the member's bindings that count
// on the chain's nodes (docs/concepts/access.md#resolution), and the earliest
// expiry among them. The chain is live but for a resolve_deleted last node,
// whose own bindings count.
func (s *Store) chainLabels(ctx context.Context, snap *feed.Snapshot, out *Admitted) error {
	nodeIDs := make([]string, len(out.Nodes))
	for i, n := range out.Nodes {
		nodeIDs[i] = n.ID
	}
	kinds, roles := bindablePairs(snap)
	rows, err := s.pool.Query(ctx, `
		SELECT b.role, b.expires_at
		FROM bindings b JOIN nodes bn ON bn.id = b.node_id
		WHERE b.member_id = $1 AND b.node_id = ANY($2) AND `+counts("$3", "$4", "$5"),
		out.MemberID, nodeIDs, s.now().UTC(), kinds, roles)
	if err != nil {
		return err
	}
	var held []string
	var role string
	var expiresAt *time.Time
	if _, err := pgx.ForEachRow(rows, []any{&role, &expiresAt}, func() error {
		held = append(held, role)
		if expiresAt != nil && (out.GoodUntil == nil || expiresAt.Before(*out.GoodUntil)) {
			t := *expiresAt
			out.GoodUntil = &t
		}
		return nil
	}); err != nil {
		return err
	}
	out.Labels = Resolve(snap, held).List()
	return nil
}

// List is the labels, sorted, with * as written.
func (l Labels) List() []string {
	out := make([]string, 0, len(l.Set))
	for label := range l.Set {
		out = append(out, label)
	}
	sort.Strings(out)
	return out
}
