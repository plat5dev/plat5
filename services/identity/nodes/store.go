// Package nodes is the tree: nodes of the deployment's kinds, their closure
// table, slugs and tombstones, soft delete, restore, purge, and kind removal
// (docs/concepts/lifecycle.md, docs/internals/identity-schema.md).
package nodes

import (
	"context"
	stderrors "errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"

	"github.com/plat5dev/plat5/identity/errors"
	"github.com/plat5dev/plat5/identity/feed"
	"github.com/plat5dev/plat5/identity/internal/dbx"
	"github.com/plat5dev/plat5/identity/internal/id"
	"github.com/plat5dev/plat5/identity/lifecycle"
	"github.com/plat5dev/plat5/identity/metrics"
)

const (
	StatusActive  = "active"
	StatusDeleted = "deleted"
	StatusPurged  = "purged"

	MaxLabelLen = 128

	storeTracerName = "identity.nodes.store"
	// treeLockSpace keys the per-tree advisory lock. Two-int keys never overlap
	// the one-bigint key lifecycle events use.
	treeLockSpace int32 = 0x706c6135        // "pla5"
	signInLockKey int64 = 0x706c617435_7369 // "plat5" "si"
	purgeBatch          = 100
	maxSlugTries        = 1000
)

var ErrNotFound = stderrors.New("node not found")

type Node struct {
	ID         string
	Kind       string
	ParentID   *string
	RootID     string
	Slug       *string
	Label      string
	Status     string
	DeletedAt  *time.Time
	PurgeAfter *time.Time
	DeletionID *string
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// Store changes the tree. Every write that changes a tree's structure holds
// that tree's lock, so a create never lands under a node being deleted, and a
// delete or restore sees the whole subtree.
type Store struct {
	pool   *pgxpool.Pool
	tracer trace.Tracer
	now    func() time.Time
}

func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool, tracer: otel.Tracer(storeTracerName), now: time.Now}
}

// WithClock is the store with a fixed notion of now, for tests of grace.
func (s *Store) WithClock(now func() time.Time) *Store {
	c := *s
	c.now = now
	return &c
}

const nodeColumns = `id, kind, parent_id, root_id, slug, label, status, deleted_at, purge_after, deletion_id, created_at, updated_at`

// Columns is the node columns qualified by alias, in ScanNode's order.
func Columns(alias string) string {
	cols := strings.Split(nodeColumns, ", ")
	for i, c := range cols {
		cols[i] = alias + "." + c
	}
	return strings.Join(cols, ", ")
}

// ScanNode reads a node selected with Columns, followed by extra columns.
func ScanNode(row dbx.Scannable, extra ...any) (*Node, error) {
	var n Node
	dest := append([]any{&n.ID, &n.Kind, &n.ParentID, &n.RootID, &n.Slug, &n.Label, &n.Status,
		&n.DeletedAt, &n.PurgeAfter, &n.DeletionID, &n.CreatedAt, &n.UpdatedAt}, extra...)
	if err := row.Scan(dest...); err != nil {
		return nil, err
	}
	return &n, nil
}

func scanNode(row dbx.Scannable) (*Node, error) {
	var n Node
	err := row.Scan(&n.ID, &n.Kind, &n.ParentID, &n.RootID, &n.Slug, &n.Label, &n.Status,
		&n.DeletedAt, &n.PurgeAfter, &n.DeletionID, &n.CreatedAt, &n.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &n, nil
}

// CreateInput is a new node. ParentID empty makes a root.
type CreateInput struct {
	ParentID string
	Kind     string
	Label    string
	Slug     *string
}

// Create makes a node of a kind the config has, under a live parent its kind
// allows, with a slug fixed for good. It writes <kind>.created.
func (s *Store) Create(ctx context.Context, snap *feed.Snapshot, in CreateInput) (*Node, error) {
	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "create_node", dbx.DefaultTimeout)
	defer cancel()
	defer op.End()

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, op.Fail(err)
	}
	defer tx.Rollback(ctx)
	n, err := s.CreateTx(ctx, tx, snap, in)
	if err != nil {
		return nil, op.Reject(err, ErrNotFound)
	}
	if err := recordCreated(ctx, tx, n); err != nil {
		return nil, op.Fail(err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, op.Fail(err)
	}
	metrics.RecordNodeOps("created", 1)
	op.OK("created")
	return n, nil
}

// CreateTx is Create inside tx, holding the new node's tree lock, for a caller
// that writes more in the same transaction (the creator's binding). It doesn't
// write <kind>.created: the caller does, after its own writes.
func (s *Store) CreateTx(ctx context.Context, tx pgx.Tx, snap *feed.Snapshot, in CreateInput) (*Node, error) {
	label, err := NormalizeLabel(in.Label)
	if err != nil {
		return nil, err
	}
	kind, ok := snap.Kind(in.Kind)
	if !ok || kind.SignIn {
		return nil, kindNotHere(snap, in.Kind)
	}
	if in.Slug != nil {
		if err := checkSlug(snap, *in.Slug); err != nil {
			return nil, err
		}
	}

	now := s.now().UTC()
	n := &Node{ID: id.New(id.Node), Kind: kind.Name, Label: label, Status: StatusActive, CreatedAt: now, UpdatedAt: now}
	if in.ParentID == "" {
		if !kind.IsRoot() {
			return nil, kindNotHere(snap, kind.Name)
		}
		n.RootID = n.ID
		if err := LockTrees(ctx, tx, []string{n.ID}); err != nil {
			return nil, err
		}
	} else {
		parent, err := s.LockedNode(ctx, tx, in.ParentID)
		if err != nil {
			return nil, err
		}
		if !kind.AllowsParent(parent.Kind) {
			return nil, kindNotHere(snap, kind.Name)
		}
		if parent.Status != StatusActive {
			return nil, parentNotLive(snap, parent)
		}
		n.ParentID = &parent.ID
		n.RootID = parent.RootID
	}

	if err := s.insertWithSlug(ctx, tx, snap, n, in.Slug); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO node_ancestors (node_id, ancestor_id, depth)
		SELECT $1, $1, 0
		UNION ALL
		SELECT $1, ancestor_id, depth + 1 FROM node_ancestors WHERE node_id = $2
	`, n.ID, n.ParentID); err != nil {
		return nil, err
	}
	return n, nil
}

// recordCreated writes n's <kind>.created.
func recordCreated(ctx context.Context, tx pgx.Tx, n *Node) error {
	return lifecycle.Record(ctx, tx, lifecycle.NodeEvent(lifecycle.TypeNodeCreated, n.Kind, n.ID, n.RootID))
}

// insertWithSlug inserts n with the slug asked for, or the first free one
// generated from its label. Tombstones and reserved slugs count as taken.
func (s *Store) insertWithSlug(ctx context.Context, tx pgx.Tx, snap *feed.Snapshot, n *Node, asked *string) error {
	insert := func(slug string) error {
		sp, err := tx.Begin(ctx)
		if err != nil {
			return err
		}
		defer sp.Rollback(ctx)
		if _, err := sp.Exec(ctx, `
			INSERT INTO nodes (id, kind, parent_id, root_id, slug, label, status, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, 'active', $7, $7)
		`, n.ID, n.Kind, n.ParentID, n.RootID, slug, n.Label, n.CreatedAt); err != nil {
			return err
		}
		return sp.Commit(ctx)
	}

	if asked != nil {
		if err := insert(*asked); err != nil {
			if dbx.IsUniqueViolation(err) {
				return slugTaken(*asked)
			}
			return err
		}
		n.Slug = asked
		return nil
	}

	base := baseSlug(n.Label, n.Kind)
	parent := ""
	if n.ParentID != nil {
		parent = *n.ParentID
	}
	for attempt := 0; attempt < 3; attempt++ {
		rows, err := tx.Query(ctx, `
			SELECT slug FROM nodes
			WHERE COALESCE(parent_id, '') = $1 AND kind = $2 AND (slug = $3 OR slug LIKE $3 || '-%')
		`, parent, n.Kind, base)
		if err != nil {
			return err
		}
		taken, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return err
		}
		used := make(map[string]bool, len(taken))
		for _, t := range taken {
			used[t] = true
		}
		for i := 1; i <= maxSlugTries; i++ {
			slug := candidate(base, i)
			if used[slug] || snap.ReservedSlugs[slug] {
				continue
			}
			err := insert(slug)
			if err == nil {
				n.Slug = &slug
				return nil
			}
			if !dbx.IsUniqueViolation(err) {
				return err
			}
			break // taken since the read: read again
		}
	}
	return fmt.Errorf("no free slug for %q", base)
}

// EnsureSignIn creates the sign-in node if there is none: one root of the
// sign-in kind, with no slug, labelled with the kind's label.
func (s *Store) EnsureSignIn(ctx context.Context, snap *feed.Snapshot) (*Node, error) {
	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "ensure_sign_in_node", dbx.DefaultTimeout)
	defer cancel()
	defer op.End()

	kind := snap.SignInKind()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, op.Fail(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, signInLockKey); err != nil {
		return nil, op.Fail(err)
	}
	existing, err := scanNode(tx.QueryRow(ctx, `
		SELECT `+nodeColumns+` FROM nodes
		WHERE kind = $1 AND parent_id IS NULL AND status <> 'purged'
		ORDER BY id LIMIT 1
	`, kind.Name))
	if err == nil {
		op.OK("exists")
		return existing, nil
	}
	if !dbx.IsNoRows(err) {
		return nil, op.Fail(err)
	}

	now := s.now().UTC()
	n := &Node{ID: id.New(id.Node), Kind: kind.Name, Label: kind.DisplayLabel(), Status: StatusActive, CreatedAt: now, UpdatedAt: now}
	n.RootID = n.ID
	if _, err := tx.Exec(ctx, `
		INSERT INTO nodes (id, kind, root_id, label, status, created_at, updated_at)
		VALUES ($1, $2, $1, $3, 'active', $4, $4)
	`, n.ID, n.Kind, n.Label, now); err != nil {
		return nil, op.Fail(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO node_ancestors (node_id, ancestor_id, depth) VALUES ($1, $1, 0)`, n.ID); err != nil {
		return nil, op.Fail(err)
	}
	if err := lifecycle.Record(ctx, tx, lifecycle.NodeEvent(lifecycle.TypeNodeCreated, n.Kind, n.ID, n.RootID)); err != nil {
		return nil, op.Fail(err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, op.Fail(err)
	}
	metrics.RecordNodeOps("created", 1)
	op.OK("created")
	return n, nil
}

func (s *Store) Get(ctx context.Context, nodeID string) (*Node, error) {
	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "get_node", dbx.DefaultTimeout)
	defer cancel()
	defer op.End()
	if !id.Valid(id.Node, nodeID) {
		return nil, op.Expected("not found", ErrNotFound)
	}
	n, err := scanNode(s.pool.QueryRow(ctx, `SELECT `+nodeColumns+` FROM nodes WHERE id = $1`, nodeID))
	if dbx.IsNoRows(err) {
		return nil, op.Expected("not found", ErrNotFound)
	}
	if err != nil {
		return nil, op.Fail(err)
	}
	op.OK("ok")
	return n, nil
}

// ListFilter narrows GET /nodes. Empty fields don't filter.
type ListFilter struct {
	Kind     string
	ParentID string
	Status   string
}

// List is every node, unfiltered by access, id ascending.
func (s *Store) List(ctx context.Context, f ListFilter, limit int, startingAfter string) ([]*Node, bool, error) {
	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "list_nodes", dbx.DefaultTimeout)
	defer cancel()
	defer op.End()
	rows, err := s.pool.Query(ctx, `
		SELECT `+nodeColumns+` FROM nodes
		WHERE ($1 = '' OR kind = $1)
		  AND ($2 = '' OR parent_id = $2)
		  AND ($3 = '' OR status = $3)
		  AND ($4 = '' OR id > $4)
		ORDER BY id ASC
		LIMIT $5
	`, f.Kind, f.ParentID, f.Status, startingAfter, limit+1)
	if err != nil {
		return nil, false, op.Fail(err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (*Node, error) { return scanNode(r) })
	if err != nil {
		return nil, false, op.Fail(err)
	}
	hasMore := len(out) > limit
	if hasMore {
		out = out[:limit]
	}
	op.OK("ok")
	return out, hasMore, nil
}

// UpdateLabel sets a node's label. It returns the label before.
func (s *Store) UpdateLabel(ctx context.Context, nodeID, label string) (string, *Node, error) {
	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "update_node_label", dbx.DefaultTimeout)
	defer cancel()
	defer op.End()
	label, err := NormalizeLabel(label)
	if err != nil {
		return "", nil, op.Expected("invalid", err)
	}
	if !id.Valid(id.Node, nodeID) {
		return "", nil, op.Expected("not found", ErrNotFound)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", nil, op.Fail(err)
	}
	defer tx.Rollback(ctx)
	var before string
	err = tx.QueryRow(ctx, `SELECT label FROM nodes WHERE id = $1 FOR UPDATE`, nodeID).Scan(&before)
	if dbx.IsNoRows(err) {
		return "", nil, op.Expected("not found", ErrNotFound)
	}
	if err != nil {
		return "", nil, op.Fail(err)
	}
	n, err := scanNode(tx.QueryRow(ctx, `
		UPDATE nodes SET label = $2, updated_at = CASE WHEN label = $2 THEN updated_at ELSE $3 END
		WHERE id = $1 RETURNING `+nodeColumns, nodeID, label, s.now().UTC()))
	if err != nil {
		return "", nil, op.Fail(err)
	}
	if err := tx.Commit(ctx); err != nil {
		return "", nil, op.Fail(err)
	}
	op.OK("ok")
	return before, n, nil
}

// Delete soft-deletes a live node and its live descendants, parents before
// children, under one deletion_id, with purge_after fixed from the grace now.
// Keys of service accounts born there are revoked. A node already deleted or
// purged is left as it is: changed is false.
func (s *Store) Delete(ctx context.Context, snap *feed.Snapshot, nodeID string) (changed bool, err error) {
	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "delete_node", dbx.DefaultTimeout)
	defer cancel()
	defer op.End()

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, op.Fail(err)
	}
	defer tx.Rollback(ctx)
	n, err := s.LockedNode(ctx, tx, nodeID)
	if err != nil {
		return false, op.Expected("not found", err)
	}
	if n.ParentID == nil && n.Kind == snap.SignInKind().Name {
		return false, op.Expected("sign-in", errors.FieldError("node_id",
			fmt.Sprintf("The %s can't be deleted.", strings.ToLower(snap.SignInKind().DisplayLabel()))))
	}
	if n.Status != StatusActive {
		op.OK("unchanged")
		return false, nil
	}

	deleted, err := s.softDelete(ctx, tx, []string{n.ID}, id.New(""), snap.DeletionGrace)
	if err != nil {
		return false, op.Fail(err)
	}
	if err := recordNodeEvents(ctx, tx, lifecycle.TypeNodeDeleted, deleted); err != nil {
		return false, op.Fail(err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, op.Fail(err)
	}
	metrics.RecordNodeOps("deleted", len(deleted))
	op.OK("deleted")
	return true, nil
}

// changedNode is one node a cascade changed, for its event.
type changedNode struct {
	id, kind, rootID string
	depth            int
}

// softDelete marks the live nodes at and under tops deleted, and revokes the
// keys of service accounts born there and the invites on them. A node already
// deleted keeps its own deletion_id and purge_after.
func (s *Store) softDelete(ctx context.Context, tx pgx.Tx, tops []string, deletionID string, grace time.Duration) ([]changedNode, error) {
	now := s.now().UTC()
	rows, err := tx.Query(ctx, `
		UPDATE nodes n
		SET status = 'deleted', deleted_at = $2, purge_after = $3, deletion_id = $4, updated_at = $2
		WHERE n.status = 'active'
		  AND n.id IN (SELECT node_id FROM node_ancestors WHERE ancestor_id = ANY($1))
		RETURNING n.id, n.kind, n.root_id, (SELECT count(*) - 1 FROM node_ancestors WHERE node_id = n.id)
	`, tops, now, now.Add(grace), deletionID)
	if err != nil {
		return nil, err
	}
	changed, err := collectChanged(rows)
	if err != nil {
		return nil, err
	}
	if len(changed) == 0 {
		return nil, nil
	}
	if _, err := tx.Exec(ctx, `
		UPDATE api_keys SET revoked_at = $2
		WHERE revoked_at IS NULL AND principal_id IN (
			SELECT p.id FROM principals p
			JOIN node_ancestors a ON a.node_id = p.born_node_id
			WHERE a.ancestor_id = ANY($1)
		)
	`, tops, now); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE invites SET status = 'revoked'
		WHERE status = 'active' AND node_id IN (SELECT node_id FROM node_ancestors WHERE ancestor_id = ANY($1))
	`, tops); err != nil {
		return nil, err
	}
	return changed, nil
}

// Restore brings back a deleted node and the descendants its delete cascaded
// to, within the grace. Keys stay revoked. A live node is left as it is:
// changed is false.
func (s *Store) Restore(ctx context.Context, snap *feed.Snapshot, nodeID string) (*Node, bool, error) {
	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "restore_node", dbx.DefaultTimeout)
	defer cancel()
	defer op.End()

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, false, op.Fail(err)
	}
	defer tx.Rollback(ctx)
	n, err := s.LockedNode(ctx, tx, nodeID)
	if err != nil {
		return nil, false, op.Expected("not found", err)
	}
	if n.Status == StatusActive {
		op.OK("unchanged")
		return n, false, nil
	}
	now := s.now().UTC()
	if n.Status == StatusPurged || !now.Before(*n.PurgeAfter) {
		return nil, false, op.Expected("past grace", errors.ConflictError("This can no longer be restored.", "purge_after", n.PurgeAfter))
	}
	if _, ok := snap.Kind(n.Kind); !ok {
		return nil, false, op.Expected("kind removed", errors.ConflictError(
			fmt.Sprintf("A %s can't be restored while its kind is removed.", n.Kind), "kind", n.Kind))
	}
	if n.ParentID != nil {
		var parentStatus, parentKind string
		if err := tx.QueryRow(ctx, `SELECT status, kind FROM nodes WHERE id = $1`, *n.ParentID).Scan(&parentStatus, &parentKind); err != nil {
			return nil, false, op.Fail(err)
		}
		if parentStatus != StatusActive {
			return nil, false, op.Expected("parent deleted", errors.ConflictError(
				fmt.Sprintf("Restore the %s first.", KindWord(snap, parentKind)), "parent_id", *n.ParentID))
		}
	}

	rows, err := tx.Query(ctx, `
		SELECT n.id, n.kind, n.root_id, n.parent_id, (SELECT count(*) - 1 FROM node_ancestors WHERE node_id = n.id) AS depth
		FROM nodes n
		WHERE n.status = 'deleted' AND n.deletion_id = $2
		  AND n.id IN (SELECT node_id FROM node_ancestors WHERE ancestor_id = $1)
		ORDER BY depth, n.id
	`, n.ID, *n.DeletionID)
	if err != nil {
		return nil, false, op.Fail(err)
	}
	restored, err := s.restoreTopDown(ctx, tx, snap, rows, map[string]bool{})
	if err != nil {
		return nil, false, op.Fail(err)
	}
	if err := recordNodeEvents(ctx, tx, lifecycle.TypeNodeRestored, restored); err != nil {
		return nil, false, op.Fail(err)
	}
	out, err := scanNode(tx.QueryRow(ctx, `SELECT `+nodeColumns+` FROM nodes WHERE id = $1`, n.ID))
	if err != nil {
		return nil, false, op.Fail(err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, false, op.Fail(err)
	}
	metrics.RecordNodeOps("restored", len(restored))
	op.OK("restored")
	return out, true, nil
}

// restoreTopDown restores candidates (rows of id, kind, root_id, parent_id,
// depth, shallowest first). A node comes back only if its kind is in the
// config and its parent is live or coming back with it, so nothing live ever
// sits under a deleted node. live holds parents known to be live.
func (s *Store) restoreTopDown(ctx context.Context, tx pgx.Tx, snap *feed.Snapshot, rows pgx.Rows, live map[string]bool) ([]changedNode, error) {
	type cand struct {
		changedNode
		parentID *string
	}
	cands, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (cand, error) {
		var c cand
		err := r.Scan(&c.id, &c.kind, &c.rootID, &c.parentID, &c.depth)
		return c, err
	})
	if err != nil {
		return nil, err
	}
	parentIDs := make([]string, 0, len(cands))
	for _, c := range cands {
		if c.parentID != nil {
			parentIDs = append(parentIDs, *c.parentID)
		}
	}
	if len(parentIDs) > 0 {
		rows, err := tx.Query(ctx, `SELECT id FROM nodes WHERE id = ANY($1) AND status = 'active'`, parentIDs)
		if err != nil {
			return nil, err
		}
		active, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return nil, err
		}
		for _, a := range active {
			live[a] = true
		}
	}

	var restored []changedNode
	var ids []string
	for _, c := range cands {
		if _, ok := snap.Kind(c.kind); !ok {
			continue
		}
		if c.parentID != nil && !live[*c.parentID] {
			continue
		}
		live[c.id] = true
		restored = append(restored, c.changedNode)
		ids = append(ids, c.id)
	}
	if len(ids) == 0 {
		return nil, nil
	}
	if _, err := tx.Exec(ctx, `
		UPDATE nodes SET status = 'active', deleted_at = NULL, purge_after = NULL, deletion_id = NULL, updated_at = $2
		WHERE id = ANY($1)
	`, ids, s.now().UTC()); err != nil {
		return nil, err
	}
	return restored, nil
}

// ReleaseSlug frees a purged node's slug for a new node. It returns the slug
// it freed, or nil when there was none left to free.
func (s *Store) ReleaseSlug(ctx context.Context, nodeID string) (*string, *Node, error) {
	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "release_node_slug", dbx.DefaultTimeout)
	defer cancel()
	defer op.End()

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, nil, op.Fail(err)
	}
	defer tx.Rollback(ctx)
	n, err := s.LockedNode(ctx, tx, nodeID)
	if err != nil {
		return nil, nil, op.Expected("not found", err)
	}
	if n.Status != StatusPurged {
		return nil, nil, op.Expected("not purged", errors.ConflictError("Only a purged node's slug can be released.", "status", n.Status))
	}
	freed := n.Slug
	if freed != nil {
		n, err = scanNode(tx.QueryRow(ctx, `
			UPDATE nodes SET slug = NULL, updated_at = $2 WHERE id = $1 RETURNING `+nodeColumns,
			n.ID, s.now().UTC()))
		if err != nil {
			return nil, nil, op.Fail(err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, nil, op.Fail(err)
	}
	op.OK("ok")
	return freed, n, nil
}

// PurgeDue purges every deleted node whose purge_after has passed, with the
// deleted nodes under it, one tree at a time. It returns how many it purged.
func (s *Store) PurgeDue(ctx context.Context) (int, error) {
	total := 0
	for {
		n, err := s.purgeBatch(ctx)
		total += n
		if err != nil || n == 0 {
			return total, err
		}
	}
}

func (s *Store) purgeBatch(ctx context.Context) (int, error) {
	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "purge_due_nodes", 4*dbx.DefaultTimeout)
	defer cancel()
	defer op.End()

	now := s.now().UTC()
	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT root_id FROM (
			SELECT root_id FROM nodes WHERE status = 'deleted' AND purge_after <= $1
			ORDER BY purge_after LIMIT $2
		) due
	`, now, purgeBatch)
	if err != nil {
		return 0, op.Fail(err)
	}
	roots, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return 0, op.Fail(err)
	}
	total := 0
	for _, root := range roots {
		n, err := s.purgeTree(ctx, root, now)
		if err != nil {
			return total, op.Fail(err)
		}
		total += n
	}
	if _, err := s.pool.Exec(ctx, `
		DELETE FROM kind_removals k
		WHERE NOT EXISTS (SELECT 1 FROM nodes WHERE deletion_id = k.deletion_id AND status = 'deleted')
	`); err != nil {
		return total, op.Fail(err)
	}
	op.OK("ok")
	return total, nil
}

// purgeTree purges one tree's due nodes in one transaction: the nodes and the
// deleted nodes under them become tombstones, their bindings go, service
// accounts born at them go (principal, keys, member), and a purged root's
// members are removed. A person left with no bindings in the tree is removed.
func (s *Store) purgeTree(ctx context.Context, rootID string, now time.Time) (int, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	if err := LockTrees(ctx, tx, []string{rootID}); err != nil {
		return 0, err
	}

	rows, err := tx.Query(ctx, `
		UPDATE nodes n SET status = 'purged', updated_at = $3
		WHERE n.status = 'deleted' AND n.id IN (
			SELECT a.node_id FROM node_ancestors a JOIN nodes due ON due.id = a.ancestor_id
			WHERE due.root_id = $1 AND due.status = 'deleted' AND due.purge_after <= $2
		)
		RETURNING n.id, n.kind, n.root_id, (SELECT count(*) - 1 FROM node_ancestors WHERE node_id = n.id)
	`, rootID, now, now)
	if err != nil {
		return 0, err
	}
	purged, err := collectChanged(rows)
	if err != nil {
		return 0, err
	}
	if len(purged) == 0 {
		return 0, tx.Commit(ctx)
	}
	ids := make([]string, len(purged))
	rootPurged := false
	for i, p := range purged {
		ids[i] = p.id
		rootPurged = rootPurged || p.id == rootID
	}

	type removal struct{ memberID, principal string }
	var removed []removal
	removeMembers := func(sql string, args ...any) error {
		rows, err := tx.Query(ctx, sql, args...)
		if err != nil {
			return err
		}
		got, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (removal, error) {
			var m removal
			err := r.Scan(&m.memberID, &m.principal)
			return m, err
		})
		if err != nil {
			return err
		}
		removed = append(removed, got...)
		return nil
	}

	// Service accounts born at a purged node go with it.
	if err := removeMembers(`
		UPDATE members m SET removed_at = $2
		FROM principals p
		WHERE p.id = m.principal_id AND p.born_node_id = ANY($1) AND m.removed_at IS NULL
		RETURNING m.id, p.kind
	`, ids, now); err != nil {
		return 0, err
	}
	if rootPurged {
		if err := removeMembers(`
			UPDATE members m SET removed_at = $2
			FROM principals p
			WHERE p.id = m.principal_id AND m.root_id = $1 AND m.removed_at IS NULL
			RETURNING m.id, p.kind
		`, rootID, now); err != nil {
			return 0, err
		}
	}
	removedIDs := make([]string, len(removed))
	for i, m := range removed {
		removedIDs[i] = m.memberID
	}
	rows, err = tx.Query(ctx, `
		DELETE FROM bindings WHERE node_id = ANY($1) OR member_id = ANY($2)
		RETURNING member_id
	`, ids, removedIDs)
	if err != nil {
		return 0, err
	}
	touched, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return 0, err
	}
	// A person's member outside the directory left with no bindings is removed.
	if err := removeMembers(`
		UPDATE members m SET removed_at = $2
		FROM principals p
		WHERE p.id = m.principal_id AND p.kind = 'person' AND m.id = ANY($1) AND m.removed_at IS NULL
		  AND NOT EXISTS (SELECT 1 FROM bindings b WHERE b.member_id = m.id)
		RETURNING m.id, p.kind
	`, touched, now); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM principals WHERE kind = 'service_account' AND born_node_id = ANY($1)`, ids); err != nil {
		return 0, err
	}

	if err := recordNodeEvents(ctx, tx, lifecycle.TypeNodePurged, purged); err != nil {
		return 0, err
	}
	for _, m := range removed {
		if err := lifecycle.Record(ctx, tx, lifecycle.MemberEvent(lifecycle.TypeMemberRemoved, m.memberID, rootID, m.principal)); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	metrics.RecordNodeOps("purged", len(purged))
	return len(purged), nil
}

// ReconcileKinds brings the tree in line with the config's kinds. A kind the
// config lacks has its live nodes soft-deleted, with their descendants, under
// one deletion_id recorded in kind_removals. A kind the config has again gets
// back the nodes its removal deleted that are still in grace, and its record
// goes. Run on every config identity reads, so a removal missed while identity
// was down still happens.
func (s *Store) ReconcileKinds(ctx context.Context, snap *feed.Snapshot) error {
	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "reconcile_kinds", 4*dbx.DefaultTimeout)
	defer cancel()
	defer op.End()

	names := snap.Names()
	rows, err := s.pool.Query(ctx, `SELECT kind FROM kind_removals WHERE kind = ANY($1) ORDER BY kind`, names)
	if err != nil {
		return op.Fail(err)
	}
	back, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return op.Fail(err)
	}
	for _, kind := range back {
		if err := s.reapplyKind(ctx, snap, kind); err != nil {
			return op.Fail(err)
		}
	}

	rows, err = s.pool.Query(ctx, `
		SELECT DISTINCT kind FROM nodes WHERE status = 'active' AND NOT (kind = ANY($1)) ORDER BY kind
	`, names)
	if err != nil {
		return op.Fail(err)
	}
	gone, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return op.Fail(err)
	}
	for _, kind := range gone {
		if err := s.removeKind(ctx, snap, kind); err != nil {
			return op.Fail(err)
		}
	}
	op.OK("ok")
	return nil
}

func (s *Store) removeKind(ctx context.Context, snap *feed.Snapshot, kind string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	var deletionID string
	if err := tx.QueryRow(ctx, `
		INSERT INTO kind_removals (kind, deletion_id, removed_at) VALUES ($1, $2, $3)
		ON CONFLICT (kind) DO UPDATE SET kind = EXCLUDED.kind
		RETURNING deletion_id
	`, kind, id.New(""), s.now().UTC()).Scan(&deletionID); err != nil {
		return err
	}
	rows, err := tx.Query(ctx, `SELECT DISTINCT root_id FROM nodes WHERE kind = $1 AND status = 'active'`, kind)
	if err != nil {
		return err
	}
	roots, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	if err := LockTrees(ctx, tx, roots); err != nil {
		return err
	}
	rows, err = tx.Query(ctx, `SELECT id FROM nodes WHERE kind = $1 AND status = 'active'`, kind)
	if err != nil {
		return err
	}
	tops, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	deleted, err := s.softDelete(ctx, tx, tops, deletionID, snap.DeletionGrace)
	if err != nil {
		return err
	}
	if err := recordNodeEvents(ctx, tx, lifecycle.TypeNodeDeleted, deleted); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	metrics.RecordNodeOps("deleted", len(deleted))
	return nil
}

func (s *Store) reapplyKind(ctx context.Context, snap *feed.Snapshot, kind string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	var deletionID string
	err = tx.QueryRow(ctx, `DELETE FROM kind_removals WHERE kind = $1 RETURNING deletion_id`, kind).Scan(&deletionID)
	if dbx.IsNoRows(err) {
		return nil
	}
	if err != nil {
		return err
	}
	now := s.now().UTC()
	rows, err := tx.Query(ctx, `
		SELECT DISTINCT root_id FROM nodes WHERE deletion_id = $1 AND status = 'deleted' AND purge_after > $2
	`, deletionID, now)
	if err != nil {
		return err
	}
	roots, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	if err := LockTrees(ctx, tx, roots); err != nil {
		return err
	}
	rows, err = tx.Query(ctx, `
		SELECT n.id, n.kind, n.root_id, n.parent_id, (SELECT count(*) - 1 FROM node_ancestors WHERE node_id = n.id) AS depth
		FROM nodes n
		WHERE n.deletion_id = $1 AND n.status = 'deleted' AND n.purge_after > $2
		ORDER BY depth, n.id
	`, deletionID, now)
	if err != nil {
		return err
	}
	restored, err := s.restoreTopDown(ctx, tx, snap, rows, map[string]bool{})
	if err != nil {
		return err
	}
	if err := recordNodeEvents(ctx, tx, lifecycle.TypeNodeRestored, restored); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	metrics.RecordNodeOps("restored", len(restored))
	return nil
}

// LockedNode takes the lock on the node's tree, and on alsoLock's trees in
// the same order, then reads the node.
func (s *Store) LockedNode(ctx context.Context, tx pgx.Tx, nodeID string, alsoLock ...string) (*Node, error) {
	if !id.Valid(id.Node, nodeID) {
		return nil, ErrNotFound
	}
	var rootID string
	err := tx.QueryRow(ctx, `SELECT root_id FROM nodes WHERE id = $1`, nodeID).Scan(&rootID)
	if dbx.IsNoRows(err) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if err := LockTrees(ctx, tx, append([]string{rootID}, alsoLock...)); err != nil {
		return nil, err
	}
	return scanNode(tx.QueryRow(ctx, `SELECT `+nodeColumns+` FROM nodes WHERE id = $1`, nodeID))
}

// LockTrees takes the per-tree locks in a fixed order, so two writers never
// wait on each other in a cycle. Lifecycle events take their own lock last.
func LockTrees(ctx context.Context, tx pgx.Tx, rootIDs []string) error {
	sorted := append([]string(nil), rootIDs...)
	sort.Strings(sorted)
	for i, r := range sorted {
		if i > 0 && r == sorted[i-1] {
			continue
		}
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1, hashtext($2))`, treeLockSpace, r); err != nil {
			return err
		}
	}
	return nil
}

func collectChanged(rows pgx.Rows) ([]changedNode, error) {
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (changedNode, error) {
		var c changedNode
		err := r.Scan(&c.id, &c.kind, &c.rootID, &c.depth)
		return c, err
	})
	sortChanged(out)
	return out, err
}

// sortChanged orders a cascade parents before children.
func sortChanged(c []changedNode) {
	sort.Slice(c, func(i, j int) bool {
		if c[i].depth != c[j].depth {
			return c[i].depth < c[j].depth
		}
		return c[i].id < c[j].id
	})
}

func recordNodeEvents(ctx context.Context, tx pgx.Tx, typ string, nodes []changedNode) error {
	for _, n := range nodes {
		if err := lifecycle.Record(ctx, tx, lifecycle.NodeEvent(typ, n.kind, n.id, n.rootID)); err != nil {
			return err
		}
	}
	return nil
}

// NormalizeLabel trims a label and checks it is 1 to 128 characters.
func NormalizeLabel(raw string) (string, error) {
	label := strings.TrimSpace(raw)
	if label == "" {
		return "", errors.FieldError("label", "Label is required.")
	}
	if utf8.RuneCountInString(label) > MaxLabelLen {
		return "", errors.FieldError("label", "Label is too long.")
	}
	return label, nil
}

func checkSlug(snap *feed.Snapshot, slug string) error {
	if !ValidSlug(slug) {
		return errors.FieldError("slug", "Use 3 to 63 lowercase letters, digits and hyphens, starting with a letter.")
	}
	if snap.ReservedSlugs[slug] {
		return errors.FieldError("slug", "That name is reserved.")
	}
	return nil
}

func slugTaken(slug string) error {
	return errors.ConflictError("That name is taken.", "slug", slug)
}

func kindNotHere(snap *feed.Snapshot, kind string) error {
	return errors.FieldError("kind", fmt.Sprintf("A %s can't go here.", KindWord(snap, kind)))
}

func parentNotLive(snap *feed.Snapshot, parent *Node) error {
	return errors.ConflictError(fmt.Sprintf("Restore the %s first.", KindWord(snap, parent.Kind)), "parent_id", parent.ID)
}

// KindWord is how a sentence names a kind: its label, lowercased, or its name.
func KindWord(snap *feed.Snapshot, kind string) string {
	if k, ok := snap.Kind(kind); ok {
		return strings.ToLower(k.DisplayLabel())
	}
	return kind
}
