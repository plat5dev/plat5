package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

const (
	defaultPort         = "3002"
	defaultInternalPort = "3003"
)

// Config is process-level audit service configuration.
// OTEL_* stays in the telemetry package (standard env contract).
type Config struct {
	Port         string
	InternalPort string
	DatabaseURL  string
	WriterRole   string
}

// Load reads what cmd needs: serve needs its ports, migrate needs WRITER_ROLE,
// prune needs DATABASE_URL.
func Load(cmd string) (Config, error) {
	return LoadFrom(cmd, os.Getenv)
}

func LoadFrom(cmd string, getenv func(string) string) (Config, error) {
	get := func(key, fallback string) string {
		if v := strings.TrimSpace(getenv(key)); v != "" {
			return v
		}
		return fallback
	}
	cfg := Config{
		Port:         get("PORT", defaultPort),
		InternalPort: get("INTERNAL_PORT", defaultInternalPort),
		DatabaseURL:  get("DATABASE_URL", ""),
		WriterRole:   get("WRITER_ROLE", ""),
	}
	var errs []error
	if cfg.DatabaseURL == "" {
		errs = append(errs, errors.New("DATABASE_URL is required"))
	}
	switch cmd {
	case "serve":
		if p, err := strconv.Atoi(cfg.Port); err != nil || p < 1 || p > 65535 {
			errs = append(errs, fmt.Errorf("PORT must be a port number"))
		}
		if p, err := strconv.Atoi(cfg.InternalPort); err != nil || p < 1 || p > 65535 {
			errs = append(errs, fmt.Errorf("INTERNAL_PORT must be a port number"))
		}
	case "migrate":
		if cfg.WriterRole == "" {
			errs = append(errs, errors.New("WRITER_ROLE is required: migrate grants it the writer's rights"))
		}
	case "prune":
		// DATABASE_URL only; cutoff flags are parsed by the prune command.
	default:
		errs = append(errs, errors.New("usage: audit migrate|serve|prune"))
	}
	return cfg, errors.Join(errs...)
}
