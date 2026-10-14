package config

import (
	"fmt"
	"os"
	"strings"
)

const (
	defaultPort         = "3000"
	defaultInternalPort = "3001"
	defaultDatabaseURL  = "postgres://plat5:plat5@localhost:5432/plat5?sslmode=disable"
	defaultAPIKeyBrand  = "plat5"
	maxAPIKeyBrandLen   = 32
)

// Config is process-level identity service configuration.
// OTEL_* stays in the telemetry package (standard env contract).
// There is no SMTP configuration: identity does not send invite (or any) email.
type Config struct {
	Port              string
	InternalPort      string
	DatabaseURL       string
	InternalAuthToken string
	// APIKeyBrand starts every credential identity mints (docs/identity.md#credential-format).
	APIKeyBrand string
	// RolesEnabled is ROLES_ENABLED: roles on (the default) or off for the
	// whole deployment. It is a boot switch; only the roles themselves change
	// while identity runs.
	RolesEnabled bool
	// ControlPlaneURL is CONTROL_PLANE_URL: the control plane's internal base
	// URL, where the roles document is read. Required while roles are on.
	ControlPlaneURL string
}

func Load() (Config, error) {
	brand, err := apiKeyBrandFromEnv()
	if err != nil {
		return Config{}, err
	}
	rolesOn, controlPlane, err := parseRoles(os.Getenv("ROLES_ENABLED"), os.Getenv("CONTROL_PLANE_URL"))
	if err != nil {
		return Config{}, err
	}
	return Config{
		Port:              envOr("PORT", defaultPort),
		InternalPort:      envOr("INTERNAL_PORT", defaultInternalPort),
		DatabaseURL:       envOr("DATABASE_URL", defaultDatabaseURL),
		InternalAuthToken: strings.TrimSpace(os.Getenv("INTERNAL_AUTH_TOKEN")),
		APIKeyBrand:       brand,
		RolesEnabled:      rolesOn,
		ControlPlaneURL:   controlPlane,
	}, nil
}

// parseRoles: roles are on unless ROLES_ENABLED=false. On, CONTROL_PLANE_URL is
// required, so a forgotten URL fails boot instead of turning roles off.
func parseRoles(enabled, controlPlaneURL string) (bool, string, error) {
	switch strings.TrimSpace(enabled) {
	case "", "true":
	case "false":
		return false, "", nil
	default:
		return false, "", fmt.Errorf("ROLES_ENABLED must be true or false, got %q", enabled)
	}
	u := strings.TrimRight(strings.TrimSpace(controlPlaneURL), "/")
	if u == "" {
		return false, "", fmt.Errorf("CONTROL_PLANE_URL is required while roles are on (set ROLES_ENABLED=false to turn roles off)")
	}
	return true, u, nil
}

func apiKeyBrandFromEnv() (string, error) {
	raw, ok := os.LookupEnv("APIKEY_BRAND")
	if !ok {
		return defaultAPIKeyBrand, nil
	}
	return parseAPIKeyBrand(raw)
}

func parseAPIKeyBrand(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", fmt.Errorf("APIKEY_BRAND is empty")
	}
	if len(s) > maxAPIKeyBrandLen {
		return "", fmt.Errorf("APIKEY_BRAND longer than %d characters", maxAPIKeyBrandLen)
	}
	for i, r := range s {
		letter := r >= 'a' && r <= 'z'
		digit := i > 0 && r >= '0' && r <= '9'
		if !letter && !digit {
			return "", fmt.Errorf("APIKEY_BRAND must be [a-z][a-z0-9]*, max %d", maxAPIKeyBrandLen)
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
