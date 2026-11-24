package access_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/plat5dev/plat5/identity/access"
	apierrors "github.com/plat5dev/plat5/identity/errors"
	"github.com/plat5dev/plat5/identity/feed"
	"github.com/plat5dev/plat5/identity/internal/testdb"
	"github.com/plat5dev/plat5/identity/members"
	"github.com/plat5dev/plat5/identity/nodes"
)

const testSchema = "identity_test_access"

const testConfig = `{
	"revision": 1, "deletion_grace": "30d", "reserved_slugs": ["new"],
	"kinds": {
		"organization": { "path": "/orgs/{organization}", "list_permission": "organization:read" },
		"folder": { "parent": "organization", "path": "/folders/{folder}", "list_permission": "folder:read" },
		"project": { "parent": "folder", "path": "/projects/{project}", "list_permission": "project:read" }
	},
	"roles": {
		"admin": { "permissions": ["*"], "bindable_at": ["organization", "folder", "project"] },
		"member": { "permissions": ["organization:read"], "bindable_at": ["organization"] },
		"developer": { "includes": ["member"], "permissions": ["project:read", "project:write"], "bindable_at": ["organization", "folder", "project"] },
		"viewer": { "permissions": ["project:read"], "bindable_at": ["project"] },
		"browser": { "permissions": ["folder:read"], "bindable_at": ["organization", "folder"] },
		"nobody": { "permissions": [], "bindable_at": ["organization"] },
		"gone_here": { "permissions": ["project:read"], "bindable_at": ["organization"] }
	},
	"creator_role": "admin"
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

func (c *clock) now() time.Time { return c.t }

type env struct {
	t        *testing.T
	ctx      context.Context
	pool     *pgxpool.Pool
	nodes    *nodes.Store
	members  *members.Store
	access   *access.Store
	src      *feed.Source
	clock    *clock
	app      *fiber.App
	internal *fiber.App
}

func setup(t *testing.T) *env {
	pool := testdb.Pool(t, testSchema)
	c := &clock{t: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)}
	nodeStore := nodes.NewStore(pool).WithClock(c.now)
	memberStore := members.NewStore(pool, nodeStore, "plat5").WithClock(c.now)
	accessStore := access.NewStore(pool, "plat5", memberStore).WithClock(c.now)
	e := &env{
		t: t, ctx: context.Background(), pool: pool, nodes: nodeStore,
		members: memberStore, access: accessStore, clock: c,
		src: feed.Static(parseConfig(t, testConfig)),
	}
	e.app = fiber.New(fiber.Config{ErrorHandler: apierrors.FiberErrorHandler})
	nodes.NewHandler(nodeStore, e.src).Mount(e.app)
	members.NewHandler(memberStore, e.src).Mount(e.app)
	e.internal = fiber.New(fiber.Config{ErrorHandler: apierrors.FiberErrorHandler})
	ah := access.NewHandler(accessStore, e.src)
	ah.MountInternal(e.internal.Group("/internal"))
	nodes.NewHandler(nodeStore, e.src).MountInternal(e.internal.Group("/internal"))
	ah.MountAccess(e.internal.Group("/access/v1"))
	return e
}

func (e *env) snap() *feed.Snapshot { return e.src.Current() }

func (e *env) person(subject string) string {
	e.t.Helper()
	_, userID, _, err := e.members.FirstSignIn(e.ctx, "https://idp.test", subject, "")
	if err != nil {
		e.t.Fatal(err)
	}
	return userID
}

func (e *env) userOf(subject string) string {
	e.t.Helper()
	var userID string
	if err := e.pool.QueryRow(e.ctx, `
		SELECT p.user_id FROM principals p JOIN idp_links l ON l.principal_id = p.id WHERE l.subject = $1
	`, subject).Scan(&userID); err != nil {
		e.t.Fatalf("user of %s: %v", subject, err)
	}
	return userID
}

func (e *env) org(userID, name string) (*nodes.Node, string) {
	e.t.Helper()
	n, memberID, err := e.members.CreateRoot(e.ctx, e.snap(), nodes.CreateInput{Kind: "organization", DisplayName: name}, userID)
	if err != nil {
		e.t.Fatal(err)
	}
	return n, memberID
}

func (e *env) child(parent *nodes.Node, kind, name string) *nodes.Node {
	e.t.Helper()
	n, err := e.nodes.Create(e.ctx, e.snap(), nodes.CreateInput{ParentID: parent.ID, Kind: kind, DisplayName: name})
	if err != nil {
		e.t.Fatal(err)
	}
	return n
}

func (e *env) join(root *nodes.Node, subject, role string) string {
	e.t.Helper()
	m, _, err := e.members.AddMember(e.ctx, e.snap(), root.ID, members.AddMemberInput{Issuer: "https://idp.test", Subject: subject, Role: role})
	if err != nil {
		e.t.Fatalf("join %s: %v", subject, err)
	}
	return m.ID
}

func (e *env) bind(n *nodes.Node, memberID, role string) *members.Binding {
	e.t.Helper()
	b, err := e.members.CreateBinding(e.ctx, e.snap(), n.ID, memberID, role, nil)
	if err != nil {
		e.t.Fatalf("bind %s on %s: %v", role, n.ID, err)
	}
	return b
}

func (e *env) bindingOf(memberID, nodeID string) string {
	e.t.Helper()
	var id string
	if err := e.pool.QueryRow(e.ctx, `SELECT id FROM bindings WHERE member_id = $1 AND node_id = $2`, memberID, nodeID).Scan(&id); err != nil {
		e.t.Fatal(err)
	}
	return id
}

func (e *env) key(owner members.KeyOwner) string {
	e.t.Helper()
	k, err := e.members.CreateKey(e.ctx, owner, members.KeyCreate{})
	if err != nil {
		e.t.Fatal(err)
	}
	return k.Secret
}

func (e *env) serviceAccount(n *nodes.Node, role string) (string, string) {
	e.t.Helper()
	sa, _, err := e.members.CreateServiceAccount(e.ctx, e.snap(), n.ID, "ci", role)
	if err != nil {
		e.t.Fatal(err)
	}
	return sa.ID, e.key(members.KeyOwner{NodeID: n.ID, MemberID: sa.ID})
}

func (e *env) invite(n *nodes.Node, role string, grants ...members.Grant) *members.Invite {
	e.t.Helper()
	one := 1
	inv, err := e.members.CreateInvite(e.ctx, e.snap(), n.ID, members.InviteInput{Role: role, Grants: grants, MaxUses: &one, ExpiresIn: 24 * time.Hour})
	if err != nil {
		e.t.Fatal(err)
	}
	return inv
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

func (e *env) lastEvent() int64 {
	e.t.Helper()
	var id int64
	if err := e.pool.QueryRow(e.ctx, `SELECT COALESCE(max(id), 0) FROM events`).Scan(&id); err != nil {
		e.t.Fatal(err)
	}
	return id
}

func (e *env) eventsAfter(after int64) []string {
	e.t.Helper()
	rows, err := e.pool.Query(e.ctx, `
		SELECT type, kind, COALESCE(node_id, member_id), root_id, principal FROM events
		WHERE id > $1 AND type NOT LIKE 'user.%' ORDER BY id
	`, after)
	if err != nil {
		e.t.Fatal(err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (string, error) {
		var typ, ref, root string
		var kind, principal *string
		if err := r.Scan(&typ, &kind, &ref, &root, &principal); err != nil {
			return "", err
		}
		if kind != nil {
			typ += " " + *kind
		}
		if principal != nil {
			return fmt.Sprintf("%s %s %s %s", typ, ref, root, *principal), nil
		}
		return fmt.Sprintf("%s %s %s", typ, ref, root), nil
	})
	if err != nil {
		e.t.Fatal(err)
	}
	return out
}

func (e *env) userEventsAfter(after int64) []string {
	e.t.Helper()
	rows, err := e.pool.Query(e.ctx, `SELECT type || ' ' || user_id FROM events WHERE id > $1 AND type LIKE 'user.%' ORDER BY id`, after)
	if err != nil {
		e.t.Fatal(err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		e.t.Fatal(err)
	}
	return out
}

type response struct {
	status int
	body   map[string]any
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

func (e *env) call(method, path, member, body string) response {
	e.t.Helper()
	h := map[string]string{}
	if member != "" {
		h[members.MemberHeader] = member
	}
	return e.send(e.app, method, path, h, body)
}

func (e *env) callInternal(method, path, body string) response {
	e.t.Helper()
	return e.send(e.internal, method, path, nil, body)
}

func (e *env) send(app *fiber.App, method, path string, headers map[string]string, body string) response {
	e.t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rd)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := app.Test(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	out := response{status: resp.StatusCode}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &out.body); err != nil {
			e.t.Fatalf("%s %s: %s: %v", method, path, raw, err)
		}
	}
	return out
}

func (e *env) expect(r response, status int, what string) {
	e.t.Helper()
	if r.status != status {
		e.t.Fatalf("%s: status %d, want %d: %v", what, r.status, status, r.body)
	}
}

func sorted(vals ...string) []string {
	out := append([]string(nil), vals...)
	slices.Sort(out)
	return out
}
