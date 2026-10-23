package nodes

import (
	"context"
	stderrors "errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/plat5dev/plat5/identity/errors"
	"github.com/plat5dev/plat5/identity/feed"
	"github.com/plat5dev/plat5/identity/internal/id"
	"github.com/plat5dev/plat5/identity/internal/testdb"
)

// The invariants of docs/internals/identity-schema.md for nodes, against
// Postgres. TEST_DATABASE_URL runs them; without it they're skipped.

const testSchema = "identity_test_nodes"

const baseKinds = `
	"directory": { "sign_in": "idp" },
	"organization": { "label": "Organization" },
	"folder": { "label": "Folder", "parent": "organization" },
	"project": { "label": "Project", "parent": ["organization", "folder"] }`

func snapshot(t *testing.T, grace string, kinds string) *feed.Snapshot {
	t.Helper()
	s, err := feed.Parse([]byte(fmt.Sprintf(`{
		"revision": 1, "deletion_grace": %q, "reserved_slugs": ["new", "settings"],
		"kinds": { %s }
	}`, grace, kinds)))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }

type env struct {
	t     *testing.T
	ctx   context.Context
	pool  *pgxpool.Pool
	store *Store
	clock *clock
	snap  *feed.Snapshot
}

func setup(t *testing.T) *env {
	pool := testdb.Pool(t, testSchema)
	c := &clock{t: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)}
	return &env{
		t:     t,
		ctx:   context.Background(),
		pool:  pool,
		store: NewStore(pool).WithClock(c.now),
		clock: c,
		snap:  snapshot(t, "30d", baseKinds),
	}
}

func (e *env) create(parent *Node, kind, label string) *Node {
	e.t.Helper()
	in := CreateInput{Kind: kind, Label: label}
	if parent != nil {
		in.ParentID = parent.ID
	}
	n, err := e.store.Create(e.ctx, e.snap, in)
	if err != nil {
		e.t.Fatalf("create %s %q: %v", kind, label, err)
	}
	return n
}

func (e *env) get(n *Node) *Node {
	e.t.Helper()
	got, err := e.store.Get(e.ctx, n.ID)
	if err != nil {
		e.t.Fatal(err)
	}
	return got
}

func (e *env) delete(n *Node) {
	e.t.Helper()
	if _, err := e.store.Delete(e.ctx, e.snap, n.ID); err != nil {
		e.t.Fatalf("delete %s: %v", n.ID, err)
	}
}

func (e *env) exec(sql string, args ...any) {
	e.t.Helper()
	if _, err := e.pool.Exec(e.ctx, sql, args...); err != nil {
		e.t.Fatalf("%s: %v", sql, err)
	}
}

func (e *env) count(sql string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.pool.QueryRow(e.ctx, sql, args...).Scan(&n); err != nil {
		e.t.Fatalf("%s: %v", sql, err)
	}
	return n
}

// lastEventID marks the feed, so a test reads only the events after it.
func (e *env) lastEventID() int64 {
	e.t.Helper()
	var n int64
	if err := e.pool.QueryRow(e.ctx, `SELECT COALESCE(max(id), 0) FROM lifecycle_events`).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

// events after mark, as "<wire type> <node or member id>".
func (e *env) events(after int64) []string {
	e.t.Helper()
	rows, err := e.pool.Query(e.ctx, `
		SELECT CASE WHEN type LIKE 'node.%' THEN kind || substr(type, 5) ELSE type END,
		       COALESCE(node_id, member_id), COALESCE(principal, '')
		FROM lifecycle_events WHERE id > $1 ORDER BY id`, after)
	if err != nil {
		e.t.Fatal(err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (string, error) {
		var typ, ref, principal string
		err := r.Scan(&typ, &ref, &principal)
		if principal != "" {
			return typ + " " + ref + " " + principal, err
		}
		return typ + " " + ref, err
	})
	if err != nil {
		e.t.Fatal(err)
	}
	return out
}

func equal(t *testing.T, what string, got, want []string) {
	t.Helper()
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("%s:\n got  %v\n want %v", what, got, want)
	}
}

func wantAPIError(t *testing.T, err error, status int, code string) *errors.ApiError {
	t.Helper()
	var apiErr *errors.ApiError
	if !stderrors.As(err, &apiErr) || apiErr.Status != status || apiErr.Code != code {
		t.Fatalf("want %d %s, got %v", status, code, err)
	}
	return apiErr
}

// principal makes a person or a service account with an active member in
// root's tree, bound at each of nodes. It returns the member id.
func (e *env) principal(kind string, born, root *Node, nodes ...*Node) (principalID, memberID string) {
	e.t.Helper()
	principalID = "p_" + id.New("")
	var bornID *string
	if born != nil {
		bornID = &born.ID
	}
	e.exec(`INSERT INTO principals (id, kind, born_node_id) VALUES ($1, $2, $3)`, principalID, kind, bornID)
	memberID = id.New(id.Member)
	e.exec(`INSERT INTO members (id, principal_id, principal, root_id) VALUES ($1, $2, $3, $4)`, memberID, principalID, kind, root.ID)
	for _, n := range nodes {
		e.exec(`INSERT INTO bindings (id, member_id, node_id, role) VALUES ($1, $2, $3, 'admin')`, id.New(id.Binding), memberID, n.ID)
	}
	return principalID, memberID
}

func (e *env) key(principalID string) string {
	e.t.Helper()
	keyID := id.New(id.Key)
	e.exec(`INSERT INTO api_keys (id, principal_id, name, key_prefix, key_hash) VALUES ($1, $2, 'k', 'plat5-k-2-AAAA', $1)`, keyID, principalID)
	return keyID
}

func TestCreateWritesTheClosureTable(t *testing.T) {
	e := setup(t)
	mark := e.lastEventID()
	org := e.create(nil, "organization", "Gruow")
	folder := e.create(org, "folder", "Apps")
	project := e.create(folder, "project", "Gatheroom")

	if org.RootID != org.ID || org.ParentID != nil || folder.RootID != org.ID || project.RootID != org.ID {
		t.Fatalf("roots: %+v %+v %+v", org, folder, project)
	}
	if *project.ParentID != folder.ID || *project.Slug != "gatheroom" || project.Status != StatusActive {
		t.Fatalf("project: %+v", project)
	}
	rows, err := e.pool.Query(e.ctx, `SELECT ancestor_id, depth FROM node_ancestors WHERE node_id = $1 ORDER BY depth`, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	got, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (string, error) {
		var a string
		var d int
		err := r.Scan(&a, &d)
		return fmt.Sprintf("%s@%d", a, d), err
	})
	if err != nil {
		t.Fatal(err)
	}
	equal(t, "ancestors", got, []string{project.ID + "@0", folder.ID + "@1", org.ID + "@2"})
	equal(t, "events", e.events(mark), []string{
		"organization.created " + org.ID, "folder.created " + folder.ID, "project.created " + project.ID,
	})
}

func TestCreateKindRules(t *testing.T) {
	e := setup(t)
	org := e.create(nil, "organization", "Gruow")
	project := e.create(org, "project", "Gatheroom")

	for _, tc := range []struct {
		name string
		in   CreateInput
	}{
		{"a non-root kind as a root", CreateInput{Kind: "project", Label: "P"}},
		{"a root kind under a node", CreateInput{ParentID: org.ID, Kind: "organization", Label: "O"}},
		{"a kind under a parent it doesn't allow", CreateInput{ParentID: project.ID, Kind: "folder", Label: "F"}},
		{"the sign-in kind", CreateInput{Kind: "directory", Label: "D"}},
		{"a kind not in the config", CreateInput{Kind: "team", Label: "T"}},
	} {
		_, err := e.store.Create(e.ctx, e.snap, tc.in)
		apiErr := wantAPIError(t, err, 422, "VALIDATION_ERROR")
		if tc.name == "a kind under a parent it doesn't allow" && apiErr.Message != "A folder can't go here." {
			t.Fatalf("%s: message %q", tc.name, apiErr.Message)
		}
	}
	if _, err := e.store.Create(e.ctx, e.snap, CreateInput{ParentID: id.New(id.Node), Kind: "project", Label: "P"}); !stderrors.Is(err, ErrNotFound) {
		t.Fatalf("missing parent: %v", err)
	}
	for _, label := range []string{"", "   ", string(make([]rune, 129))} {
		_, err := e.store.Create(e.ctx, e.snap, CreateInput{Kind: "organization", Label: label})
		wantAPIError(t, err, 422, "VALIDATION_ERROR")
	}
}

func TestCreateUnderADeletedParentIsRefused(t *testing.T) {
	e := setup(t)
	org := e.create(nil, "organization", "Gruow")
	e.delete(org)
	_, err := e.store.Create(e.ctx, e.snap, CreateInput{ParentID: org.ID, Kind: "project", Label: "P"})
	apiErr := wantAPIError(t, err, 409, "CONFLICT")
	if apiErr.Message != "Restore the organization first." {
		t.Fatalf("message %q", apiErr.Message)
	}
}

func TestSlugsAreUniquePerParentAndKind(t *testing.T) {
	e := setup(t)
	slug := func(s string) *string { return &s }
	org := e.create(nil, "organization", "Gruow")
	other := e.create(nil, "organization", "Kyvera")

	in := CreateInput{ParentID: org.ID, Kind: "project", Label: "P", Slug: slug("web")}
	if _, err := e.store.Create(e.ctx, e.snap, in); err != nil {
		t.Fatal(err)
	}
	_, err := e.store.Create(e.ctx, e.snap, in)
	wantAPIError(t, err, 409, "CONFLICT")

	// Another parent, or another kind under the same parent, may use it.
	if _, err := e.store.Create(e.ctx, e.snap, CreateInput{ParentID: other.ID, Kind: "project", Label: "P", Slug: slug("web")}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.Create(e.ctx, e.snap, CreateInput{ParentID: org.ID, Kind: "folder", Label: "F", Slug: slug("web")}); err != nil {
		t.Fatal(err)
	}

	// Roots share one namespace per kind: org slugs are deployment-wide.
	_, err = e.store.Create(e.ctx, e.snap, CreateInput{Kind: "organization", Label: "Again", Slug: slug("gruow")})
	wantAPIError(t, err, 409, "CONFLICT")

	for _, bad := range []string{"ab", "1abc", "has_underscore", "Upper", "trailing-"} {
		_, err := e.store.Create(e.ctx, e.snap, CreateInput{ParentID: org.ID, Kind: "project", Label: "P", Slug: slug(bad)})
		wantAPIError(t, err, 422, "VALIDATION_ERROR")
	}
	_, err = e.store.Create(e.ctx, e.snap, CreateInput{ParentID: org.ID, Kind: "project", Label: "P", Slug: slug("settings")})
	if apiErr := wantAPIError(t, err, 422, "VALIDATION_ERROR"); apiErr.Message != "That name is reserved." {
		t.Fatalf("reserved: %q", apiErr.Message)
	}
}

func TestGeneratedSlugs(t *testing.T) {
	e := setup(t)
	org := e.create(nil, "organization", "Gruow")
	var got []string
	for _, label := range []string{"Gather Room", "Gather Room", "gather room!", "42", "New", "New"} {
		got = append(got, *e.create(org, "project", label).Slug)
	}
	equal(t, "slugs", got, []string{"gather-room", "gather-room-2", "gather-room-3", "project-42", "new-2", "new-3"})

	if s := *e.create(nil, "organization", "Gruow").Slug; s != "gruow-2" {
		t.Fatalf("second root: %q", s)
	}
}

func TestPurgedSlugsStayTaken(t *testing.T) {
	e := setup(t)
	org := e.create(nil, "organization", "Gruow")
	old := e.create(org, "project", "Web")
	e.delete(old)
	e.clock.advance(31 * 24 * time.Hour)
	if _, err := e.store.PurgeDue(e.ctx); err != nil {
		t.Fatal(err)
	}
	if e.get(old).Status != StatusPurged {
		t.Fatal("not purged")
	}

	web := "web"
	_, err := e.store.Create(e.ctx, e.snap, CreateInput{ParentID: org.ID, Kind: "project", Label: "Web", Slug: &web})
	wantAPIError(t, err, 409, "CONFLICT")
	if s := *e.create(org, "project", "Web").Slug; s != "web-2" {
		t.Fatalf("generated past a tombstone: %q", s)
	}

	// Released by hand, the slug is free again.
	freed, n, err := e.store.ReleaseSlug(e.ctx, old.ID)
	if err != nil || *freed != "web" || n.Slug != nil {
		t.Fatalf("release: %v %v %+v", freed, err, n)
	}
	if _, err := e.store.Create(e.ctx, e.snap, CreateInput{ParentID: org.ID, Kind: "project", Label: "Web", Slug: &web}); err != nil {
		t.Fatal(err)
	}
}

func TestReleaseSlugOnlyOnAPurgedNode(t *testing.T) {
	e := setup(t)
	org := e.create(nil, "organization", "Gruow")
	_, _, err := e.store.ReleaseSlug(e.ctx, org.ID)
	wantAPIError(t, err, 409, "CONFLICT")
	e.delete(org)
	_, _, err = e.store.ReleaseSlug(e.ctx, org.ID)
	wantAPIError(t, err, 409, "CONFLICT")
}

func TestDeleteCascadesUnderOneDeletionID(t *testing.T) {
	e := setup(t)
	org := e.create(nil, "organization", "Gruow")
	folder := e.create(org, "folder", "Apps")
	p1 := e.create(folder, "project", "One")
	p2 := e.create(org, "project", "Two")
	e.delete(p2)
	earlier := e.get(p2)

	e.clock.advance(time.Hour)
	mark := e.lastEventID()
	e.delete(org)

	o, f, a, b := e.get(org), e.get(folder), e.get(p1), e.get(p2)
	for _, n := range []*Node{o, f, a} {
		if n.Status != StatusDeleted || *n.DeletionID != *o.DeletionID {
			t.Fatalf("%s: %s %v", n.Kind, n.Status, n.DeletionID)
		}
		if !n.PurgeAfter.Equal(e.clock.t.Add(30 * 24 * time.Hour)) {
			t.Fatalf("%s purge_after %v", n.Kind, n.PurgeAfter)
		}
	}
	// Already deleted: keeps its own id and grace.
	if *b.DeletionID != *earlier.DeletionID || !b.PurgeAfter.Equal(*earlier.PurgeAfter) {
		t.Fatalf("p2 changed: %+v", b)
	}
	equal(t, "events", e.events(mark), []string{
		"organization.deleted " + org.ID, "folder.deleted " + folder.ID, "project.deleted " + p1.ID,
	})
}

func TestDeletingAgainChangesNothing(t *testing.T) {
	e := setup(t)
	org := e.create(nil, "organization", "Gruow")
	e.delete(org)
	first := e.get(org)

	e.clock.advance(5 * 24 * time.Hour)
	mark := e.lastEventID()
	changed, err := e.store.Delete(e.ctx, e.snap, org.ID)
	if err != nil || changed {
		t.Fatalf("second delete: %v %v", changed, err)
	}
	again := e.get(org)
	if *again.DeletionID != *first.DeletionID || !again.PurgeAfter.Equal(*first.PurgeAfter) {
		t.Fatalf("second delete moved the grace: %+v", again)
	}
	equal(t, "events", e.events(mark), nil)
}

func TestTheSignInNodeCantBeDeleted(t *testing.T) {
	e := setup(t)
	dir, err := e.store.EnsureSignIn(e.ctx, e.snap)
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.store.Delete(e.ctx, e.snap, dir.ID)
	if apiErr := wantAPIError(t, err, 422, "VALIDATION_ERROR"); apiErr.Message != "The directory can't be deleted." {
		t.Fatalf("message %q", apiErr.Message)
	}
}

func TestEnsureSignInMakesOneNode(t *testing.T) {
	e := setup(t)
	mark := e.lastEventID()
	first, err := e.store.EnsureSignIn(e.ctx, e.snap)
	if err != nil {
		t.Fatal(err)
	}
	second, err := e.store.EnsureSignIn(e.ctx, e.snap)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != second.ID || first.Slug != nil || first.ParentID != nil || first.RootID != first.ID || first.Label != "directory" {
		t.Fatalf("sign-in node: %+v %+v", first, second)
	}
	equal(t, "events", e.events(mark), []string{"directory.created " + first.ID})

	// A label on the kind is the node's label.
	e2 := setup(t)
	e2.snap = snapshot(t, "30d", `"people": { "sign_in": "idp", "label": "People" }, "organization": {}`)
	n, err := e2.store.EnsureSignIn(e2.ctx, e2.snap)
	if err != nil || n.Label != "People" || n.Kind != "people" {
		t.Fatalf("labelled: %+v %v", n, err)
	}
}

func TestDeleteRevokesKeysAndRestoreKeepsThemRevoked(t *testing.T) {
	e := setup(t)
	org := e.create(nil, "organization", "Gruow")
	project := e.create(org, "project", "Web")
	sa, _ := e.principal("service_account", project, org, project)
	saKey := e.key(sa)
	person, _ := e.principal("person", nil, org, project)
	personKey := e.key(person)

	e.delete(project)
	revoked := func(keyID string) bool {
		return e.count(`SELECT count(*) FROM api_keys WHERE id = $1 AND revoked_at IS NOT NULL`, keyID) == 1
	}
	if !revoked(saKey) || revoked(personKey) {
		t.Fatalf("after delete: sa %v person %v", revoked(saKey), revoked(personKey))
	}
	if _, _, err := e.store.Restore(e.ctx, e.snap, project.ID); err != nil {
		t.Fatal(err)
	}
	if !revoked(saKey) {
		t.Fatal("restore brought a key back")
	}
	if e.count(`SELECT count(*) FROM bindings WHERE node_id = $1`, project.ID) != 2 {
		t.Fatal("bindings must survive delete and restore")
	}
}

func TestRestoreUndoesExactlyItsDelete(t *testing.T) {
	e := setup(t)
	org := e.create(nil, "organization", "Gruow")
	folder := e.create(org, "folder", "Apps")
	kept := e.create(folder, "project", "Kept")
	gone := e.create(folder, "project", "Gone")
	e.delete(gone)
	e.delete(org)

	mark := e.lastEventID()
	n, changed, err := e.store.Restore(e.ctx, e.snap, org.ID)
	if err != nil || !changed || n.Status != StatusActive || n.DeletedAt != nil {
		t.Fatalf("restore: %+v %v %v", n, changed, err)
	}
	for _, n := range []*Node{e.get(org), e.get(folder), e.get(kept)} {
		if n.Status != StatusActive || n.DeletionID != nil || n.PurgeAfter != nil {
			t.Fatalf("%s %s not restored: %+v", n.Kind, n.Label, n)
		}
	}
	if e.get(gone).Status != StatusDeleted {
		t.Fatal("a node deleted separately came back")
	}
	equal(t, "events", e.events(mark), []string{
		"organization.restored " + org.ID, "folder.restored " + folder.ID, "project.restored " + kept.ID,
	})
}

func TestRestoreConflicts(t *testing.T) {
	e := setup(t)
	org := e.create(nil, "organization", "Gruow")
	project := e.create(org, "project", "Web")

	// Live: succeeds, changes nothing.
	mark := e.lastEventID()
	n, changed, err := e.store.Restore(e.ctx, e.snap, project.ID)
	if err != nil || changed || n.Status != StatusActive {
		t.Fatalf("live: %v %v", changed, err)
	}
	equal(t, "events", e.events(mark), nil)

	// Parent deleted: 409, and the parent isn't restored for it.
	e.delete(org)
	_, _, err = e.store.Restore(e.ctx, e.snap, project.ID)
	if apiErr := wantAPIError(t, err, 409, "CONFLICT"); apiErr.Message != "Restore the organization first." {
		t.Fatalf("message %q", apiErr.Message)
	}
	if e.get(org).Status != StatusDeleted {
		t.Fatal("parent restored")
	}

	// Kind not in the config: 409.
	noProjects := snapshot(t, "30d", `"directory": { "sign_in": "idp" }, "organization": {}, "folder": { "parent": "organization" }`)
	if _, _, err := e.store.Restore(e.ctx, e.snap, org.ID); err != nil {
		t.Fatal(err)
	}
	e.delete(project)
	_, _, err = e.store.Restore(e.ctx, noProjects, project.ID)
	wantAPIError(t, err, 409, "CONFLICT")

	// Past the grace, even before the sweeper ran: 409.
	e.delete(org)
	e.clock.advance(30 * 24 * time.Hour)
	_, _, err = e.store.Restore(e.ctx, e.snap, org.ID)
	if apiErr := wantAPIError(t, err, 409, "CONFLICT"); apiErr.Message != "This can no longer be restored." {
		t.Fatalf("message %q", apiErr.Message)
	}
}

func TestGraceIsFixedAtDelete(t *testing.T) {
	e := setup(t)
	org := e.create(nil, "organization", "Gruow")
	a := e.create(org, "project", "A")
	b := e.create(org, "project", "B")
	e.delete(a)
	e.snap = snapshot(t, "1d", baseKinds)
	e.delete(b)

	if !e.get(a).PurgeAfter.Equal(e.clock.t.Add(30 * 24 * time.Hour)) {
		t.Fatal("a changed grace moved an earlier delete")
	}
	if !e.get(b).PurgeAfter.Equal(e.clock.t.Add(24 * time.Hour)) {
		t.Fatal("a new delete didn't take the new grace")
	}
	e.clock.advance(2 * 24 * time.Hour)
	if _, err := e.store.PurgeDue(e.ctx); err != nil {
		t.Fatal(err)
	}
	if e.get(a).Status != StatusDeleted || e.get(b).Status != StatusPurged {
		t.Fatalf("after 2d: a %s b %s", e.get(a).Status, e.get(b).Status)
	}
}

func TestPurgeAfterTheGrace(t *testing.T) {
	e := setup(t)
	org := e.create(nil, "organization", "Gruow")
	project := e.create(org, "project", "Web")
	other := e.create(org, "project", "Other")
	_, onlyHere := e.principal("person", nil, org, project)
	_, alsoOrg := e.principal("person", nil, org, project, org)
	sa, saMember := e.principal("service_account", project, org, project, other)
	e.key(sa)
	e.delete(project)

	// Not yet: the grace hasn't passed.
	e.clock.advance(30*24*time.Hour - time.Second)
	if n, err := e.store.PurgeDue(e.ctx); err != nil || n != 0 {
		t.Fatalf("early purge: %d %v", n, err)
	}

	e.clock.advance(time.Second)
	mark := e.lastEventID()
	if n, err := e.store.PurgeDue(e.ctx); err != nil || n != 1 {
		t.Fatalf("purge: %d %v", n, err)
	}
	if p := e.get(project); p.Status != StatusPurged || *p.Slug != "web" {
		t.Fatalf("tombstone: %+v", p)
	}
	if e.count(`SELECT count(*) FROM bindings WHERE node_id = $1`, project.ID) != 0 {
		t.Fatal("bindings on the purged node remain")
	}
	if e.count(`SELECT count(*) FROM principals WHERE id = $1`, sa) != 0 || e.count(`SELECT count(*) FROM api_keys WHERE principal_id = $1`, sa) != 0 {
		t.Fatal("the service account born there remains")
	}
	if e.count(`SELECT count(*) FROM bindings WHERE member_id = $1`, saMember) != 0 {
		t.Fatal("the service account's other bindings remain")
	}
	removed := func(memberID string) bool {
		return e.count(`SELECT count(*) FROM members WHERE id = $1 AND removed_at IS NOT NULL`, memberID) == 1
	}
	if !removed(onlyHere) || removed(alsoOrg) || !removed(saMember) {
		t.Fatalf("removed: onlyHere %v alsoOrg %v sa %v", removed(onlyHere), removed(alsoOrg), removed(saMember))
	}
	equal(t, "events", e.events(mark), []string{
		"project.purged " + project.ID,
		"member.removed " + saMember + " service_account",
		"member.removed " + onlyHere + " person",
	})

	// Purged is final: nothing more on the next sweep.
	mark = e.lastEventID()
	if n, err := e.store.PurgeDue(e.ctx); err != nil || n != 0 {
		t.Fatalf("second sweep: %d %v", n, err)
	}
	equal(t, "events", e.events(mark), nil)
}

func TestPurgingARootRemovesItsMembers(t *testing.T) {
	e := setup(t)
	dir, err := e.store.EnsureSignIn(e.ctx, e.snap)
	if err != nil {
		t.Fatal(err)
	}
	org := e.create(nil, "organization", "Gruow")
	project := e.create(org, "project", "Web")
	person, member := e.principal("person", nil, org, org)
	directoryMember := id.New(id.Member)
	e.exec(`INSERT INTO members (id, principal_id, principal, root_id) VALUES ($1, $2, 'person', $3)`, directoryMember, person, dir.ID)
	_, saMember := e.principal("service_account", org, org, project)
	e.delete(org)
	e.clock.advance(31 * 24 * time.Hour)

	mark := e.lastEventID()
	if n, err := e.store.PurgeDue(e.ctx); err != nil || n != 2 {
		t.Fatalf("purge: %d %v", n, err)
	}
	if e.count(`SELECT count(*) FROM members WHERE root_id = $1 AND removed_at IS NULL`, org.ID) != 0 {
		t.Fatal("members of the purged root remain")
	}
	if e.count(`SELECT count(*) FROM members WHERE id = $1 AND removed_at IS NULL`, directoryMember) != 1 {
		t.Fatal("the person's directory member was removed")
	}
	equal(t, "events", e.events(mark), []string{
		"organization.purged " + org.ID,
		"project.purged " + project.ID,
		"member.removed " + saMember + " service_account",
		"member.removed " + member + " person",
	})
}

func TestPurgeTakesTheDeletedNodesBelow(t *testing.T) {
	e := setup(t)
	org := e.create(nil, "organization", "Gruow")
	project := e.create(org, "project", "Web")
	e.delete(project) // 30d grace
	e.snap = snapshot(t, "1d", baseKinds)
	e.delete(org) // 1d grace, so the org is due first
	e.clock.advance(2 * 24 * time.Hour)

	if n, err := e.store.PurgeDue(e.ctx); err != nil || n != 2 {
		t.Fatalf("purge: %d %v", n, err)
	}
	if e.get(project).Status != StatusPurged {
		t.Fatal("a deleted node under a purged one was left restorable")
	}
}

func TestKindRemovalAndReapply(t *testing.T) {
	e := setup(t)
	org := e.create(nil, "organization", "Gruow")
	f1 := e.create(org, "folder", "Apps")
	underF1 := e.create(f1, "project", "In a folder")
	f2 := e.create(org, "folder", "Old")
	direct := e.create(org, "project", "Direct")
	e.delete(f2)
	before := e.get(f2)

	noFolders := snapshot(t, "30d", `
		"directory": { "sign_in": "idp" },
		"organization": { "label": "Organization" },
		"project": { "label": "Project", "parent": "organization" }`)
	mark := e.lastEventID()
	if err := e.store.ReconcileKinds(e.ctx, noFolders); err != nil {
		t.Fatal(err)
	}
	var removal string
	if err := e.pool.QueryRow(e.ctx, `SELECT deletion_id FROM kind_removals WHERE kind = 'folder'`).Scan(&removal); err != nil {
		t.Fatalf("removal not recorded: %v", err)
	}
	for _, n := range []*Node{e.get(f1), e.get(underF1)} {
		if n.Status != StatusDeleted || *n.DeletionID != removal {
			t.Fatalf("%s: %+v", n.Label, n)
		}
	}
	if b := e.get(f2); *b.DeletionID != *before.DeletionID {
		t.Fatal("a folder deleted before the removal took its id")
	}
	if e.get(direct).Status != StatusActive {
		t.Fatal("a project outside any folder was deleted")
	}
	equal(t, "removal events", e.events(mark), []string{"folder.deleted " + f1.ID, "project.deleted " + underF1.ID})

	// Every config is checked; nothing more happens on the same one.
	mark = e.lastEventID()
	if err := e.store.ReconcileKinds(e.ctx, noFolders); err != nil {
		t.Fatal(err)
	}
	equal(t, "repeat", e.events(mark), nil)

	// Applied again: the removal's nodes come back, not the earlier delete.
	if err := e.store.ReconcileKinds(e.ctx, e.snap); err != nil {
		t.Fatal(err)
	}
	if e.get(f1).Status != StatusActive || e.get(underF1).Status != StatusActive || e.get(f2).Status != StatusDeleted {
		t.Fatalf("reapply: f1 %s underF1 %s f2 %s", e.get(f1).Status, e.get(underF1).Status, e.get(f2).Status)
	}
	if e.count(`SELECT count(*) FROM kind_removals`) != 0 {
		t.Fatal("the removal wasn't forgotten")
	}
	equal(t, "reapply events", e.events(mark), []string{"folder.restored " + f1.ID, "project.restored " + underF1.ID})
}

func TestKindReapplySkipsNodesUnderAParentDeletedSince(t *testing.T) {
	e := setup(t)
	org := e.create(nil, "organization", "Gruow")
	folder := e.create(org, "folder", "Apps")
	noFolders := snapshot(t, "30d", `"directory": { "sign_in": "idp" }, "organization": {}, "project": { "parent": "organization" }`)
	if err := e.store.ReconcileKinds(e.ctx, noFolders); err != nil {
		t.Fatal(err)
	}
	e.delete(org)
	if err := e.store.ReconcileKinds(e.ctx, e.snap); err != nil {
		t.Fatal(err)
	}
	if e.get(folder).Status != StatusDeleted {
		t.Fatal("a folder came back under a deleted org")
	}
}

func TestKindRemovalIsForgottenOncePurged(t *testing.T) {
	e := setup(t)
	org := e.create(nil, "organization", "Gruow")
	folder := e.create(org, "folder", "Apps")
	noFolders := snapshot(t, "30d", `"directory": { "sign_in": "idp" }, "organization": {}, "project": { "parent": "organization" }`)
	if err := e.store.ReconcileKinds(e.ctx, noFolders); err != nil {
		t.Fatal(err)
	}
	e.clock.advance(31 * 24 * time.Hour)
	if _, err := e.store.PurgeDue(e.ctx); err != nil {
		t.Fatal(err)
	}
	if e.count(`SELECT count(*) FROM kind_removals`) != 0 {
		t.Fatal("the removal outlived its nodes")
	}
	mark := e.lastEventID()
	if err := e.store.ReconcileKinds(e.ctx, e.snap); err != nil {
		t.Fatal(err)
	}
	equal(t, "reapply after purge", e.events(mark), nil)
	if e.get(folder).Status != StatusPurged {
		t.Fatal("purged folder changed")
	}
}

func TestUpdateLabel(t *testing.T) {
	e := setup(t)
	org := e.create(nil, "organization", "Gruow")
	e.clock.advance(time.Minute)
	before, n, err := e.store.UpdateLabel(e.ctx, org.ID, "  Gruow Inc  ")
	if err != nil || before != "Gruow" || n.Label != "Gruow Inc" || *n.Slug != "gruow" || !n.UpdatedAt.Equal(e.clock.t) {
		t.Fatalf("update: %q %+v %v", before, n, err)
	}
	if _, _, err := e.store.UpdateLabel(e.ctx, id.New(id.Node), "X"); !stderrors.Is(err, ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
}

func TestList(t *testing.T) {
	e := setup(t)
	org := e.create(nil, "organization", "Gruow")
	a := e.create(org, "project", "A")
	b := e.create(org, "project", "B")
	e.delete(b)

	all, more, err := e.store.List(e.ctx, ListFilter{ParentID: org.ID}, 1, "")
	if err != nil || len(all) != 1 || !more || all[0].ID != a.ID {
		t.Fatalf("page 1: %v %v %v", all, more, err)
	}
	rest, more, err := e.store.List(e.ctx, ListFilter{ParentID: org.ID}, 1, a.ID)
	if err != nil || len(rest) != 1 || more || rest[0].ID != b.ID {
		t.Fatalf("page 2: %v %v %v", rest, more, err)
	}
	deleted, _, err := e.store.List(e.ctx, ListFilter{Kind: "project", Status: StatusDeleted}, 50, "")
	if err != nil || len(deleted) != 1 || deleted[0].ID != b.ID {
		t.Fatalf("deleted: %v %v", deleted, err)
	}
}
