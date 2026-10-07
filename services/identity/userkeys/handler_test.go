package userkeys

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
)

func TestRestrictedCallerCannotMintWiderUserKey(t *testing.T) {
	keys := &fakeKeys{}
	h := &Handler{store: keys, prefix: "plat5-sk-1-"}
	app := fiber.New(fiber.Config{ErrorHandler: errors.FiberErrorHandler})
	h.MountPublic(app.Group("/users"))
	caller := map[string]string{"X-Plat5-Scopes": "profile:read"}

	code, body := doJSON(t, app, http.MethodPost, "/users/user1/api-keys", `{}`, caller)
	if code != http.StatusCreated {
		t.Fatalf("inherit: %d %s", code, body)
	}
	if keys.keys[0].UserID != "user1" || keys.keys[0].Scopes == nil || len(keys.keys[0].Scopes) != 1 || keys.keys[0].Scopes[0] != "profile:read" {
		t.Fatalf("stored: %+v", keys.keys[0])
	}
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatal(err)
	}
	scopes, ok := raw["scopes"].([]any)
	if !ok || len(scopes) != 1 || scopes[0] != "profile:read" {
		t.Fatalf("body scopes: %s", body)
	}

	code, body = doJSON(t, app, http.MethodPost, "/users/user1/api-keys", `{"scopes":["admin"]}`, caller)
	if code != http.StatusForbidden || !strings.Contains(string(body), "INSUFFICIENT_SCOPE") || !strings.Contains(string(body), "admin") {
		t.Fatalf("escalate: %d %s", code, body)
	}
	if len(keys.keys) != 1 {
		t.Fatalf("rejected key stored")
	}

	code, body = doJSON(t, app, http.MethodPost, "/users/user1/api-keys", `{"scopes":["admin"]}`, nil)
	if code != http.StatusCreated {
		t.Fatalf("unrestricted: %d %s", code, body)
	}
}

type fakeKeys struct {
	keys []*APIKey
}

func (f *fakeKeys) Create(_ context.Context, key *APIKey) error {
	f.keys = append(f.keys, key)
	return nil
}

func (f *fakeKeys) GetByHash(context.Context, string) (*APIKey, error) {
	return nil, ErrNotFound
}

func (f *fakeKeys) List(context.Context, string, int, string) ([]*APIKey, bool, error) {
	return nil, false, nil
}

func (f *fakeKeys) Get(context.Context, string, string) (*APIKey, error) {
	return nil, ErrNotFound
}

func (f *fakeKeys) Revoke(context.Context, string, string) (*APIKey, error) {
	return nil, ErrNotFound
}

func doJSON(t *testing.T, app *fiber.App, method, path, body string, headers map[string]string) (int, []byte) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
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
