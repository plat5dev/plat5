package members

import (
	"encoding/json"
	"fmt"
	"slices"
	"testing"

	"github.com/plat5dev/plat5/identity/errors"
	"github.com/plat5dev/plat5/identity/internal/auditx"
)

func TestNormalizeAllowedCIDRs(t *testing.T) {
	got, err := normalizeAllowedCIDRs(nil)
	if err != nil || len(got) != 0 {
		t.Fatalf("nil: got %v, err %v", got, err)
	}
	got, err = normalizeAllowedCIDRs([]string{})
	if err != nil || len(got) != 0 {
		t.Fatalf("empty: got %v, err %v", got, err)
	}

	got, err = normalizeAllowedCIDRs([]string{
		" 203.0.113.7 ",
		"203.0.113.7/32",
		"198.51.100.42/24",
		"2001:db8::1",
		"2001:db8::abcd/64",
		"::ffff:192.0.2.5",
		"::ffff:192.0.2.5/120",
	})
	if err != nil {
		t.Fatalf("valid: %v", err)
	}
	want := []string{
		"203.0.113.7/32",
		"198.51.100.0/24",
		"2001:db8::1/128",
		"2001:db8::/64",
		"192.0.2.5/32",
		"192.0.2.0/24",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}

	bad := []struct {
		name  string
		input []string
	}{
		{"empty entry", []string{""}},
		{"garbage", []string{"not-an-ip"}},
		{"bad prefix", []string{"203.0.113.0/33"}},
		{"v6 bad prefix", []string{"2001:db8::/129"}},
		{"zone id", []string{"fe80::1%eth0"}},
		{"cidr with zone", []string{"fe80::1%eth0/64"}},
	}
	for _, tc := range bad {
		if _, err := normalizeAllowedCIDRs(tc.input); err == nil {
			t.Errorf("%s: expected error", tc.name)
		} else if apiErr, ok := err.(*errors.ApiError); !ok || apiErr.Status != 422 {
			t.Errorf("%s: expected 422 ApiError, got %v", tc.name, err)
		}
	}

	tooMany := make([]string, MaxAllowedCIDRs+1)
	for i := range tooMany {
		tooMany[i] = fmt.Sprintf("10.0.%d.%d", i/256, i%256)
	}
	if _, err := normalizeAllowedCIDRs(tooMany); err == nil {
		t.Fatal("expected error over MaxAllowedCIDRs")
	}
}

func TestKeyIPAllowlist(t *testing.T) {
	e := setup(t)
	alice := e.person("alice")
	org, _ := e.org(alice, "Acme")
	saID, _ := e.serviceAccount(org, "developer")

	for _, tc := range []struct {
		name     string
		basePath string
	}{
		{"personal", "/users/" + alice + "/keys"},
		{"service_account", "/nodes/" + org.ID + "/service-accounts/" + saID + "/keys"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Create with IP allowlist; bare IP and unmasked CIDR canonicalize and dedupe.
			r := e.call("POST", tc.basePath, "", `{"name":"ci","allowed_cidrs":["203.0.113.7","203.0.113.7/32","198.51.100.9/24"]}`)
			if r.status != 201 {
				t.Fatalf("create: %d %v", r.status, r.body)
			}
			keyID := r.str("id")
			secret := r.str("key")
			assertCIDRs(t, r.body["allowed_cidrs"], []string{"203.0.113.7/32", "198.51.100.0/24"})

			// Admit returns the key's allowed_cidrs.
			adm := e.callInternal("POST", "/internal/admit", fmt.Sprintf(`{"credential":%q,"chain":[{"kind":"organization","ref":%q}]}`, secret, *org.Slug))
			if adm.status != 200 || adm.body["valid"] != true {
				t.Fatalf("admit: %d %v", adm.status, adm.body)
			}
			assertCIDRs(t, adm.body["allowed_cidrs"], []string{"203.0.113.7/32", "198.51.100.0/24"})

			// PATCH name and allowed_cidrs.
			r = e.call("PATCH", tc.basePath+"/"+keyID, "", `{"name":"ci-prod","allowed_cidrs":["2001:db8::1"]}`)
			if r.status != 200 || r.str("name") != "ci-prod" {
				t.Fatalf("patch: %d %v", r.status, r.body)
			}
			assertCIDRs(t, r.body["allowed_cidrs"], []string{"2001:db8::1/128"})
			var details map[string]any
			if err := json.Unmarshal([]byte(r.headers[auditx.Header]), &details); err != nil || details["name"] == nil || details["allowed_cidrs"] == nil {
				t.Fatalf("expected name and allowed_cidrs audit details, got %q", r.headers[auditx.Header])
			}

			// PATCH clear allowlist with null.
			r = e.call("PATCH", tc.basePath+"/"+keyID, "", `{"allowed_cidrs":null}`)
			if r.status != 200 {
				t.Fatalf("patch clear null: %d %v", r.status, r.body)
			}
			assertCIDRs(t, r.body["allowed_cidrs"], []string{})

			// PATCH set then clear with [].
			r = e.call("PATCH", tc.basePath+"/"+keyID, "", `{"allowed_cidrs":["10.0.0.0/8"]}`)
			if r.status != 200 {
				t.Fatalf("patch set: %d %v", r.status, r.body)
			}
			r = e.call("PATCH", tc.basePath+"/"+keyID, "", `{"allowed_cidrs":[]}`)
			if r.status != 200 {
				t.Fatalf("patch clear empty: %d %v", r.status, r.body)
			}
			assertCIDRs(t, r.body["allowed_cidrs"], []string{})

			// PATCH validation errors.
			for _, badBody := range []string{`{}`, `{"name":null}`, `{"name":""}`, `{"allowed_cidrs":["bad"]}`, `{"allowed_cidrs":"10.0.0.1"}`} {
				r = e.call("PATCH", tc.basePath+"/"+keyID, "", badBody)
				if r.status != 422 {
					t.Fatalf("patch %s: expected 422, got %d %v", badBody, r.status, r.body)
				}
			}

			// Revoke then PATCH -> 409 KEY_REVOKED.
			r = e.call("DELETE", tc.basePath+"/"+keyID, "", "")
			if r.status != 204 {
				t.Fatalf("revoke: %d %v", r.status, r.body)
			}
			r = e.call("PATCH", tc.basePath+"/"+keyID, "", `{"name":"after-revoke"}`)
			if r.status != 409 || r.code() != errors.CodeKeyRevoked {
				t.Fatalf("patch revoked: %d %v", r.status, r.body)
			}
		})
	}
}

func assertCIDRs(t *testing.T, raw any, want []string) {
	t.Helper()
	arr, ok := raw.([]any)
	if !ok {
		t.Fatalf("allowed_cidrs is %T (%v), want array", raw, raw)
	}
	got := make([]string, len(arr))
	for i, v := range arr {
		got[i], _ = v.(string)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("allowed_cidrs = %v, want %v", got, want)
	}
}
