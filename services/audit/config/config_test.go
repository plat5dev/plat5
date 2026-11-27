package config

import (
	"strings"
	"testing"
)

func envMap(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestLoadRequiresDatabaseURL(t *testing.T) {
	for _, cmd := range []string{"serve", "migrate", "prune"} {
		if _, err := LoadFrom(cmd, envMap(map[string]string{"WRITER_ROLE": "audit_writer"})); err == nil || !strings.Contains(err.Error(), "DATABASE_URL") {
			t.Fatalf("%s: want DATABASE_URL error when empty, got %v", cmd, err)
		}
		if _, err := LoadFrom(cmd, envMap(map[string]string{"DATABASE_URL": "   ", "WRITER_ROLE": "audit_writer"})); err == nil || !strings.Contains(err.Error(), "DATABASE_URL") {
			t.Fatalf("%s: want DATABASE_URL error when whitespace, got %v", cmd, err)
		}
	}
}

func TestLoadServeDefaultsAndOverrides(t *testing.T) {
	cfg, err := LoadFrom("serve", envMap(map[string]string{
		"DATABASE_URL": "postgres://audit_writer:pw@localhost:5432/plat5?sslmode=disable",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Port != defaultPort || cfg.InternalPort != defaultInternalPort {
		t.Fatalf("defaults: %+v", cfg)
	}

	cfg, err = LoadFrom("serve", envMap(map[string]string{
		"DATABASE_URL":  "postgres://audit_writer:pw@localhost:5432/plat5?sslmode=disable",
		"PORT":          "4002",
		"INTERNAL_PORT": "4003",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Port != "4002" || cfg.InternalPort != "4003" {
		t.Fatalf("overrides: %+v", cfg)
	}

	if _, err := LoadFrom("serve", envMap(map[string]string{
		"DATABASE_URL":  "postgres://audit_writer:pw@localhost:5432/plat5?sslmode=disable",
		"INTERNAL_PORT": "not-a-port",
	})); err == nil || !strings.Contains(err.Error(), "INTERNAL_PORT") {
		t.Fatalf("bad internal port: %v", err)
	}
}

func TestLoadMigrateAndPruneAndUsage(t *testing.T) {
	if _, err := LoadFrom("migrate", envMap(map[string]string{
		"DATABASE_URL": "postgres://audit_owner:pw@localhost:5432/plat5?sslmode=disable",
	})); err == nil || !strings.Contains(err.Error(), "WRITER_ROLE") {
		t.Fatalf("migrate without WRITER_ROLE: %v", err)
	}
	cfg, err := LoadFrom("migrate", envMap(map[string]string{
		"DATABASE_URL": "postgres://audit_owner:pw@localhost:5432/plat5?sslmode=disable",
		"WRITER_ROLE":  "audit_writer",
	}))
	if err != nil || cfg.WriterRole != "audit_writer" {
		t.Fatalf("migrate: %+v %v", cfg, err)
	}
	if _, err := LoadFrom("prune", envMap(map[string]string{
		"DATABASE_URL": "postgres://audit_owner:pw@localhost:5432/plat5?sslmode=disable",
	})); err != nil {
		t.Fatalf("prune: %v", err)
	}
	for _, bad := range []string{"", "unknown"} {
		if _, err := LoadFrom(bad, envMap(map[string]string{"DATABASE_URL": "postgres://x"})); err == nil || !strings.Contains(err.Error(), "usage:") {
			t.Fatalf("cmd %q: want usage error, got %v", bad, err)
		}
	}
}
