package e2e

import (
	"fmt"
	"slices"
	"testing"
	"time"
)

type apiKey struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	KeyPrefix    string   `json:"key_prefix"`
	AllowedCIDRs []string `json:"allowed_cidrs"`
	Key          string   `json:"key"`
}

// API key IP allowlists (docs/reference/api/identity.md#key):
// a key restricted to another CIDR is refused with 401 on normal routes and
// /access/routes; PATCH updates and clears the allowlist, and refuses a revoked key.
func TestKeyIPAllowlist(t *testing.T) {
	s := newStack(t)
	run := fmt.Sprintf("%d", time.Now().UnixNano())
	alice := s.token("e2e-key-ip-"+run, nil)

	// Ensure person exists.
	s.call(alice, "GET", "/user", nil, 200, nil)

	// Create a personal key restricted to a documentation-only CIDR.
	var k apiKey
	s.call(alice, "POST", "/user/keys", map[string]any{
		"name":          "restricted",
		"allowed_cidrs": []string{"198.51.100.7/24"},
	}, 201, &k)
	if !slices.Equal(k.AllowedCIDRs, []string{"198.51.100.0/24"}) {
		t.Fatalf("create allowed_cidrs = %v, want [198.51.100.0/24]", k.AllowedCIDRs)
	}
	restricted := caller(k.Key)

	// Gateway refuses the key from the test runner's IP with 401.
	s.call(restricted, "GET", "/user", nil, 401, nil)
	s.call(restricted, "POST", "/access/routes", map[string]any{
		"checks": []map[string]string{{"method": "GET", "path": "/user"}},
	}, 401, nil)

	// PATCH to allow all IPs and rename; after admit cache settles, the key works.
	var updated apiKey
	s.call(alice, "PATCH", "/user/keys/"+k.ID, map[string]any{
		"name":          "opened",
		"allowed_cidrs": []string{"0.0.0.0/0", "::/0"},
	}, 200, &updated)
	if updated.Name != "opened" || !slices.Equal(updated.AllowedCIDRs, []string{"0.0.0.0/0", "::/0"}) {
		t.Fatalf("patch: %+v", updated)
	}
	settle()
	s.call(restricted, "GET", "/user", nil, 200, nil)

	// PATCH to clear allowlist with empty array.
	s.call(alice, "PATCH", "/user/keys/"+k.ID, map[string]any{
		"allowed_cidrs": []string{},
	}, 200, &updated)
	if len(updated.AllowedCIDRs) != 0 {
		t.Fatalf("clear allowed_cidrs = %v, want []", updated.AllowedCIDRs)
	}

	// Revoke, then PATCH is refused with 409.
	s.call(alice, "DELETE", "/user/keys/"+k.ID, nil, 204, nil)
	s.call(alice, "PATCH", "/user/keys/"+k.ID, map[string]any{"name": "nope"}, 409, nil)
}
