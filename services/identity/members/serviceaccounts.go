package members

import (
	"context"
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

// ServiceAccount is a principal that belongs to a node, addressed by its member id.
type ServiceAccount struct {
	ID        string // its member id
	Name      string
	NodeID    string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// serviceAccountSelect reads active service accounts that belong to $1; where may
// use $2 on.
func serviceAccountSelect(where string) string {
	return `
		SELECT m.id, p.name, p.node_id, p.created_at, p.updated_at, p.id
		FROM principals p JOIN members m ON m.principal_id = p.id AND m.removed_at IS NULL
		WHERE p.kind = 'service_account' AND p.node_id = $1 AND ` + where
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
	if utf8.RuneCountInString(name) > secret.MaxNameLen {
		return "", errors.FieldError("name", "Name is too long.")
	}
	return name, nil
}

// CreateServiceAccount makes a service account that belongs to nodeID, in one
// transaction: principal, member in the node's tree, and (in built-in RBAC
// mode) a binding at the node.
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
	n, err := s.nodes.LockedWritable(ctx, w.tx, nodeID)
	if err != nil {
		return nil, nil, op.Reject(err, nodes.ErrNotFound)
	}
	if snap.ExternalAuthz() {
		if strings.TrimSpace(role) != "" {
			return nil, nil, op.Expected("invalid", errors.FieldError("role", "Roles are managed by the external authorizer."))
		}
	} else {
		if err := checkRoleTx(ctx, w.tx, snap, n.RootID, role, n.Kind, "role"); err != nil {
			return nil, nil, op.Expected("invalid", err)
		}
	}
	now := s.now().UTC()
	principalID := id.New()
	if _, err := w.tx.Exec(ctx, `
		INSERT INTO principals (id, kind, name, node_id, created_at, updated_at)
		VALUES ($1, 'service_account', $2, $3, $4, $4)
	`, principalID, name, n.ID, now); err != nil {
		return nil, nil, op.Fail(err)
	}
	memberID, err := s.insertMember(ctx, w, principalID, PrincipalServiceAccount, n.RootID)
	if err != nil {
		return nil, nil, op.Fail(err)
	}
	var b *Binding
	if !snap.ExternalAuthz() {
		b, err = s.insertBinding(ctx, w, memberID, n.ID, role, nil)
		if err != nil {
			return nil, nil, op.Fail(err)
		}
	}
	if err := w.commit(ctx); err != nil {
		return nil, nil, op.Fail(err)
	}
	op.OK("created")
	return &ServiceAccount{ID: memberID, Name: name, NodeID: n.ID, CreatedAt: now, UpdatedAt: now}, b, nil
}

// ListServiceAccounts is the live service accounts that belong to a node, by member id.
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
	out, hasMore := dbx.Page(out, limit)
	op.OK("ok")
	return out, hasMore, nil
}

// serviceAccount reads a live service account that belongs to nodeID, and its principal.
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
	if _, err := s.nodes.LockedWritable(ctx, w.tx, nodeID); err != nil {
		return "", nil, op.Reject(err, nodes.ErrNotFound)
	}
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
// deleted, in the node's tree, is left as it is: changed is false. Returns the
// member's status prior to deletion when changed is true.
func (s *Store) DeleteServiceAccount(ctx context.Context, nodeID, memberID string) (string, bool, error) {
	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "delete_service_account", dbx.DefaultTimeout)
	defer cancel()
	defer op.End()

	w, err := s.begin(ctx)
	if err != nil {
		return "", false, op.Fail(err)
	}
	defer w.rollback(ctx)
	n, err := s.nodes.LockedNode(ctx, w.tx, nodeID)
	if err != nil {
		return "", false, op.Reject(err, nodes.ErrNotFound)
	}
	_, principalID, err := s.serviceAccount(ctx, w.tx, n.ID, memberID)
	if err == ErrNotFound {
		var gone bool
		if err := w.tx.QueryRow(ctx, `
			SELECT EXISTS (SELECT 1 FROM members WHERE id = $1 AND root_id = $2 AND principal = 'service_account' AND removed_at IS NOT NULL)
		`, memberID, n.RootID).Scan(&gone); err != nil {
			return "", false, op.Fail(err)
		}
		if gone {
			op.OK("unchanged")
			return StatusRemoved, false, nil
		}
		return "", false, op.Expected("not found", ErrNotFound)
	}
	if err != nil {
		return "", false, op.Fail(err)
	}
	m, err := s.lockedMember(ctx, w, memberID)
	if err != nil {
		return "", false, op.Fail(err)
	}
	prior := StatusActive
	if m.suspended {
		prior = StatusSuspended
	}
	if err := s.removeMember(ctx, w, m); err != nil {
		return "", false, op.Fail(err)
	}
	keyRows, err := w.tx.Query(ctx, `SELECT id FROM api_keys WHERE principal_id = $1 AND revoked_at IS NULL`, principalID)
	if err != nil {
		return "", false, op.Fail(err)
	}
	keyIDs, err := pgx.CollectRows(keyRows, pgx.RowTo[string])
	if err != nil {
		return "", false, op.Fail(err)
	}
	for _, kid := range keyIDs {
		w.invalidate(lifecycle.InvalidateKey(kid))
	}
	if _, err := w.tx.Exec(ctx, `DELETE FROM principals WHERE id = $1`, principalID); err != nil {
		return "", false, op.Fail(err)
	}
	if err := w.commit(ctx); err != nil {
		return "", false, op.Fail(err)
	}
	op.OK("deleted")
	return prior, true, nil
}
