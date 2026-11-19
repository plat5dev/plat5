package config

import (
	"fmt"
	"os"
	"strings"
)

const (
	defaultPort         = "3002"
	defaultInternalPort = "3003"
)

// Config is process-level audit service configuration.
// OTEL_* stays in the telemetry package (standard env contract).
// There is no retention setting: events are kept (docs/internals/audit-storage.md).
type Config struct {
	Port         string
	InternalPort string
	DatabaseURL  string
}

func Load() (Config, error) {
	databaseURL := strings.TrimSpace(os.Getenv("DATABASE_URL"))
	if databaseURL == "" {
		return Config{}, fmt.Errorf("DATABASE_URL is required")
	}
	return Config{
		Port:         envOr("PORT", defaultPort),
		InternalPort: envOr("INTERNAL_PORT", defaultInternalPort),
		DatabaseURL:  databaseURL,
	}, nil
}

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}
