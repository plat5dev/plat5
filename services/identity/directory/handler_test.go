package directory

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"

	apierrors "github.com/plat5dev/plat5/identity/errors"
)

type fakeStore struct {
	user User
}

func (f *fakeStore) List(context.Context, int, string) ([]*User, bool, error) {
	return []*User{&f.user}, false, nil
}

func (f *fakeStore) Get(_ context.Context, userID string) (*User, error) {
	if userID != f.user.ID {
		return nil, ErrNotFound
	}
	u := f.user
	return &u, nil
}

func (f *fakeStore) Update(_ context.Context, userID string, u Update) (*User, User, error) {
	if userID != f.user.ID {
		return nil, User{}, ErrNotFound
	}
	prior := f.user
	if u.ClearName {
		f.user.DisplayName = nil
	} else if u.DisplayName != nil {
		f.user.DisplayName = u.DisplayName
	}
	if u.Status != nil {
		f.user.Status = *u.Status
	}
	out := f.user
	return &out, prior, nil
}

func newApp(store *fakeStore) *fiber.App {
	app := fiber.New(fiber.Config{ErrorHandler: apierrors.FiberErrorHandler})
	(&Handler{store: store}).Mount(app.Group("/users"))
	return app
}

func do(t *testing.T, app *fiber.App, method, path, body string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	res, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(b)
}

// The person's own PATCH is the customer route: it must not change status, or
// a person could suspend themselves with nobody but staff to undo it.
func TestPatchUserRefusesStatus(t *testing.T) {
	store := &fakeStore{user: User{ID: "u1", Status: StatusActive, CreatedAt: time.Now(), UpdatedAt: time.Now()}}
	app := newApp(store)

	code, body := do(t, app, http.MethodPatch, "/users/u1", `{"status":"suspended"}`)
	if code != http.StatusUnprocessableEntity || !strings.Contains(body, `"status"`) {
		t.Fatalf("status on PATCH /users/u1: %d %s", code, body)
	}
	code, _ = do(t, app, http.MethodPatch, "/users/u1", `{"display_name":"Al","status":"suspended"}`)
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("status with a name: %d", code)
	}
	if store.user.Status != StatusActive || store.user.DisplayName != nil {
		t.Fatalf("a refused write changed the person: %+v", store.user)
	}

	code, body = do(t, app, http.MethodPatch, "/users/u1", `{"display_name":"Al"}`)
	if code != http.StatusOK || !strings.Contains(body, `"display_name":"Al"`) {
		t.Fatalf("rename: %d %s", code, body)
	}
}

func TestSetStatus(t *testing.T) {
	store := &fakeStore{user: User{ID: "u1", Status: StatusActive, CreatedAt: time.Now(), UpdatedAt: time.Now()}}
	app := newApp(store)

	for _, body := range []string{`{}`, `{"status":"removed"}`, `{"status":""}`} {
		if code, _ := do(t, app, http.MethodPatch, "/users/u1/status", body); code != http.StatusUnprocessableEntity {
			t.Fatalf("%s: %d", body, code)
		}
	}
	code, body := do(t, app, http.MethodPatch, "/users/u1/status", `{"status":"suspended"}`)
	if code != http.StatusOK || !strings.Contains(body, `"status":"suspended"`) {
		t.Fatalf("suspend: %d %s", code, body)
	}
	code, _ = do(t, app, http.MethodPatch, "/users/u1/status", `{"status":"active"}`)
	if code != http.StatusOK || store.user.Status != StatusActive {
		t.Fatalf("re-enable: %d %+v", code, store.user)
	}
	if code, _ := do(t, app, http.MethodPatch, "/users/nope/status", `{"status":"active"}`); code != http.StatusNotFound {
		t.Fatalf("unknown person: %d", code)
	}
}
