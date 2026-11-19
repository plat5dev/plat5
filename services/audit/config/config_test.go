package config

import (
	"strings"
	"testing"
)

func TestLoadRequiresDatabaseURL(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "DATABASE_URL") {
		t.Fatalf("want DATABASE_URL error when empty, got %v", err)
	}

	t.Setenv("DATABASE_URL", "   ")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "DATABASE_URL") {
		t.Fatalf("want DATABASE_URL error when whitespace, got %v", err)
	}
}

func TestLoadDefaultsAndOverrides(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://plat5:plat5@localhost:5432/plat5?sslmode=disable")
	t.Setenv("PORT", "")
	t.Setenv("INTERNAL_PORT", "")

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Port != defaultPort || cfg.InternalPort != defaultInternalPort {
		t.Fatalf("defaults: %+v", cfg)
	}
	if cfg.DatabaseURL != "postgres://plat5:plat5@localhost:5432/plat5?sslmode=disable" {
		t.Fatalf("database url: %q", cfg.DatabaseURL)
	}

	t.Setenv("PORT", "4002")
	t.Setenv("INTERNAL_PORT", "4003")
	cfg, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Port != "4002" || cfg.InternalPort != "4003" {
		t.Fatalf("overrides: %+v", cfg)
	}
}
