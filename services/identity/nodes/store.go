// Package nodes is the tree: nodes of the deployment's kinds, their closure
// table, slugs and tombstones, soft delete, restore and purge
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

	MaxDisplayNameLen = 128

	storeTracerName = "identity.nodes.store"
	// treeLockSpace keys the per-tree advisory lock. Two-int keys never overlap
	// the one-bigint key lifecycle events use.
	treeLockSpace int32 = 0x706c6135 // "pla5"
	purgeBatch          = 100
	maxSlugTries        = 1000
)

var ErrNotFound = stderrors.New("node not found")

type Node struct {
	ID          string
	Kind        string
	ParentID    *string
	RootID      string
	Slug        *string
	DisplayName string
	Status      string
	DeletedAt   *time.Time
	PurgeAfter  *time.Time
	DeletionID  *string
	CreatedAt   time.Time
	UpdatedAt   time.Time
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

const nodeColumns = `id, kind, parent_id, root_id, slug, display_name, status, deleted_at, purge_after, deletion_id, created_at, updated_at`

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
	dest := append([]any{&n.ID, &n.Kind, &n.ParentID, &n.RootID, &n.Slug, &n.DisplayName, &n.Status,
		&n.DeletedAt, &n.PurgeAfter, &n.DeletionID, &n.CreatedAt, &n.UpdatedAt}, extra...)
	if err := row.Scan(dest...); err != nil {
		return nil, err
	}
	return &n, nil
}

func scanNode(row dbx.Scannable) (*Node, error) {
	return ScanNode(row)
}

// CreateInput is a new node. ParentID empty makes a root.
type CreateInput struct {
	ParentID    string
	Kind        string
	DisplayName string
	Slug        *string
}

// Create makes a node of a kind the config has, under a live parent of the
// kind's parent kind, with a slug fixed for good. It writes node.created.
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

// CreateTx is Create inside tx, holding the new node's tree lock, for a caller
// that writes more in the same transaction (the creator's binding). It doesn't
// write node.created: the caller does, after its own writes.
func (s *Store) CreateTx(ctx context.Context, tx pgx.Tx, snap *feed.Snapshot, in CreateInput) (*Node, error) {
	name, err := NormalizeDisplayName(in.DisplayName)
	if err != nil {
		return nil, err
	}
	kind, ok := snap.Kind(in.Kind)
	if !ok {
		return nil, kindNotHere(in.Kind)
	}
	if in.Slug != nil {
		if err := checkSlug(snap, *in.Slug); err != nil {
			return nil, err
		}
	}

	now := s.now().UTC()
	n := &Node{ID: id.New(), Kind: kind.Name, DisplayName: name, Status: StatusActive, CreatedAt: now, UpdatedAt: now}
	if in.ParentID == "" {
		if !kind.IsRoot() {
			return nil, kindNotHere(kind.Name)
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
		if kind.Parent != parent.Kind {
			return nil, kindNotHere(kind.Name)
		}
		if parent.Status != StatusActive {
			return nil, parentNotLive(parent)
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

// insertWithSlug inserts n with the slug asked for, or the first free one
// generated from its display name. Root tombstones and reserved slugs count as
// taken.
func (s *Store) insertWithSlug(ctx context.Context, tx pgx.Tx, snap *feed.Snapshot, n *Node, asked *string) error {
	insert := func(slug string) error {
		sp, err := tx.Begin(ctx)
		if err != nil {
			return err
		}
		defer sp.Rollback(ctx)
		if _, err := sp.Exec(ctx, `
			INSERT INTO nodes (id, kind, parent_id, root_id, slug, display_name, status, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, 'active', $7, $7)
		`, n.ID, n.Kind, n.ParentID, n.RootID, slug, n.DisplayName, n.CreatedAt); err != nil {
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

	base := baseSlug(n.DisplayName, n.Kind)
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

func (s *Store) Get(ctx context.Context, nodeID string) (*Node, error) {
	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "get_node", dbx.DefaultTimeout)
	defer cancel()
	defer op.End()
	if !id.Valid(nodeID) {
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

// UpdateDisplayName sets a node's display name. It returns the name before.
func (s *Store) UpdateDisplayName(ctx context.Context, nodeID, name string) (string, *Node, error) {
	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "update_node_display_name", dbx.DefaultTimeout)
	defer cancel()
	defer op.End()
	name, err := NormalizeDisplayName(name)
	if err != nil {
		return "", nil, op.Expected("invalid", err)
	}
	if !id.Valid(nodeID) {
		return "", nil, op.Expected("not found", ErrNotFound)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", nil, op.Fail(err)
	}
	defer tx.Rollback(ctx)
	var before string
	err = tx.QueryRow(ctx, `SELECT display_name FROM nodes WHERE id = $1 FOR UPDATE`, nodeID).Scan(&before)
	if dbx.IsNoRows(err) {
		return "", nil, op.Expected("not found", ErrNotFound)
	}
	if err != nil {
		return "", nil, op.Fail(err)
	}
	n, err := scanNode(tx.QueryRow(ctx, `
		UPDATE nodes SET display_name = $2, updated_at = CASE WHEN display_name = $2 THEN updated_at ELSE $3 END
		WHERE id = $1 RETURNING `+nodeColumns, nodeID, name, s.now().UTC()))
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
// It revokes nothing: bindings, service accounts, keys and invites are kept,
// and refused while the node is deleted. A node already deleted or purged is
// left as it is: changed is false.
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
	if n.Status != StatusActive {
		op.OK("unchanged")
		return false, nil
	}

	now := s.now().UTC()
	rows, err := tx.Query(ctx, `
		UPDATE nodes n
		SET status = 'deleted', deleted_at = $2, purge_after = $3, deletion_id = $4, updated_at = $2
		WHERE n.status = 'active'
		  AND n.id IN (SELECT node_id FROM node_ancestors WHERE ancestor_id = $1)
		RETURNING n.id, n.kind, n.root_id, (SELECT count(*) - 1 FROM node_ancestors WHERE node_id = n.id)
	`, n.ID, now, now.Add(snap.DeletionGrace), id.New())
	if err != nil {
		return false, op.Fail(err)
	}
	deleted, err := collectChanged(rows)
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

// Restore brings back a deleted node and the descendants its delete cascaded
// to, within the grace: everything that hung off them works again. A live
// node is left as it is: changed is false.
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
	var parentStatus, parentKind string
	if n.ParentID != nil {
		if err := tx.QueryRow(ctx, `SELECT status, kind FROM nodes WHERE id = $1`, *n.ParentID).Scan(&parentStatus, &parentKind); err != nil {
			return nil, false, op.Fail(err)
		}
	}
	if !fits(snap, n.Kind, parentKind) {
		return nil, false, op.Expected("kind", errors.ConflictError(fmt.Sprintf("A %s can't be restored here.", n.Kind), "kind", n.Kind))
	}
	if n.ParentID != nil && parentStatus != StatusActive {
		return nil, false, op.Expected("parent deleted", errors.ConflictError(
			fmt.Sprintf("Restore the %s first.", parentKind), "parent_id", *n.ParentID))
	}

	rows, err := tx.Query(ctx, `
		SELECT n.id, n.kind, n.root_id, n.parent_id, COALESCE(p.kind, ''),
		       (SELECT count(*) - 1 FROM node_ancestors WHERE node_id = n.id) AS depth
		FROM nodes n LEFT JOIN nodes p ON p.id = n.parent_id
		WHERE n.status = 'deleted' AND n.deletion_id = $2
		  AND n.id IN (SELECT node_id FROM node_ancestors WHERE ancestor_id = $1)
		ORDER BY depth, n.id
	`, n.ID, *n.DeletionID)
	if err != nil {
		return nil, false, op.Fail(err)
	}
	restored, err := s.restoreTopDown(ctx, tx, snap, rows, n.ParentID)
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

// fits is whether a node of kind can sit under a parent of parentKind ("" for
// none) in this config.
func fits(snap *feed.Snapshot, kind, parentKind string) bool {
	k, ok := snap.Kind(kind)
	return ok && k.Parent == parentKind
}

// restoreTopDown restores candidates (rows of id, kind, root_id, parent_id,
// parent kind, depth, shallowest first). A node comes back only if it fits
// the config under its parent and that parent is live or coming back with it,
// so nothing live ever sits under a deleted node. topParent is the restored
// node's parent, live, or nil for a root.
func (s *Store) restoreTopDown(ctx context.Context, tx pgx.Tx, snap *feed.Snapshot, rows pgx.Rows, topParent *string) ([]changedNode, error) {
	type cand struct {
		changedNode
		parentID   *string
		parentKind string
	}
	cands, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (cand, error) {
		var c cand
		err := r.Scan(&c.id, &c.kind, &c.rootID, &c.parentID, &c.parentKind, &c.depth)
		return c, err
	})
	if err != nil {
		return nil, err
	}
	live := map[string]bool{}
	if topParent != nil {
		live[*topParent] = true
	}
	var restored []changedNode
	var ids []string
	for _, c := range cands {
		if !fits(snap, c.kind, c.parentKind) || (c.parentID != nil && !live[*c.parentID]) {
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

// ReleaseSlug frees a purged root's slug for a new root. It returns the slug
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
	if n.Status != StatusPurged || n.ParentID != nil {
		return nil, nil, op.Expected("not a purged root", errors.ConflictError("Only a purged root's slug can be released.", "status", n.Status))
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
	op.OK("ok")
	return total, nil
}

// purgeTree purges one tree's due nodes in one transaction: the nodes and the
// deleted nodes under them are marked purged, a purged node below the root
// frees its slug, their bindings and invites go, service accounts born at
// them go (principal, keys, member), and a purged root's members are removed.
// A person left with no bindings in the tree is removed.
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
		UPDATE nodes n SET status = 'purged', updated_at = $3,
		       slug = CASE WHEN n.parent_id IS NULL THEN n.slug END
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
		RETURNING m.id, m.principal
	`, ids, now); err != nil {
		return 0, err
	}
	if rootPurged {
		if err := removeMembers(`
			UPDATE members m SET removed_at = $2
			WHERE m.root_id = $1 AND m.removed_at IS NULL
			RETURNING m.id, m.principal
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
	// A person's member left with no bindings is removed.
	if err := removeMembers(`
		UPDATE members m SET removed_at = $2
		WHERE m.principal = 'person' AND m.id = ANY($1) AND m.removed_at IS NULL
		  AND NOT EXISTS (SELECT 1 FROM bindings b WHERE b.member_id = m.id)
		RETURNING m.id, m.principal
	`, touched, now); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM invites WHERE node_id = ANY($1)`, ids); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM principals WHERE kind = 'service_account' AND born_node_id = ANY($1)`, ids); err != nil {
		return 0, err
	}

	if err := recordNodeEvents(ctx, tx, lifecycle.TypeNodePurged, purged); err != nil {
		return 0, err
	}
	sort.Slice(removed, func(i, j int) bool { return removed[i].memberID < removed[j].memberID })
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

// Ancestor is one node above another, as the node shape lists it.
type Ancestor struct {
	ID          string
	Kind        string
	Slug        *string
	DisplayName string
}

// Ancestors reads each node's ancestors, root first, without the node itself.
func (s *Store) Ancestors(ctx context.Context, nodeIDs []string) (map[string][]Ancestor, error) {
	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "node_ancestors", dbx.DefaultTimeout)
	defer cancel()
	defer op.End()
	out := make(map[string][]Ancestor, len(nodeIDs))
	if len(nodeIDs) == 0 {
		op.OK("ok")
		return out, nil
	}
	rows, err := s.pool.Query(ctx, `
		SELECT a.node_id, n.id, n.kind, n.slug, n.display_name
		FROM node_ancestors a JOIN nodes n ON n.id = a.ancestor_id
		WHERE a.node_id = ANY($1) AND a.depth > 0
		ORDER BY a.node_id, a.depth DESC
	`, nodeIDs)
	if err != nil {
		return nil, op.Fail(err)
	}
	var nodeID string
	var an Ancestor
	if _, err := pgx.ForEachRow(rows, []any{&nodeID, &an.ID, &an.Kind, &an.Slug, &an.DisplayName}, func() error {
		out[nodeID] = append(out[nodeID], an)
		return nil
	}); err != nil {
		return nil, op.Fail(err)
	}
	op.OK("ok")
	return out, nil
}

// LockedNode takes the lock on the node's tree, and on alsoLock's trees in
// the same order, then reads the node.
func (s *Store) LockedNode(ctx context.Context, tx pgx.Tx, nodeID string, alsoLock ...string) (*Node, error) {
	if !id.Valid(nodeID) {
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

// NormalizeDisplayName trims a display name and checks it is 1 to 128
// characters.
func NormalizeDisplayName(raw string) (string, error) {
	name := strings.TrimSpace(raw)
	if name == "" {
		return "", errors.FieldError("display_name", "Display name is required.")
	}
	if utf8.RuneCountInString(name) > MaxDisplayNameLen {
		return "", errors.FieldError("display_name", "Display name is too long.")
	}
	return name, nil
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

func kindNotHere(kind string) error {
	return errors.FieldError("kind", fmt.Sprintf("A %s can't go here.", kind))
}

func parentNotLive(parent *Node) error {
	return errors.ConflictError(fmt.Sprintf("Restore the %s first.", parent.Kind), "parent_id", parent.ID)
}
