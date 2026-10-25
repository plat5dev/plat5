package members

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/plat5dev/plat5/identity/errors"
	"github.com/plat5dev/plat5/identity/feed"
	"github.com/plat5dev/plat5/identity/internal/apikey"
	"github.com/plat5dev/plat5/identity/internal/dbx"
	"github.com/plat5dev/plat5/identity/internal/id"
	"github.com/plat5dev/plat5/identity/nodes"
)

const (
	InviteActive   = "active"
	InviteRedeemed = "redeemed"
	InviteRevoked  = "revoked"
	InviteExpired  = "expired"

	tokenPrefix      = "inv_"
	minInviteExpiry  = 60 * time.Second
	maxInviteExpiry  = 30 * 24 * time.Hour
	tokenRandomBytes = 32
)

// DefaultInviteExpiry is how long a token works when expires_in is omitted.
const DefaultInviteExpiry = 7 * 24 * time.Hour

// Grant is a binding below the root an invite to the root makes on redeem.
type Grant struct {
	NodeID string `json:"node_id"`
	Role   string `json:"role"`
}

// Invite is a join token for a node. Below a root it can admit a guest.
type Invite struct {
	ID          string
	NodeID      string
	Role        string
	Grants      []Grant
	MaxUses     *int // nil: unlimited
	UseCount    int
	Status      string
	ExpiresAt   time.Time
	TokenPrefix string
	CreatedAt   time.Time
	// Token is set once, on create.
	Token string
}

const inviteColumns = `id, node_id, role, grants, max_uses, use_count, status, expires_at, token_prefix, created_at`

// scanInvite reads an invite, with an active one past its expiry read as
// expired: the sweeper writes that later.
func (s *Store) scanInvite(row dbx.Scannable) (*Invite, error) {
	var inv Invite
	var grants []byte
	if err := row.Scan(&inv.ID, &inv.NodeID, &inv.Role, &grants, &inv.MaxUses, &inv.UseCount,
		&inv.Status, &inv.ExpiresAt, &inv.TokenPrefix, &inv.CreatedAt); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(grants, &inv.Grants); err != nil {
		return nil, err
	}
	if inv.Grants == nil {
		inv.Grants = []Grant{}
	}
	if inv.Status == InviteActive && !inv.ExpiresAt.After(s.now()) {
		inv.Status = InviteExpired
	}
	return &inv, nil
}

// InviteInput is POST /nodes/{node_id}/invites, read.
type InviteInput struct {
	Role      string
	Grants    []Grant
	MaxUses   *int // nil: unlimited
	ExpiresIn time.Duration
}

// CreateInvite makes an invite at nodeID. The token is in the result once.
func (s *Store) CreateInvite(ctx context.Context, snap *feed.Snapshot, nodeID string, in InviteInput) (*Invite, error) {
	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "create_invite", dbx.DefaultTimeout)
	defer cancel()
	defer op.End()

	if in.ExpiresIn < minInviteExpiry || in.ExpiresIn > maxInviteExpiry {
		return nil, op.Expected("invalid", ExpiresInError())
	}
	if in.MaxUses != nil && *in.MaxUses < 1 {
		return nil, op.Expected("invalid", MaxUsesError())
	}

	w, err := s.begin(ctx)
	if err != nil {
		return nil, op.Fail(err)
	}
	defer w.rollback(ctx)
	n, err := s.nodes.LockedNode(ctx, w.tx, nodeID)
	if err != nil {
		return nil, op.Reject(err, nodes.ErrNotFound)
	}
	if err := checkRole(snap, in.Role, n.Kind, "role"); err != nil {
		return nil, op.Expected("invalid", err)
	}
	grants, err := s.checkGrants(ctx, w, snap, n, in.Grants)
	if err != nil {
		return nil, op.Reject(err)
	}
	grantsJSON, err := json.Marshal(grants)
	if err != nil {
		return nil, op.Fail(err)
	}

	token, err := newToken()
	if err != nil {
		return nil, op.Fail(err)
	}
	now := s.now().UTC()
	inv, err := s.scanInvite(w.tx.QueryRow(ctx, `
		INSERT INTO invites (id, node_id, role, grants, token_hash, token_prefix, status, max_uses, expires_at, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, 'active', $7, $8, $9)
		RETURNING `+inviteColumns,
		id.New(), n.ID, in.Role, grantsJSON, apikey.Hash(token),
		apikey.DisplayPrefix(token, tokenPrefix), in.MaxUses, now.Add(in.ExpiresIn), now))
	if err != nil {
		return nil, op.Fail(err)
	}
	if err := w.commit(ctx); err != nil {
		return nil, op.Fail(err)
	}
	inv.Token = token
	op.OK("created")
	return inv, nil
}

// ExpiresInError is the 422 for an expires_in out of range or unreadable.
func ExpiresInError() error {
	return errors.FieldError("expires_in", "Expiry must be between 60 seconds and 30 days.")
}

// MaxUsesError is the 422 for a max_uses that's neither a positive number nor
// unlimited.
func MaxUsesError() error {
	return errors.FieldError("max_uses", "Max uses must be at least 1, or unlimited.")
}

func newToken() (string, error) {
	buf := make([]byte, tokenRandomBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return tokenPrefix + base64.RawURLEncoding.EncodeToString(buf), nil
}

// checkGrants checks an invite's grants: only on a root, each on a distinct
// live node below it, with a role bindable there.
func (s *Store) checkGrants(ctx context.Context, w *write, snap *feed.Snapshot, n *nodes.Node, in []Grant) ([]Grant, error) {
	out := []Grant{}
	if len(in) == 0 {
		return out, nil
	}
	if n.ParentID != nil {
		var rootKind string
		if err := w.tx.QueryRow(ctx, `SELECT kind FROM nodes WHERE id = $1`, n.RootID).Scan(&rootKind); err != nil {
			return nil, err
		}
		return nil, errors.FieldError("grants", fmt.Sprintf("Only an invite to the %s can grant more.", rootKind))
	}
	seen := map[string]bool{}
	for i, g := range in {
		field := fmt.Sprintf("grants[%d]", i)
		badNode := errors.FieldError(field+".node_id", errors.FallbackValidation)
		if !id.Valid(g.NodeID) || seen[g.NodeID] {
			return nil, badNode
		}
		var kind string
		err := w.tx.QueryRow(ctx, `
			SELECT kind FROM nodes WHERE id = $1 AND root_id = $2 AND id <> $2 AND status = 'active'
		`, g.NodeID, n.ID).Scan(&kind)
		if dbx.IsNoRows(err) {
			return nil, badNode
		}
		if err != nil {
			return nil, err
		}
		seen[g.NodeID] = true
		if err := checkRole(snap, g.Role, kind, field+".role"); err != nil {
			return nil, err
		}
		out = append(out, Grant{NodeID: g.NodeID, Role: g.Role})
	}
	return out, nil
}

// ListInvites is the invites at a node, id ascending. No tokens.
func (s *Store) ListInvites(ctx context.Context, nodeID string, limit int, startingAfter string) ([]*Invite, bool, error) {
	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "list_invites", dbx.DefaultTimeout)
	defer cancel()
	defer op.End()
	if _, err := s.nodes.Get(ctx, nodeID); err != nil {
		return nil, false, op.Reject(err, nodes.ErrNotFound)
	}
	rows, err := s.pool.Query(ctx, `
		SELECT `+inviteColumns+` FROM invites WHERE node_id = $1 AND id > $2 ORDER BY id LIMIT $3
	`, nodeID, startingAfter, limit+1)
	if err != nil {
		return nil, false, op.Fail(err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (*Invite, error) { return s.scanInvite(r) })
	if err != nil {
		return nil, false, op.Fail(err)
	}
	out, hasMore := page(out, limit)
	op.OK("ok")
	return out, hasMore, nil
}

// RevokeInvite revokes an active invite. Any other is left as it is: changed
// is false.
func (s *Store) RevokeInvite(ctx context.Context, nodeID, inviteID string) (bool, error) {
	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "revoke_invite", dbx.DefaultTimeout)
	defer cancel()
	defer op.End()
	if !id.Valid(inviteID) {
		return false, op.Expected("not found", ErrNotFound)
	}
	var exists, changed bool
	err := s.pool.QueryRow(ctx, `
		WITH target AS (SELECT id FROM invites WHERE id = $1 AND node_id = $2),
		     revoked AS (
		         UPDATE invites SET status = 'revoked'
		         WHERE id IN (SELECT id FROM target) AND status = 'active' AND expires_at > $3
		         RETURNING id)
		SELECT EXISTS (SELECT 1 FROM target), EXISTS (SELECT 1 FROM revoked)
	`, inviteID, nodeID, s.now().UTC()).Scan(&exists, &changed)
	if err != nil {
		return false, op.Fail(err)
	}
	if !exists {
		return false, op.Expected("not found", ErrNotFound)
	}
	op.OK("ok")
	return changed, nil
}

// ExpireInvites marks active invites past their expiry expired. Reads already
// show them so; this writes it down. It returns how many it marked.
func (s *Store) ExpireInvites(ctx context.Context) (int, error) {
	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "expire_invites", dbx.DefaultTimeout)
	defer cancel()
	defer op.End()
	tag, err := s.pool.Exec(ctx, `UPDATE invites SET status = 'expired' WHERE status = 'active' AND expires_at <= $1`, s.now().UTC())
	if err != nil {
		return 0, op.Fail(err)
	}
	op.OK("ok")
	return int(tag.RowsAffected()), nil
}

// Redeemed is what a redeem did: the member in the invite's tree, and the
// node it was for. Binding is nil when nothing changed.
type Redeemed struct {
	MemberID      string
	Node          *nodes.Node
	MemberCreated bool
	Binding       *Binding
}

// Redeem uses an invite for a person, by user id. At a root it makes a member
// with the invite's role plus each grant on a node still live; below a root
// it binds the role there, making a guest of someone not in the tree. Someone
// already bound on the invite's node changes nothing, and that isn't a use.
func (s *Store) Redeem(ctx context.Context, userID, token string) (*Redeemed, error) {
	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "redeem_invite", dbx.DefaultTimeout)
	defer cancel()
	defer op.End()

	u, err := s.user(ctx, s.pool, userID)
	if err != nil {
		return nil, op.Reject(err, ErrNotFound)
	}
	hash := apikey.Hash(token)
	var nodeID string
	err = s.pool.QueryRow(ctx, `SELECT node_id FROM invites WHERE token_hash = $1`, hash).Scan(&nodeID)
	if dbx.IsNoRows(err) {
		return nil, op.Expected("unknown token", errors.NotFoundError("invite", nil))
	}
	if err != nil {
		return nil, op.Fail(err)
	}

	w, err := s.begin(ctx)
	if err != nil {
		return nil, op.Fail(err)
	}
	defer w.rollback(ctx)
	n, err := s.nodes.LockedNode(ctx, w.tx, nodeID)
	if err != nil {
		return nil, op.Fail(err)
	}
	inv, err := s.scanInvite(w.tx.QueryRow(ctx, `SELECT `+inviteColumns+` FROM invites WHERE token_hash = $1 FOR UPDATE`, hash))
	if dbx.IsNoRows(err) {
		return nil, op.Expected("unknown token", errors.NotFoundError("invite", nil)) // purged meanwhile
	}
	if err != nil {
		return nil, op.Fail(err)
	}
	if inv.Status != InviteActive {
		return nil, op.Expected("unusable", inviteUnusable(inv.Status))
	}
	if n.Status != nodes.StatusActive {
		return nil, op.Expected("node deleted", errors.ConflictError(fmt.Sprintf("This invite's %s was deleted.", n.Kind), "node", nodes.StatusDeleted))
	}

	out := &Redeemed{Node: n}
	if out.MemberID, err = activeMemberID(ctx, w.tx, u.principalID, n.RootID); err != nil {
		return nil, op.Fail(err)
	}
	if out.MemberID == "" {
		if out.MemberID, err = s.insertMember(ctx, w, u.principalID, PrincipalPerson, n.RootID); err != nil {
			return nil, op.Fail(err)
		}
		out.MemberCreated = true
	}
	var held bool
	if err := w.tx.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM bindings WHERE member_id = $1 AND node_id = $2)
	`, out.MemberID, n.ID).Scan(&held); err != nil {
		return nil, op.Fail(err)
	}
	if held {
		if err := w.commit(ctx); err != nil {
			return nil, op.Fail(err)
		}
		op.OK("unchanged")
		return out, nil
	}
	if out.Binding, err = s.insertBinding(ctx, w, out.MemberID, n.ID, inv.Role); err != nil {
		return nil, op.Fail(err)
	}
	if err := s.applyGrants(ctx, w, out.MemberID, inv.Grants); err != nil {
		return nil, op.Fail(err)
	}
	if _, err := w.tx.Exec(ctx, `
		UPDATE invites SET use_count = use_count + 1,
		       status = CASE WHEN max_uses IS NOT NULL AND use_count + 1 >= max_uses THEN 'redeemed' ELSE status END
		WHERE id = $1
	`, inv.ID); err != nil {
		return nil, op.Fail(err)
	}
	if err := w.commit(ctx); err != nil {
		return nil, op.Fail(err)
	}
	op.OK("redeemed")
	return out, nil
}

// applyGrants binds each grant on a node still live where the member has no
// binding yet. Roles were checked when the invite was made; one removed since
// binds and grants nothing, like any binding whose role is gone.
func (s *Store) applyGrants(ctx context.Context, w *write, memberID string, grants []Grant) error {
	for _, g := range grants {
		var live, held bool
		if err := w.tx.QueryRow(ctx, `
			SELECT EXISTS (SELECT 1 FROM nodes WHERE id = $1 AND status = 'active'),
			       EXISTS (SELECT 1 FROM bindings WHERE member_id = $2 AND node_id = $1)
		`, g.NodeID, memberID).Scan(&live, &held); err != nil {
			return err
		}
		if !live || held {
			continue
		}
		if _, err := s.insertBinding(ctx, w, memberID, g.NodeID, g.Role); err != nil {
			return err
		}
	}
	return nil
}

func inviteUnusable(status string) error {
	msg := map[string]string{
		InviteRedeemed: "This invite has already been used.",
		InviteRevoked:  "This invite is no longer valid.",
		InviteExpired:  "This invite has expired.",
	}[status]
	return errors.ConflictError(msg, "status", status)
}
