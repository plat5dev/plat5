package config

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseAPIKeyBrand(t *testing.T) {
	ok := []string{"plat5", "acme", "a", "a1", "happ", "sk"}
	for _, in := range ok {
		got, err := parseAPIKeyBrand(in)
		if err != nil {
			t.Fatalf("%q: %v", in, err)
		}
		if got != in {
			t.Fatalf("%q: got %q", in, got)
		}
	}
	got, err := parseAPIKeyBrand("  acme  ")
	if err != nil || got != "acme" {
		t.Fatalf("trim: %q %v", got, err)
	}

	bad := []string{"", "   ", "Plat5", "acme-app", "1acme", "-x"}
	for _, in := range bad {
		if _, err := parseAPIKeyBrand(in); err == nil {
			t.Fatalf("%q: expected error", in)
		}
	}

	long := make([]byte, maxAPIKeyBrandLen+1)
	for i := range long {
		long[i] = 'a'
	}
	if _, err := parseAPIKeyBrand(string(long)); err == nil {
		t.Fatal("expected length error")
	}

	max := make([]byte, maxAPIKeyBrandLen)
	for i := range max {
		max[i] = 'a'
	}
	if _, err := parseAPIKeyBrand(string(max)); err != nil {
		t.Fatalf("max length: %v", err)
	}
}

func TestParseControlPlaneURL(t *testing.T) {
	u, err := parseControlPlaneURL(" http://control-plane:5003/ ")
	if err != nil || u != "http://control-plane:5003" {
		t.Fatalf("%q %v", u, err)
	}
	if _, err := parseControlPlaneURL(" "); err == nil {
		t.Fatal("a missing CONTROL_PLANE_URL must fail boot")
	}
}

func TestLoadIgnoresRolesEnabled(t *testing.T) {
	t.Setenv("CONTROL_PLANE_URL", "http://control-plane:5003")
	t.Setenv("ROLES_ENABLED", "false")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := reflect.TypeOf(cfg).FieldByName("RolesEnabled"); ok {
		t.Fatal("roles are always on in v2")
	}
}

func TestLoadIgnoresSMTP(t *testing.T) {
	t.Setenv("CONTROL_PLANE_URL", "http://control-plane:5003")
	t.Setenv("SMTP_HOST", "mail.example.com")
	t.Setenv("SMTP_USER", "u")
	t.Setenv("SMTP_PASS", "p")
	t.Setenv("SMTP_FROM", "noreply@example.com")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	typ := reflect.TypeOf(cfg)
	for i := 0; i < typ.NumField(); i++ {
		name := typ.Field(i).Name
		if strings.Contains(strings.ToLower(name), "smtp") {
			t.Fatalf("identity config must not have SMTP fields: %s", name)
		}
	}
}
