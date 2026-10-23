// Package testdb gives tests a migrated identity schema in TEST_DATABASE_URL.
// Without it, tests that need Postgres are skipped.
package testdb

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/plat5dev/plat5/identity/db"
)

// Pool drops and recreates schema in TEST_DATABASE_URL, migrates it, and
// returns a pool on it. Use one schema per package: packages test in parallel.
func Pool(t *testing.T, schema string) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set TEST_DATABASE_URL to run against Postgres")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, "DROP SCHEMA IF EXISTS "+pgx.Identifier{schema}.Sanitize()+" CASCADE"); err != nil {
		t.Fatal(err)
	}
	if err := db.MigrateSchema(ctx, admin, schema); err != nil {
		t.Fatal(err)
	}
	admin.Close()
	pool, err := db.ConnectSchema(ctx, url, schema)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}
