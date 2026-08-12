package config

import (
	"os"
	"strings"
)

const (
	defaultPort         = "3000"
	defaultInternalPort = "3001"
	defaultDatabaseURL  = "postgres://plat5:plat5@localhost:5432/plat5?sslmode=disable"
)

// Config is process-level identity service configuration.
// OTEL_* stays in the telemetry package (standard env contract).
type Config struct {
	Port              string
	InternalPort      string
	DatabaseURL       string
	InternalAuthToken string
}

func Load() Config {
	return Config{
		Port:              envOr("PORT", defaultPort),
		InternalPort:      envOr("INTERNAL_PORT", defaultInternalPort),
		DatabaseURL:       envOr("DATABASE_URL", defaultDatabaseURL),
		InternalAuthToken: strings.TrimSpace(os.Getenv("INTERNAL_AUTH_TOKEN")),
	}
}

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}
