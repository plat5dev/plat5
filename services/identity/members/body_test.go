package members

import (
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
)

// sendAs sends body with the Content-Type given, none when contentType is "".
func (e *env) sendAs(app *fiber.App, method, path, contentType, body string, headers map[string]string) (int, string) {
	e.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Del("Content-Type")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
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
	return resp.StatusCode, string(raw)
}

// A body not sent as application/json is 400 INVALID_REQUEST on every write
// that reads one, never a 500.
func TestBodyNotJSONIs400(t *testing.T) {
	e := setup(t)
	user := e.person("owner")
	org, member := e.org(user, "Acme")
	folder := e.child(org, "folder", "Eng")
	project := e.child(folder, "project", "Web")

	writes := []struct {
		app            *fiber.App
		method, path   string
		body           string
		headers        map[string]string
		jsonWantStatus int
	}{
		{e.app, "POST", "/nodes/" + folder.ID + "/children/project", `{"display_name": "Api"}`, map[string]string{MemberHeader: member}, 201},
		{e.app, "POST", "/roots/organization", `{"display_name": "Other"}`, map[string]string{UserHeader: user}, 201},
		{e.app, "PATCH", "/nodes/" + project.ID, `{"display_name": "Web 2"}`, nil, 200},
		{e.app, "POST", "/nodes/" + org.ID + "/members", `{"idp": {"issuer": "https://idp.test", "subject": "new"}, "role": "member"}`, nil, 201},
		{e.app, "POST", "/nodes/" + org.ID + "/invites", `{"role": "member"}`, nil, 201},
		{e.internal, "POST", "/internal/plan", `{"kinds": ["project"]}`, nil, 200},
		{e.internal, "POST", "/access/v1/evaluation", `{"subject": {"type": "member", "id": "` + member + `"}, "resource": {"type": "project", "id": "` + project.ID + `"}, "action": {"name": "project:read"}}`, nil, 200},
	}
	for _, w := range writes {
		for _, ct := range []string{"", "text/plain", "application/x-www-form-urlencoded"} {
			status, raw := e.sendAs(w.app, w.method, w.path, ct, w.body, w.headers)
			if status != 400 || !strings.Contains(raw, `"INVALID_REQUEST"`) {
				t.Errorf("%s %s as %q: %d %s, want 400 INVALID_REQUEST", w.method, w.path, ct, status, raw)
			}
		}
	}
	// application/json with parameters is JSON.
	for _, w := range writes {
		status, raw := e.sendAs(w.app, w.method, w.path, "application/json; charset=utf-8", w.body, w.headers)
		if status != w.jsonWantStatus {
			t.Errorf("%s %s as JSON with charset: %d %s, want %d", w.method, w.path, status, raw, w.jsonWantStatus)
		}
	}
}

// A route whose body is optional takes none, whatever the Content-Type; a
// body it does get must be JSON.
func TestOptionalBody(t *testing.T) {
	e := setup(t)
	user := e.person("owner")
	if status, raw := e.sendAs(e.app, "POST", "/users/"+user+"/keys", "", "", nil); status != 201 {
		t.Fatalf("no body: %d %s", status, raw)
	}
	if status, raw := e.sendAs(e.app, "POST", "/users/"+user+"/keys", "text/plain", `{"name": "ci"}`, nil); status != 400 {
		t.Fatalf("text/plain body: %d %s", status, raw)
	}
	// A required body that's empty is 400 too, JSON or not.
	if status, raw := e.sendAs(e.app, "POST", "/users/"+user+"/invites/redeem", "application/json", "", nil); status != 400 {
		t.Fatalf("empty JSON body: %d %s", status, raw)
	}
}
