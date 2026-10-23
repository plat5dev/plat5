package nodes

import (
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gofiber/fiber/v3"

	apierrors "github.com/plat5dev/plat5/identity/errors"
	"github.com/plat5dev/plat5/identity/feed"
	"github.com/plat5dev/plat5/identity/internal/auditx"
	"github.com/plat5dev/plat5/identity/internal/id"
)

func (e *env) app() *fiber.App {
	app := fiber.New(fiber.Config{ErrorHandler: apierrors.FiberErrorHandler})
	NewHandler(e.store, feed.Static(e.snap)).Mount(app)
	return app
}

func call(t *testing.T, app *fiber.App, method, path, body string) (int, map[string]any, string) {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, r)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
	}
	return resp.StatusCode, out, resp.Header.Get(auditx.Header)
}

func errorCode(body map[string]any) string {
	e, _ := body["error"].(map[string]any)
	code, _ := e["code"].(string)
	return code
}

func TestRoutes(t *testing.T) {
	e := setup(t)
	app := e.app()
	dir, err := e.store.EnsureSignIn(e.ctx, e.snap)
	if err != nil {
		t.Fatal(err)
	}
	org := e.create(nil, "organization", "Gruow")
	project := e.create(org, "project", "Web")

	status, body, _ := call(t, app, "GET", "/nodes/"+project.ID, "")
	if status != 200 || body["slug"] != "web" || body["parent_id"] != org.ID || body["root_id"] != org.ID || body["deleted_at"] != nil || body["status"] != "active" {
		t.Fatalf("get: %d %v", status, body)
	}
	if _, ok := body["deletion_id"]; ok {
		t.Fatal("deletion_id is internal")
	}
	for _, missing := range []string{id.New(id.Node), "gruow", id.New(id.Member)} {
		if status, body, _ := call(t, app, "GET", "/nodes/"+missing, ""); status != 404 || errorCode(body) != "NOT_FOUND" {
			t.Fatalf("get %s: %d %v", missing, status, body)
		}
	}

	status, body, details := call(t, app, "PATCH", "/nodes/"+project.ID, `{"label":"Website"}`)
	if status != 200 || body["label"] != "Website" || details != `{"label":{"from":"Web","to":"Website"}}` {
		t.Fatalf("patch: %d %v %s", status, body, details)
	}
	if status, body, _ := call(t, app, "PATCH", "/nodes/"+project.ID, `{"label":""}`); status != 422 || errorCode(body) != "VALIDATION_ERROR" {
		t.Fatalf("patch empty: %d %v", status, body)
	}

	if status, body, _ := call(t, app, "DELETE", "/nodes/"+dir.ID, ""); status != 422 {
		t.Fatalf("delete sign-in: %d %v", status, body)
	}
	status, _, details = call(t, app, "DELETE", "/nodes/"+org.ID, "")
	if status != 204 || details != `{"status":{"from":"active","to":"deleted"}}` {
		t.Fatalf("delete: %d %s", status, details)
	}
	if status, _, details := call(t, app, "DELETE", "/nodes/"+org.ID, ""); status != 204 || details != "" {
		t.Fatalf("delete again: %d %s", status, details)
	}
	if status, body, _ := call(t, app, "POST", "/nodes/"+project.ID+"/restore", ""); status != 409 || errorCode(body) != "CONFLICT" {
		t.Fatalf("restore under deleted parent: %d %v", status, body)
	}
	status, body, _ = call(t, app, "GET", "/nodes/"+org.ID, "")
	if status != 200 || body["status"] != "deleted" || body["deleted_at"] == nil {
		t.Fatalf("get deleted: %d %v", status, body)
	}
	status, body, _ = call(t, app, "POST", "/nodes/"+org.ID+"/restore", "")
	if status != 200 || body["status"] != "active" {
		t.Fatalf("restore: %d %v", status, body)
	}
	if status, _, details := call(t, app, "POST", "/nodes/"+org.ID+"/restore", ""); status != 200 || details != "" {
		t.Fatalf("restore live: %d %s", status, details)
	}
	if status, body, _ := call(t, app, "POST", "/nodes/"+org.ID+"/release-slug", ""); status != 409 {
		t.Fatalf("release a live slug: %d %v", status, body)
	}

	status, body, _ = call(t, app, "GET", "/nodes?kind=project&limit=10", "")
	if status != 200 || body["has_more"] != false || len(body["nodes"].([]any)) != 1 {
		t.Fatalf("list: %d %v", status, body)
	}
	for _, bad := range []string{"?status=gone", "?starting_after=" + id.New(id.Member), "?limit=0"} {
		if status, _, _ := call(t, app, "GET", "/nodes"+bad, ""); status != 422 {
			t.Fatalf("list %s: %d", bad, status)
		}
	}
}

// The tree lock: a create racing a delete either lands before it and is
// deleted with it, or after it and is refused. Never live under a deleted node.
func TestCreatesRacingADelete(t *testing.T) {
	e := setup(t)
	org := e.create(nil, "organization", "Gruow")
	folder := e.create(org, "folder", "Apps")

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = e.store.Create(e.ctx, e.snap, CreateInput{ParentID: folder.ID, Kind: "project", Label: "P"})
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		e.delete(org)
	}()
	wg.Wait()

	if n := e.count(`
		SELECT count(*) FROM nodes c JOIN nodes p ON p.id = c.parent_id
		WHERE c.status = 'active' AND p.status <> 'active'`); n != 0 {
		t.Fatalf("%d live nodes under deleted parents", n)
	}
	if n := e.count(`SELECT count(DISTINCT slug) FROM nodes WHERE parent_id = $1`, folder.ID); n != e.count(`SELECT count(*) FROM nodes WHERE parent_id = $1`, folder.ID) {
		t.Fatal("generated slugs collided")
	}
}
