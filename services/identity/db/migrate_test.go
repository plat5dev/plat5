package db_test

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/plat5dev/plat5/identity/db"
	"github.com/plat5dev/plat5/identity/internal/testdb"
)

func TestMigrateIsIdempotent(t *testing.T) {
	pool := testdb.Pool(t, "identity_test_db")
	if err := db.MigrateSchema(context.Background(), pool, "identity_test_db"); err != nil {
		t.Fatal(err)
	}
}

func TestMigrateRefusesAnUnknownMigration(t *testing.T) {
	pool := testdb.Pool(t, "identity_test_db_unknown")
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO schema_migrations (version) VALUES ('005_lifecycle_created')`); err != nil {
		t.Fatal(err)
	}
	admin, err := pgxpool.New(ctx, os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	err = db.MigrateSchema(ctx, admin, "identity_test_db_unknown")
	if err == nil || !strings.Contains(err.Error(), "005_lifecycle_created") {
		t.Fatalf("want refusal naming the unknown migration, got %v", err)
	}
}

func TestMigrateConcurrent(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set TEST_DATABASE_URL to run against Postgres")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	const schema = "identity_test_db_concurrent"
	if _, err := admin.Exec(ctx, "DROP SCHEMA IF EXISTS "+schema+" CASCADE"); err != nil {
		t.Fatal(err)
	}

	const replicas = 4
	errs := make([]error, replicas)
	var wg sync.WaitGroup
	for i := range replicas {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			errs[idx] = db.MigrateSchema(ctx, admin, schema)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("replica %d failed: %v", i, err)
		}
	}
}
