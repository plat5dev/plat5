package memberkeys

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/plat5dev/plat5/identity/internal/auditx"
	"github.com/plat5dev/plat5/identity/orgs"
)

func TestKeyCreateReportsDisplayPrefixNeverKey(t *testing.T) {
	org := &fakeOrgs{}
	org.add(saFixture("org1", "sa1", "mem-sa", orgs.StatusActive))
	app := testKeyApp(&Handler{store: &fakeKeys{}, orgStore: org, prefix: testPrefix, roles: starterSet(t)})

	req := httptest.NewRequest(http.MethodPost, "/organizations/org1/service-accounts/sa1/api-keys", strings.NewReader(`{"name":"deploy"}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	created := decodeCreate(t, body)

	raw := resp.Header.Get(auditx.Header)
	var details map[string]any
	if err := json.Unmarshal([]byte(raw), &details); err != nil {
		t.Fatalf("details header %q: %v", raw, err)
	}
	if details["key_id"] != created.ID || details["key_prefix"] != created.KeyPrefix || details["name"] != "deploy" {
		t.Fatalf("details: %s", raw)
	}
	if strings.Contains(raw, created.Key) {
		t.Fatalf("the key must never be in details: %s", raw)
	}
}
