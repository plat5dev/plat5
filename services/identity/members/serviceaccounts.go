package members

import (
	"context"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/plat5dev/plat5/identity/errors"
	"github.com/plat5dev/plat5/identity/feed"
	"github.com/plat5dev/plat5/identity/internal/apikey"
	"github.com/plat5dev/plat5/identity/internal/dbx"
	"github.com/plat5dev/plat5/identity/internal/id"
	"github.com/plat5dev/plat5/identity/nodes"
)

// ServiceAccount is a principal born at a node, addressed by its member id.
type ServiceAccount struct {
	ID        string // its member id
	Name      string
	NodeID    string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// serviceAccountSelect reads active service accounts born at $1; where may
// use $2 on.
func serviceAccountSelect(where string) string {
	return `
		SELECT m.id, p.name, p.born_node_id, p.created_at, p.updated_at, p.id
		FROM principals p JOIN members m ON m.principal_id = p.id AND m.removed_at IS NULL
		WHERE p.kind = 'service_account' AND p.born_node_id = $1 AND ` + where
}

func scanServiceAccount(row dbx.Scannable, principalID *string) (*ServiceAccount, error) {
	var sa ServiceAccount
	var name *string
	var pid string
	if err := row.Scan(&sa.ID, &name, &sa.NodeID, &sa.CreatedAt, &sa.UpdatedAt, &pid); err != nil {
		return nil, err
	}
	if name != nil {
		sa.Name = *name
	}
	if principalID != nil {
		*principalID = pid
	}
	return &sa, nil
}

func normalizeName(raw string) (string, error) {
	name := strings.TrimSpace(raw)
	if name == "" {
		return "", errors.FieldError("name", "Name is required.")
	}
	if utf8.RuneCountInString(name) > apikey.MaxNameLen {
		return "", errors.FieldError("name", "Name is too long.")
	}
	return name, nil
}

// CreateServiceAccount makes a service account born at nodeID, in one
// transaction: principal, member in the node's tree, binding at the node.
func (s *Store) CreateServiceAccount(ctx context.Context, snap *feed.Snapshot, nodeID, name, role string) (*ServiceAccount, *Binding, error) {
	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "create_service_account", dbx.DefaultTimeout)
	defer cancel()
	defer op.End()

	name, err := normalizeName(name)
	if err != nil {
		return nil, nil, op.Expected("invalid", err)
	}
	w, err := s.begin(ctx)
	if err != nil {
		return nil, nil, op.Fail(err)
	}
	defer w.rollback(ctx)
	n, err := s.nodes.LockedNode(ctx, w.tx, nodeID)
	if err != nil {
		return nil, nil, op.Reject(err, nodes.ErrNotFound)
	}
	if err := checkRole(snap, role, n.Kind, "role"); err != nil {
		return nil, nil, op.Expected("invalid", err)
	}
	now := s.now().UTC()
	principalID := id.New()
	if _, err := w.tx.Exec(ctx, `
		INSERT INTO principals (id, kind, name, born_node_id, created_at, updated_at)
		VALUES ($1, 'service_account', $2, $3, $4, $4)
	`, principalID, name, n.ID, now); err != nil {
		return nil, nil, op.Fail(err)
	}
	memberID, err := s.insertMember(ctx, w, principalID, PrincipalServiceAccount, n.RootID)
	if err != nil {
		return nil, nil, op.Fail(err)
	}
	b, err := s.insertBinding(ctx, w, memberID, n.ID, role)
	if err != nil {
		return nil, nil, op.Fail(err)
	}
	if err := w.commit(ctx); err != nil {
		return nil, nil, op.Fail(err)
	}
	op.OK("created")
	return &ServiceAccount{ID: memberID, Name: name, NodeID: n.ID, CreatedAt: now, UpdatedAt: now}, b, nil
}

// ListServiceAccounts is the live service accounts born at a node, by member id.
func (s *Store) ListServiceAccounts(ctx context.Context, nodeID string, limit int, startingAfter string) ([]*ServiceAccount, bool, error) {
	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "list_service_accounts", dbx.DefaultTimeout)
	defer cancel()
	defer op.End()
	if _, err := s.nodes.Get(ctx, nodeID); err != nil {
		return nil, false, op.Reject(err, nodes.ErrNotFound)
	}
	rows, err := s.pool.Query(ctx, serviceAccountSelect(`m.id > $2 ORDER BY m.id LIMIT $3`), nodeID, startingAfter, limit+1)
	if err != nil {
		return nil, false, op.Fail(err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (*ServiceAccount, error) { return scanServiceAccount(r, nil) })
	if err != nil {
		return nil, false, op.Fail(err)
	}
	out, hasMore := page(out, limit)
	op.OK("ok")
	return out, hasMore, nil
}

// serviceAccount reads a live service account born at nodeID, and its principal.
func (s *Store) serviceAccount(ctx context.Context, q dbx.Querier, nodeID, memberID string) (*ServiceAccount, string, error) {
	if !id.Valid(nodeID) || !id.Valid(memberID) {
		return nil, "", ErrNotFound
	}
	var principalID string
	sa, err := scanServiceAccount(q.QueryRow(ctx, serviceAccountSelect(`m.id = $2`), nodeID, memberID), &principalID)
	if dbx.IsNoRows(err) {
		return nil, "", ErrNotFound
	}
	return sa, principalID, err
}

func (s *Store) GetServiceAccount(ctx context.Context, nodeID, memberID string) (*ServiceAccount, error) {
	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "get_service_account", dbx.DefaultTimeout)
	defer cancel()
	defer op.End()
	sa, _, err := s.serviceAccount(ctx, s.pool, nodeID, memberID)
	if err != nil {
		return nil, op.Reject(err, ErrNotFound)
	}
	op.OK("ok")
	return sa, nil
}

// RenameServiceAccount sets its name. It returns the name before.
func (s *Store) RenameServiceAccount(ctx context.Context, nodeID, memberID, name string) (string, *ServiceAccount, error) {
	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "rename_service_account", dbx.DefaultTimeout)
	defer cancel()
	defer op.End()
	name, err := normalizeName(name)
	if err != nil {
		return "", nil, op.Expected("invalid", err)
	}
	w, err := s.begin(ctx)
	if err != nil {
		return "", nil, op.Fail(err)
	}
	defer w.rollback(ctx)
	before, principalID, err := s.serviceAccount(ctx, w.tx, nodeID, memberID)
	if err != nil {
		return "", nil, op.Reject(err, ErrNotFound)
	}
	if _, err := w.tx.Exec(ctx, `
		UPDATE principals SET name = $2, updated_at = CASE WHEN name = $2 THEN updated_at ELSE $3 END WHERE id = $1
	`, principalID, name, s.now().UTC()); err != nil {
		return "", nil, op.Fail(err)
	}
	after, _, err := s.serviceAccount(ctx, w.tx, nodeID, memberID)
	if err != nil {
		return "", nil, op.Fail(err)
	}
	if err := w.commit(ctx); err != nil {
		return "", nil, op.Fail(err)
	}
	op.OK("ok")
	return before.Name, after, nil
}

// DeleteServiceAccount ends it as purge does: its member is removed with
// member.removed, and its principal is deleted with its keys. One already
// deleted, in the node's tree, is left as it is: changed is false.
func (s *Store) DeleteServiceAccount(ctx context.Context, nodeID, memberID string) (bool, error) {
	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "delete_service_account", dbx.DefaultTimeout)
	defer cancel()
	defer op.End()

	w, err := s.begin(ctx)
	if err != nil {
		return false, op.Fail(err)
	}
	defer w.rollback(ctx)
	n, err := s.nodes.LockedNode(ctx, w.tx, nodeID)
	if err != nil {
		return false, op.Reject(err, nodes.ErrNotFound)
	}
	_, principalID, err := s.serviceAccount(ctx, w.tx, n.ID, memberID)
	if err == ErrNotFound {
		var gone bool
		if err := w.tx.QueryRow(ctx, `
			SELECT EXISTS (SELECT 1 FROM members WHERE id = $1 AND root_id = $2 AND principal = 'service_account' AND removed_at IS NOT NULL)
		`, memberID, n.RootID).Scan(&gone); err != nil {
			return false, op.Fail(err)
		}
		if gone {
			op.OK("unchanged")
			return false, nil
		}
		return false, op.Expected("not found", ErrNotFound)
	}
	if err != nil {
		return false, op.Fail(err)
	}
	m, err := s.lockedMember(ctx, w, memberID)
	if err != nil {
		return false, op.Fail(err)
	}
	if err := s.removeMember(ctx, w, m); err != nil {
		return false, op.Fail(err)
	}
	if _, err := w.tx.Exec(ctx, `DELETE FROM principals WHERE id = $1`, principalID); err != nil {
		return false, op.Fail(err)
	}
	if err := w.commit(ctx); err != nil {
		return false, op.Fail(err)
	}
	op.OK("deleted")
	return true, nil
}

// --- keys ---

// Key is an API key, on a person or a service account.
type Key struct {
	ID        string
	Name      string
	KeyPrefix string
	CreatedAt time.Time
	RevokedAt *time.Time
	// Secret is set once, on create.
	Secret string
}

const keyColumns = `id, name, key_prefix, created_at, revoked_at`

func scanKey(row dbx.Scannable) (*Key, error) {
	var k Key
	if err := row.Scan(&k.ID, &k.Name, &k.KeyPrefix, &k.CreatedAt, &k.RevokedAt); err != nil {
		return nil, err
	}
	return &k, nil
}

// KeyOwner is whose keys a request addresses: a service account born at a
// node, by its member id, or a person, by their user id.
type KeyOwner struct {
	NodeID   string
	MemberID string
	UserID   string
}

func (s *Store) keyPrincipal(ctx context.Context, q dbx.Querier, owner KeyOwner) (string, error) {
	if owner.UserID != "" {
		u, err := s.user(ctx, q, owner.UserID)
		if err != nil {
			return "", err
		}
		return u.principalID, nil
	}
	_, principalID, err := s.serviceAccount(ctx, q, owner.NodeID, owner.MemberID)
	return principalID, err
}

// ListKeys is the owner's keys, revoked ones included, id ascending.
func (s *Store) ListKeys(ctx context.Context, owner KeyOwner, limit int, startingAfter string) ([]*Key, bool, error) {
	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "list_keys", dbx.DefaultTimeout)
	defer cancel()
	defer op.End()
	principalID, err := s.keyPrincipal(ctx, s.pool, owner)
	if err != nil {
		return nil, false, op.Reject(err, ErrNotFound)
	}
	rows, err := s.pool.Query(ctx, `
		SELECT `+keyColumns+` FROM api_keys WHERE principal_id = $1 AND id > $2 ORDER BY id LIMIT $3
	`, principalID, startingAfter, limit+1)
	if err != nil {
		return nil, false, op.Fail(err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (*Key, error) { return scanKey(r) })
	if err != nil {
		return nil, false, op.Fail(err)
	}
	out, hasMore := page(out, limit)
	op.OK("ok")
	return out, hasMore, nil
}

// CreateKey mints a key for the owner. The secret is returned once and stored
// only as its hash.
func (s *Store) CreateKey(ctx context.Context, owner KeyOwner, name string) (*Key, error) {
	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "create_key", dbx.DefaultTimeout)
	defer cancel()
	defer op.End()
	name = strings.TrimSpace(name)
	if name == "" {
		name = apikey.DefaultName
	}
	if utf8.RuneCountInString(name) > apikey.MaxNameLen {
		return nil, op.Expected("invalid", errors.FieldError("name", "Name is too long."))
	}
	principalID, err := s.keyPrincipal(ctx, s.pool, owner)
	if err != nil {
		return nil, op.Reject(err, ErrNotFound)
	}
	prefix := apikey.Prefix(s.brand)
	secret, err := apikey.Generate(prefix)
	if err != nil {
		return nil, op.Fail(err)
	}
	k, err := scanKey(s.pool.QueryRow(ctx, `
		INSERT INTO api_keys (id, principal_id, name, key_prefix, key_hash, created_at)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING `+keyColumns, id.New(), principalID, name, apikey.DisplayPrefix(secret, prefix), apikey.Hash(secret), s.now().UTC()))
	if dbx.IsForeignKeyViolation(err) {
		return nil, op.Expected("not found", ErrNotFound) // the owner went meanwhile
	}
	if err != nil {
		return nil, op.Fail(err)
	}
	k.Secret = secret
	op.OK("created")
	return k, nil
}

// RevokeKey revokes one of the owner's keys. Revoking a revoked key changes
// nothing: changed is false.
func (s *Store) RevokeKey(ctx context.Context, owner KeyOwner, keyID string) (*Key, bool, error) {
	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "revoke_key", dbx.DefaultTimeout)
	defer cancel()
	defer op.End()
	principalID, err := s.keyPrincipal(ctx, s.pool, owner)
	if err == nil && !id.Valid(keyID) {
		err = ErrNotFound
	}
	if err != nil {
		return nil, false, op.Reject(err, ErrNotFound)
	}
	var k Key
	var changed bool
	err = s.pool.QueryRow(ctx, `
		UPDATE api_keys k SET revoked_at = COALESCE(k.revoked_at, $3)
		FROM (SELECT id, revoked_at FROM api_keys WHERE id = $1 AND principal_id = $2 FOR UPDATE) old
		WHERE k.id = old.id
		RETURNING k.id, k.name, k.key_prefix, k.created_at, k.revoked_at, old.revoked_at IS NULL
	`, keyID, principalID, s.now().UTC()).Scan(&k.ID, &k.Name, &k.KeyPrefix, &k.CreatedAt, &k.RevokedAt, &changed)
	if dbx.IsNoRows(err) {
		return nil, false, op.Expected("not found", ErrNotFound)
	}
	if err != nil {
		return nil, false, op.Fail(err)
	}
	op.OK("ok")
	return &k, changed, nil
}
