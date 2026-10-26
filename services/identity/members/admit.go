package members

import (
	"context"
	"strings"

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
// An empty chain is a user route.
type AdmitInput struct {
	Credential   *string
	IdP          *IdPClaims
	Chain        []ChainRef
	AllowDeleted bool
}

// IdPClaims is what the gateway read from a verified JWT.
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
	Valid       bool
	Principal   string
	KeyID       string
	Nodes       []ChainNode
	MemberID    string
	UserID      string
	Permissions []string
}

// admitPrincipal is the principal behind a credential.
type admitPrincipal struct {
	id, kind, userID, keyID string
}

// Admit turns a credential and a node chain into the member and permissions
// for one request, in the steps of the contract. It writes only for a new IdP
// subject (the person) or a changed name.
func (s *Store) Admit(ctx context.Context, snap *feed.Snapshot, in AdmitInput) (*Admitted, error) {
	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "admit", dbx.DefaultTimeout)
	defer cancel()
	defer op.End()

	if err := checkAdmit(in); err != nil {
		return nil, op.Expected("invalid", err)
	}
	userRoute := len(in.Chain) == 0

	// 1. Principal.
	p, err := s.admitPrincipal(ctx, in)
	if err != nil {
		return nil, op.Fail(err)
	}
	if p == nil || (userRoute && p.kind == PrincipalServiceAccount) {
		op.OK("invalid")
		return &Admitted{}, nil
	}
	out := &Admitted{Valid: true, Principal: p.kind, KeyID: p.keyID}
	if userRoute {
		out.UserID = p.userID
		op.OK("admitted")
		return out, nil
	}

	// 2. Nodes.
	chain, err := s.resolveChain(ctx, in.Chain, in.AllowDeleted)
	if err != nil {
		return nil, op.Fail(err)
	}
	if chain == nil {
		op.OK("not found")
		return out, nil
	}

	// 3. Member.
	memberID, err := activeMemberID(ctx, s.pool, p.id, chain[0].ID)
	if err != nil {
		return nil, op.Fail(err)
	}
	if memberID == "" {
		op.OK("no member")
		return out, nil
	}
	out.Nodes, out.MemberID = chain, memberID

	// 4. Permissions.
	if out.Permissions, err = s.chainPermissions(ctx, snap, out); err != nil {
		return nil, op.Fail(err)
	}
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
	var userID *string
	var status string
	var revoked bool
	var nodeStatus *string
	err := s.pool.QueryRow(ctx, `
		SELECT p.id, p.kind, p.user_id, k.id, p.status, k.revoked_at IS NOT NULL, bn.status
		FROM api_keys k
		JOIN principals p ON p.id = k.principal_id
		LEFT JOIN nodes bn ON bn.id = p.node_id
		WHERE k.key_hash = $1
	`, apikey.Hash(key)).Scan(&p.id, &p.kind, &userID, &p.keyID, &status, &revoked, &nodeStatus)
	if dbx.IsNoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if revoked || status != StatusActive || (nodeStatus != nil && *nodeStatus != nodes.StatusActive) {
		return nil, nil
	}
	if userID != nil {
		p.userID = *userID
	}
	return &p, nil
}

func (s *Store) idpPrincipal(ctx context.Context, c IdPClaims) (*admitPrincipal, error) {
	issuer, subject := strings.TrimSpace(c.Issuer), strings.TrimSpace(c.Subject)
	var name string
	if c.Name != nil {
		name = strings.TrimSpace(*c.Name)
	}
	var principalID, userID, status string
	err := s.pool.QueryRow(ctx, `
		SELECT p.id, p.user_id, p.status FROM idp_links l JOIN principals p ON p.id = l.principal_id
		WHERE l.issuer = $1 AND l.subject = $2
	`, issuer, subject).Scan(&principalID, &userID, &status)
	switch {
	case dbx.IsNoRows(err):
		if principalID, userID, status, err = s.firstSignIn(ctx, issuer, subject, name); err != nil {
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
	return &admitPrincipal{id: principalID, kind: PrincipalPerson, userID: userID}, nil
}

// firstSignIn creates the person behind a new IdP subject. A concurrent
// admit for the same subject finds the link once it has the subject's lock.
// It returns the principal, its user id and its status.
func (s *Store) firstSignIn(ctx context.Context, issuer, subject, name string) (string, string, string, error) {
	w, err := s.begin(ctx)
	if err != nil {
		return "", "", "", err
	}
	defer w.rollback(ctx)
	principalID, err := s.ensurePerson(ctx, w, issuer, subject)
	if err != nil {
		return "", "", "", err
	}
	var userID, status string
	if err := w.tx.QueryRow(ctx, `
		UPDATE principals SET name = COALESCE(NULLIF($2, ''), name) WHERE id = $1 RETURNING user_id, status
	`, principalID, name).Scan(&userID, &status); err != nil {
		return "", "", "", err
	}
	return principalID, userID, status, w.commit(ctx)
}

// resolveChain resolves each ref under the previous node, by kind; the first
// is a root. Nil when a node is missing, purged, or deleted, except a deleted
// last node with allowDeleted.
func (s *Store) resolveChain(ctx context.Context, chain []ChainRef, allowDeleted bool) ([]ChainNode, error) {
	out := make([]ChainNode, 0, len(chain))
	parent := ""
	for i, c := range chain {
		var nodeID, status string
		var err error
		if id.IsID(c.Ref) {
			if !id.Valid(c.Ref) {
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
		if status != nodes.StatusActive && !(last && allowDeleted && status == nodes.StatusDeleted) {
			return nil, nil
		}
		out = append(out, ChainNode{Kind: c.Kind, ID: nodeID})
		parent = nodeID
	}
	return out, nil
}

// chainPermissions is the union of the roles of the member's bindings that
// count on the chain's nodes (docs/concepts/access.md#resolution). The chain
// is live but for an allow_deleted last node, whose own bindings count.
func (s *Store) chainPermissions(ctx context.Context, snap *feed.Snapshot, out *Admitted) ([]string, error) {
	nodeIDs := make([]string, len(out.Nodes))
	for i, n := range out.Nodes {
		nodeIDs[i] = n.ID
	}
	kinds, roles := bindablePairs(snap)
	rows, err := s.pool.Query(ctx, `
		SELECT b.role
		FROM bindings b JOIN nodes bn ON bn.id = b.node_id
		WHERE b.member_id = $1 AND b.node_id = ANY($2) AND `+counts("$3", "$4"),
		out.MemberID, nodeIDs, kinds, roles)
	if err != nil {
		return nil, err
	}
	held, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	return Resolve(snap, held).List(), nil
}
