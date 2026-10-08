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

func TestUserKeyHasNoScopes(t *testing.T) {
	keys := &fakeKeys{}
	h := &Handler{store: keys, prefix: "plat5-sk-1-"}
	app := fiber.New(fiber.Config{ErrorHandler: errors.FiberErrorHandler})
	h.MountPublic(app.Group("/users"))
	h.MountInternal(app.Group("/internal"))

	code, body := doJSON(t, app, http.MethodPost, "/users/user1/api-keys", `{"name":"ci"}`)
	if code != http.StatusCreated {
		t.Fatalf("create: %d %s", code, body)
	}
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatal(err)
	}
	if _, ok := raw["labels"]; ok {
		t.Fatalf("create must not echo labels: %s", body)
	}
	if len(keys.keys) != 1 || keys.keys[0].UserID != "user1" {
		t.Fatalf("stored: %+v", keys.keys)
	}

	code, body = doJSON(t, app, http.MethodPost, "/users/user1/api-keys", `{"scopes":["profile:read"]}`)
	if code != http.StatusUnprocessableEntity || !strings.Contains(string(body), "can't be narrowed") {
		t.Fatalf("scopes must be refused: %d %s", code, body)
	}
	if len(keys.keys) != 1 {
		t.Fatalf("refused key stored")
	}

	code, body = doJSON(t, app, http.MethodPost, "/users/user1/api-keys", `{"scopes":null}`)
	if code != http.StatusCreated {
		t.Fatalf("null scopes is omitted: %d %s", code, body)
	}

	keys.validated = keys.keys[0]
	code, body = doJSON(t, app, http.MethodPost, "/internal/user-keys/validate", `{"key":"plat5-sk-1-abcdefabcdefabcdefabcdefabcdefab"}`)
	if code != http.StatusOK {
		t.Fatalf("validate: %d %s", code, body)
	}
	raw = map[string]any{}
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatal(err)
	}
	if raw["valid"] != true || raw["user_id"] != "user1" {
		t.Fatalf("validate: %s", body)
	}
	if _, ok := raw["labels"]; ok {
		t.Fatalf("a user key carries no labels: %s", body)
	}
}

type fakeKeys struct {
	keys      []*APIKey
	validated *APIKey
}

func (f *fakeKeys) Create(_ context.Context, key *APIKey) error {
	f.keys = append(f.keys, key)
	return nil
}

func (f *fakeKeys) GetByHash(context.Context, string) (*APIKey, error) {
	if f.validated == nil {
		return nil, ErrNotFound
	}
	return f.validated, nil
}

func (f *fakeKeys) List(context.Context, string, int, string) ([]*APIKey, bool, error) {
	return nil, false, nil
}

func (f *fakeKeys) Revoke(context.Context, string, string) (*APIKey, error) {
	return nil, ErrNotFound
}

func doJSON(t *testing.T, app *fiber.App, method, path, body string) (int, []byte) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
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
