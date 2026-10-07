package sessions

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"

	"github.com/plat5dev/plat5/identity/errors"
	"github.com/plat5dev/plat5/identity/orgs"
)

func TestRestrictedUserKeyMintsScopedSession(t *testing.T) {
	userID := "user1"
	org := &fakeOrg{member: &orgs.Member{
		ID:             "mem1",
		OrganizationID: "org1",
		UserID:         &userID,
		Status:         orgs.StatusActive,
	}}
	store := &fakeSessions{}
	h := &Handler{store: store, orgStore: org, prefix: "plat5-ms-1-"}
	app := fiber.New(fiber.Config{ErrorHandler: errors.FiberErrorHandler})
	h.MountPublic(app.Group("/users"))
	h.MountInternal(app.Group("/internal"))

	code, body := doSession(t, app, http.MethodPost, "/users/user1/organizations/org1/session", map[string]string{
		"X-Plat5-Scopes": "profile:read",
	})
	if code != http.StatusCreated {
		t.Fatalf("create: %d %s", code, body)
	}
	var created CreateResponse
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatal(err)
	}
	if created.MemberID != "mem1" || created.OrganizationID != "org1" || !strings.HasPrefix(created.Token, "plat5-ms-1-") {
		t.Fatalf("create: %+v", created)
	}
	if created.Scopes == nil || len(*created.Scopes) != 1 || (*created.Scopes)[0] != "profile:read" {
		t.Fatalf("response scopes: %+v", created.Scopes)
	}
	if store.created == nil || store.created.Scopes == nil || len(store.created.Scopes) != 1 || store.created.Scopes[0] != "profile:read" {
		t.Fatalf("stored scopes: %+v", store.created)
	}
	if _, ok := mustObject(t, body)["user_id"]; ok {
		t.Fatalf("mint must not return user_id: %s", body)
	}

	store.validated = &Validated{
		Session:        store.created,
		OrganizationID: "org1",
		MemberStatus:   string(orgs.StatusActive),
	}
	code, body = doSessionJSON(t, app, http.MethodPost, "/internal/member-sessions/validate", `{"token":"`+created.Token+`"}`, nil)
	if code != http.StatusOK {
		t.Fatalf("validate: %d %s", code, body)
	}
	payload := mustObject(t, body)
	if _, ok := payload["user_id"]; ok {
		t.Fatalf("validate user_id: %s", body)
	}
	scopes, ok := payload["scopes"].([]any)
	if !ok || len(scopes) != 1 || scopes[0] != "profile:read" {
		t.Fatalf("validate scopes: %s", body)
	}

	code, body = doSession(t, app, http.MethodPost, "/users/user1/organizations/org1/session", nil)
	if code != http.StatusCreated {
		t.Fatalf("unrestricted: %d %s", code, body)
	}
	if scopes, ok := mustObject(t, body)["scopes"]; !ok || scopes != nil {
		t.Fatalf("unrestricted session must stay null: %s", body)
	}
	if store.created.Scopes != nil {
		t.Fatalf("unrestricted stored: %#v", store.created.Scopes)
	}

	code, body = doSession(t, app, http.MethodPost, "/users/user1/organizations/org1/session", map[string]string{
		"X-Plat5-Scopes": "[]",
	})
	if code != http.StatusCreated {
		t.Fatalf("empty caller: %d %s", code, body)
	}
	empty := mustObject(t, body)["scopes"]
	list, ok := empty.([]any)
	if !ok || len(list) != 0 {
		t.Fatalf("empty caller must not mint null scopes: %s", body)
	}
}

func TestSessionMintStill404WhenNotAMember(t *testing.T) {
	h := &Handler{store: &fakeSessions{}, orgStore: &fakeOrg{err: orgs.ErrNotFound}, prefix: "plat5-ms-1-"}
	app := fiber.New(fiber.Config{ErrorHandler: errors.FiberErrorHandler})
	h.MountPublic(app.Group("/users"))
	code, body := doSession(t, app, http.MethodPost, "/users/user1/organizations/org1/session", map[string]string{
		"X-Plat5-Scopes": "profile:read",
	})
	if code != http.StatusNotFound {
		t.Fatalf("not a member: %d %s", code, body)
	}
}

type fakeSessions struct {
	created   *Session
	validated *Validated
}

func (f *fakeSessions) Create(_ context.Context, session *Session) error {
	cp := *session
	f.created = &cp
	return nil
}

func (f *fakeSessions) GetByHash(context.Context, string) (*Validated, error) {
	if f.validated == nil {
		return nil, ErrNotFound
	}
	return f.validated, nil
}

type fakeOrg struct {
	member *orgs.Member
	err    error
}

func (f *fakeOrg) ResolveMember(context.Context, string, string) (*orgs.Member, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.member, nil
}

func doSession(t *testing.T, app *fiber.App, method, path string, headers map[string]string) (int, []byte) {
	t.Helper()
	return doSessionJSON(t, app, method, path, "", headers)
}

func doSessionJSON(t *testing.T, app *fiber.App, method, path, body string, headers map[string]string) (int, []byte) {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rdr)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, b
}

func mustObject(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatal(err)
	}
	return raw
}
