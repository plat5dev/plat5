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

func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	return MigrateSchema(ctx, pool, Schema)
}

func MigrateSchema(ctx context.Context, pool *pgxpool.Pool, schema string) error {
	if !schemaNameRe.MatchString(schema) {
		return fmt.Errorf("invalid schema name %q", schema)
	}
	quoted := pgx.Identifier{schema}.Sanitize()

	if _, err := pool.Exec(ctx, "CREATE SCHEMA IF NOT EXISTS "+quoted); err != nil {
		return fmt.Errorf("create schema %s: %w", schema, err)
	}
	// Qualified: a pool's statements can each run on a different connection.
	migrations := quoted + ".schema_migrations"
	if _, err := pool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS `+migrations+` (
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
	if err := refuseUnknown(ctx, pool, migrations, versions); err != nil {
		return err
	}

	for _, name := range versions {
		version := strings.TrimSuffix(name, ".sql")
		var exists bool
		if err := pool.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM `+migrations+` WHERE version = $1)`, version,
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

		if _, err := tx.Exec(ctx, string(body)); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("apply migration %s: %w", version, err)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO `+migrations+` (version) VALUES ($1)`, version,
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

// refuseUnknown stops boot on a database that recorded migrations this build
// doesn't have: a v1 identity schema, or one from a newer build. v2 is a hard
// cutover, so a v1 database is reset or moved by hand, never migrated
// (docs/v2-cutover.md).
func refuseUnknown(ctx context.Context, pool *pgxpool.Pool, migrations string, files []string) error {
	known := make(map[string]bool, len(files))
	for _, name := range files {
		known[strings.TrimSuffix(name, ".sql")] = true
	}
	rows, err := pool.Query(ctx, `SELECT version FROM `+migrations+` ORDER BY version`)
	if err != nil {
		return fmt.Errorf("read schema_migrations: %w", err)
	}
	applied, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return fmt.Errorf("read schema_migrations: %w", err)
	}
	for _, v := range applied {
		if !known[v] {
			return fmt.Errorf("schema has migration %q that this build doesn't know (a v1 identity schema?); drop the schema or use a fresh database", v)
		}
	}
	return nil
}
