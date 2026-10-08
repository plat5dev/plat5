package orgs

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/plat5dev/plat5/identity/internal/auditx"
)

func TestInviteCreateReportsIDAndRoleNeverToken(t *testing.T) {
	f := newFakeInvites()
	seedOwner(f, "org1", "owner1")
	app := testInviteApp(&Handler{invites: f, roles: starterSet(t)})

	req := httptest.NewRequest(http.MethodPost, "/organizations/org1/invites", strings.NewReader(`{"role":"admin"}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var created InviteResponse
	if err := json.Unmarshal(body, &created); err != nil || resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: %d %s", resp.StatusCode, body)
	}

	raw := resp.Header.Get(auditx.Header)
	var details map[string]any
	if err := json.Unmarshal([]byte(raw), &details); err != nil {
		t.Fatalf("details header %q: %v", raw, err)
	}
	if details["invite_id"] != created.ID || details["role"] != "admin" {
		t.Fatalf("details: %s", raw)
	}
	if created.Token == "" || strings.Contains(raw, created.Token) {
		t.Fatalf("the invite token must never be in details: %s", raw)
	}
}
