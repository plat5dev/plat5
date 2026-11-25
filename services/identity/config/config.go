package config

import (
	"fmt"
	"os"
	"strings"
)

const (
	defaultPort         = "3000"
	defaultInternalPort = "3001"
	defaultSecretBrand  = "plat5"
	maxSecretBrandLen   = 32
)

// Config is process-level identity service configuration.
// OTEL_* stays in the telemetry package (standard env contract).
// There is no SMTP configuration: identity does not send invite (or any) email.
type Config struct {
	Port         string
	InternalPort string
	// DatabaseURL is DATABASE_URL: Postgres connection string. Required.
	DatabaseURL string
	// SecretBrand starts every secret identity mints, API keys and invite
	// tokens (docs/reference/api/identity.md#secrets).
	SecretBrand string
	// ControlPlaneURL is CONTROL_PLANE_URL: the control plane's internal base
	// URL, where identity reads the config. Required.
	ControlPlaneURL string
	// UpstreamBlockPrivateIPs is UPSTREAM_BLOCK_PRIVATE_IPS: blocks outbound
	// AuthZEN PDP calls from resolving to private, loopback, or link-local IPs.
	UpstreamBlockPrivateIPs bool
}

func Load() (Config, error) {
	brand, err := secretBrandFromEnv()
	if err != nil {
		return Config{}, err
	}
	databaseURL, err := parseDatabaseURL(os.Getenv("DATABASE_URL"))
	if err != nil {
		return Config{}, err
	}
	controlPlane, err := parseControlPlaneURL(os.Getenv("CONTROL_PLANE_URL"))
	if err != nil {
		return Config{}, err
	}
	blockPrivate, err := parseBoolEnv("UPSTREAM_BLOCK_PRIVATE_IPS", false)
	if err != nil {
		return Config{}, err
	}
	return Config{
		Port:                    envOr("PORT", defaultPort),
		InternalPort:            envOr("INTERNAL_PORT", defaultInternalPort),
		DatabaseURL:             databaseURL,
		SecretBrand:             brand,
		ControlPlaneURL:         controlPlane,
		UpstreamBlockPrivateIPs: blockPrivate,
	}, nil
}

func parseBoolEnv(key string, fallback bool) (bool, error) {
	raw, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(raw) == "" {
		return fallback, nil
	}
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "true", "1":
		return true, nil
	case "false", "0":
		return false, nil
	default:
		return false, fmt.Errorf("%s must be true or false, got %q", key, raw)
	}
}

func parseDatabaseURL(raw string) (string, error) {
	u := strings.TrimSpace(raw)
	if u == "" {
		return "", fmt.Errorf("DATABASE_URL is required")
	}
	return u, nil
}

// parseControlPlaneURL: identity doesn't listen until it has the control
// plane's config, so a missing URL fails boot.
func parseControlPlaneURL(raw string) (string, error) {
	u := strings.TrimRight(strings.TrimSpace(raw), "/")
	if u == "" {
		return "", fmt.Errorf("CONTROL_PLANE_URL is required")
	}
	return u, nil
}

func secretBrandFromEnv() (string, error) {
	raw, ok := os.LookupEnv("SECRET_BRAND")
	if !ok {
		return defaultSecretBrand, nil
	}
	return parseSecretBrand(raw)
}

func parseSecretBrand(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", fmt.Errorf("SECRET_BRAND is empty")
	}
	if len(s) > maxSecretBrandLen {
		return "", fmt.Errorf("SECRET_BRAND longer than %d characters", maxSecretBrandLen)
	}
	for i, r := range s {
		letter := r >= 'a' && r <= 'z'
		digit := i > 0 && r >= '0' && r <= '9'
		if !letter && !digit {
			return "", fmt.Errorf("SECRET_BRAND must be [a-z][a-z0-9]*, max %d", maxSecretBrandLen)
		}
	}
	return s, nil
}

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}
