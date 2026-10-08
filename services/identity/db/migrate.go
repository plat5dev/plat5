package db

import (
	"context"
	"embed"
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// Settings are set transaction-local before each migration, as
// current_setting('plat5.<key>'). They carry boot config a migration needs,
// like the roles file's slugs for a backfill.
type Settings map[string]string

func Migrate(ctx context.Context, pool *pgxpool.Pool, settings Settings) error {
	return MigrateSchema(ctx, pool, Schema, settings)
}

func MigrateSchema(ctx context.Context, pool *pgxpool.Pool, schema string, settings Settings) error {
	if !schemaNameRe.MatchString(schema) {
		return fmt.Errorf("invalid schema name %q", schema)
	}
	quoted := pgx.Identifier{schema}.Sanitize()

	if _, err := pool.Exec(ctx, "CREATE SCHEMA IF NOT EXISTS "+quoted); err != nil {
		return fmt.Errorf("create schema %s: %w", schema, err)
	}
	if _, err := pool.Exec(ctx, "SET search_path TO "+quoted); err != nil {
		return fmt.Errorf("set search_path: %w", err)
	}

	if _, err := pool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version TEXT PRIMARY KEY,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)
	`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	entries, err := migrationFS.ReadDir("migrations")
	if err != nil {
		return fmt.Errorf("read migrations: %w", err)
	}

	var versions []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		versions = append(versions, e.Name())
	}
	sort.Strings(versions)

	for _, name := range versions {
		version := strings.TrimSuffix(name, ".sql")
		var exists bool
		if err := pool.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version = $1)`, version,
		).Scan(&exists); err != nil {
			return fmt.Errorf("check migration %s: %w", version, err)
		}
		if exists {
			continue
		}

		body, err := migrationFS.ReadFile(path.Join("migrations", name))
		if err != nil {
			return fmt.Errorf("read migration %s: %w", name, err)
		}

		tx, err := pool.Begin(ctx)
		if err != nil {
			return fmt.Errorf("begin migration %s: %w", version, err)
		}

		if _, err := tx.Exec(ctx, "SET LOCAL search_path TO "+quoted); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("set migration search_path %s: %w", version, err)
		}

		for key, value := range settings {
			if _, err := tx.Exec(ctx, `SELECT set_config($1, $2, true)`, "plat5."+key, value); err != nil {
				_ = tx.Rollback(ctx)
				return fmt.Errorf("set migration setting %s: %w", key, err)
			}
		}

		if _, err := tx.Exec(ctx, string(body)); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("apply migration %s: %w", version, err)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO schema_migrations (version) VALUES ($1)`, version,
		); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("record migration %s: %w", version, err)
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("commit migration %s: %w", version, err)
		}
	}

	return nil
}
