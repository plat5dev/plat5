package members

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	apierrors "github.com/plat5dev/plat5/identity/errors"
	"github.com/plat5dev/plat5/identity/feed"
	"github.com/plat5dev/plat5/identity/internal/auditx"
	"github.com/plat5dev/plat5/identity/internal/testdb"
	"github.com/plat5dev/plat5/identity/nodes"
)

// The rules of docs/concepts/access.md and docs/concepts/lifecycle.md#members,
// through the identity API (docs/reference/api/identity.md), against
// Postgres. TEST_DATABASE_URL runs them; without it they're skipped.

const testSchema = "identity_test_members"

// testConfig is a feed body: org > folder > project, roles for each case, and
// identity's read and delete routes. Folders have no read route.
const testConfig = `{
	"revision": 1, "deletion_grace": "30d", "reserved_slugs": ["new"],
	"kinds": {
		"directory": { "sign_in": "idp" },
		"organization": { "label": "Organization" },
		"folder": { "label": "Folder", "parent": "organization" },
		"project": { "label": "Project", "parent": ["organization", "folder"] }
	},
	"roles": {
		"owner": { "labels": ["*"], "bindable_at": ["organization", "folder", "project"] },
		"member": { "labels": ["organization:read"], "bindable_at": ["organization"] },
		"developer": { "labels": ["project:read", "project:write"], "bindable_at": ["organization", "project"] },
		"viewer": { "labels": ["project:read"], "bindable_at": ["project"] },
		"support": { "labels": ["project:read"], "bindable_at": ["organization"], "max_duration": "7d" },
		"gone_here": { "labels": ["project:read"], "bindable_at": ["organization"] }
	},
	"default_role": "member",
	"creator_role": "owner",
	"services": {
		"identity": { "url": "http://identity:3000", "routes": [
			{ "path": "/orgs/{organization}", "scope": "organization", "upstream": "/nodes/{subject.organization_id}", "methods": ["GET"], "required_labels": ["organization:read"] },
			{ "path": "/orgs/{organization}", "scope": "organization", "upstream": "/nodes/{subject.organization_id}", "methods": ["DELETE"], "required_labels": ["organization:delete"] },
			{ "path": "/orgs/{organization}/projects/{project}", "scope": "project", "upstream": "/nodes/{subject.project_id}", "methods": ["GET"], "required_labels": ["project:read"] },
			{ "path": "/orgs/{organization}/projects/{project}", "scope": "project", "upstream": "/nodes/{subject.project_id}", "methods": ["DELETE"], "required_labels": ["project:delete"] }
		] },
		"other": { "url": "http://other:3000", "routes": [
			{ "path": "/x/{organization}/folders/{folder}", "scope": "folder", "upstream": "/nodes/{subject.folder_id}", "methods": ["GET"], "required_labels": [] }
		] }
	}
}`

func parseConfig(t *testing.T, body string) *feed.Snapshot {
	t.Helper()
	s, err := feed.Parse([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }

type env struct {
	t      *testing.T
	ctx    context.Context
	pool   *pgxpool.Pool
	nodes  *nodes.Store
	store  *Store
	src    *feed.Source
	clock  *clock
	signIn *nodes.Node
	app    *fiber.App
	// internal is the internal port's /internal routes: admit and plan counts.
	internal *fiber.App
}

func setup(t *testing.T) *env {
	return setupWith(t, testConfig)
}

// setupWith is setup on another feed body.
func setupWith(t *testing.T, config string) *env {
	pool := testdb.Pool(t, testSchema)
	c := &clock{t: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)}
	snap := parseConfig(t, config)
	nodeStore := nodes.NewStore(pool).WithClock(c.now)
	signIn, err := nodeStore.EnsureSignIn(context.Background(), snap)
	if err != nil {
		t.Fatal(err)
	}
	e := &env{
		t: t, ctx: context.Background(), pool: pool, nodes: nodeStore, clock: c, signIn: signIn,
		src:   feed.Static(snap),
		store: NewStore(pool, nodeStore, signIn.ID, "plat5").WithClock(c.now),
	}
	e.app = fiber.New(fiber.Config{ErrorHandler: apierrors.FiberErrorHandler})
	nodes.NewHandler(nodeStore, e.src).Mount(e.app)
	NewHandler(e.store, e.src).Mount(e.app)
	e.internal = fiber.New(fiber.Config{ErrorHandler: apierrors.FiberErrorHandler})
	NewHandler(e.store, e.src).MountInternal(e.internal.Group("/internal"))
	nodes.NewHandler(nodeStore, e.src).MountInternal(e.internal.Group("/internal"))
	return e
}

func (e *env) snap() *feed.Snapshot { return e.src.Current() }

// --- fixtures ---

// person makes a person and their directory member, as first sign-in will.
func (e *env) person(subject string) string {
	e.t.Helper()
	w, err := e.store.begin(e.ctx)
	if err != nil {
		e.t.Fatal(err)
	}
	defer w.rollback(e.ctx)
	if err := nodes.LockTrees(e.ctx, w.tx, []string{e.signIn.ID}); err != nil {
		e.t.Fatal(err)
	}
	principalID, err := e.store.ensurePerson(e.ctx, w, "https://idp.test", subject)
	if err != nil {
		e.t.Fatal(err)
	}
	if err := w.commit(e.ctx); err != nil {
		e.t.Fatal(err)
	}
	var memberID string
	if err := e.pool.QueryRow(e.ctx, `SELECT id FROM members WHERE principal_id = $1 AND root_id = $2`, principalID, e.signIn.ID).Scan(&memberID); err != nil {
		e.t.Fatal(err)
	}
	return memberID
}

// org creates an org as dirMember, who becomes its owner. It returns the org
// and the creator's member in it.
func (e *env) org(dirMember, label string) (*nodes.Node, string) {
	e.t.Helper()
	n, m, err := e.store.CreateRoot(e.ctx, e.snap(), nodes.CreateInput{Kind: "organization", Label: label}, dirMember)
	if err != nil {
		e.t.Fatal(err)
	}
	return n, m.ID
}

// child creates a node with no member behind it.
func (e *env) child(parent *nodes.Node, kind, label string) *nodes.Node {
	e.t.Helper()
	n, err := e.nodes.Create(e.ctx, e.snap(), nodes.CreateInput{ParentID: parent.ID, Kind: kind, Label: label})
	if err != nil {
		e.t.Fatal(err)
	}
	return n
}

// join adds a person to a root with role. It returns their member there.
func (e *env) join(root *nodes.Node, subject, role string) string {
	e.t.Helper()
	m, _, err := e.store.AddMember(e.ctx, e.snap(), root.ID, AddMemberInput{Issuer: "https://idp.test", Subject: subject, Role: &role})
	if err != nil {
		e.t.Fatalf("join %s: %v", subject, err)
	}
	return m.ID
}

func (e *env) bind(n *nodes.Node, memberID, role string, expiresAt *time.Time) *Binding {
	e.t.Helper()
	b, err := e.store.CreateBinding(e.ctx, e.snap(), n.ID, memberID, role, expiresAt)
	if err != nil {
		e.t.Fatalf("bind %s on %s: %v", role, n.ID, err)
	}
	return b
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

func (e *env) removed(memberID string) bool {
	e.t.Helper()
	return e.count(`SELECT count(*) FROM members WHERE id = $1 AND removed_at IS NOT NULL`, memberID) == 1
}

// events is the lifecycle events after id after, as "type id" lines.
func (e *env) eventsAfter(after int64) []string {
	e.t.Helper()
	rows, err := e.pool.Query(e.ctx, `
		SELECT type, kind, COALESCE(node_id, member_id), root_id, principal FROM lifecycle_events WHERE id > $1 ORDER BY id
	`, after)
	if err != nil {
		e.t.Fatal(err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (string, error) {
		var typ, subject, root string
		var kind, principal *string
		if err := r.Scan(&typ, &kind, &subject, &root, &principal); err != nil {
			return "", err
		}
		if kind != nil {
			typ = *kind + strings.TrimPrefix(typ, "node")
		}
		if principal != nil {
			return fmt.Sprintf("%s %s %s %s", typ, subject, root, *principal), nil
		}
		return fmt.Sprintf("%s %s %s", typ, subject, root), nil
	})
	if err != nil {
		e.t.Fatal(err)
	}
	return out
}

func (e *env) lastEvent() int64 {
	e.t.Helper()
	var id int64
	if err := e.pool.QueryRow(e.ctx, `SELECT COALESCE(max(id), 0) FROM lifecycle_events`).Scan(&id); err != nil {
		e.t.Fatal(err)
	}
	return id
}

// --- HTTP ---

type response struct {
	status  int
	body    map[string]any
	headers map[string]string
}

func (r response) str(key string) string {
	s, _ := r.body[key].(string)
	return s
}

func (r response) list(key string) []map[string]any {
	items, _ := r.body[key].([]any)
	out := make([]map[string]any, 0, len(items))
	for _, it := range items {
		out = append(out, it.(map[string]any))
	}
	return out
}

func (r response) code() string {
	e, _ := r.body["error"].(map[string]any)
	c, _ := e["code"].(string)
	return c
}

func (r response) message() string {
	e, _ := r.body["error"].(map[string]any)
	m, _ := e["message"].(string)
	return m
}

// call sends a request as caller (X-Plat5-Member-Id, "" for none).
func (e *env) call(method, path, caller, body string) response {
	e.t.Helper()
	return e.send(e.app, method, path, caller, body)
}

// callInternal sends a request to the internal port.
func (e *env) callInternal(method, path, body string) response {
	e.t.Helper()
	return e.send(e.internal, method, path, "", body)
}

func (e *env) send(app *fiber.App, method, path, caller, body string) response {
	e.t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rd)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if caller != "" {
		req.Header.Set(MemberHeader, caller)
	}
	resp, err := app.Test(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	out := response{status: resp.StatusCode, headers: map[string]string{}}
	for _, h := range []string{auditx.Header, auditx.CreatedHeader, auditx.RootHeader, auditx.MemberHeader} {
		out.headers[h] = resp.Header.Get(h)
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &out.body); err != nil {
			e.t.Fatalf("%s %s: %s: %v", method, path, raw, err)
		}
	}
	return out
}

// expect fails unless r has status (and, for an error, code).
func (e *env) expect(r response, status int, what string) {
	e.t.Helper()
	if r.status != status {
		e.t.Fatalf("%s: status %d, want %d: %v", what, r.status, status, r.body)
	}
}

func ids(items []map[string]any) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it["id"].(string))
	}
	return out
}

func rfc(t time.Time) string { return t.UTC().Format(time.RFC3339) }
