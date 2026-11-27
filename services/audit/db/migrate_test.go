package db_test

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/plat5dev/plat5/audit/db"
)

// A schema that records a migration this build doesn't know is refused, never
// read: such a database is reset or moved by hand.
func TestMigrateRefusesAnUnknownMigration(t *testing.T) {
	url := os.Getenv("AUDIT_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("AUDIT_TEST_DATABASE_URL is unset")
	}
	ctx := context.Background()
	schema := fmt.Sprintf("audit_test_unknown_%d", time.Now().UnixNano())
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
	})

	if err := db.MigrateSchema(ctx, pool, schema); err != nil {
		t.Fatal(err)
	}
	if err := db.MigrateSchema(ctx, pool, schema); err != nil {
		t.Fatalf("migrating twice: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO `+pgx.Identifier{schema}.Sanitize()+`.schema_migrations (version) VALUES ('099_unknown')`); err != nil {
		t.Fatal(err)
	}
	err = db.MigrateSchema(ctx, pool, schema)
	if err == nil || !strings.Contains(err.Error(), "099_unknown") {
		t.Fatalf("want refusal naming the unknown migration, got %v", err)
	}
}

func TestPruneSchema(t *testing.T) {
	url := os.Getenv("AUDIT_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("AUDIT_TEST_DATABASE_URL is unset")
	}
	ctx := context.Background()
	schema := fmt.Sprintf("audit_test_prune_%d", time.Now().UnixNano())
	pool, err := db.ConnectSchema(ctx, url, schema)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
	})

	if err := db.MigrateSchema(ctx, pool, schema); err != nil {
		t.Fatal(err)
	}
	for _, m := range []time.Time{
		time.Date(2025, 10, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2025, 11, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2025, 12, 1, 0, 0, 0, 0, time.UTC),
	} {
		if _, err := pool.Exec(ctx, `SELECT ensure_partitions($1)`, m); err != nil {
			t.Fatal(err)
		}
	}

	cutoff := time.Date(2025, 12, 1, 0, 0, 0, 0, time.UTC)
	dry, err := db.PruneSchema(ctx, pool, schema, cutoff, true)
	if err != nil || len(dry) != 4 {
		t.Fatalf("dry run: %v %v", dry, err)
	}
	dropped, err := db.PruneSchema(ctx, pool, schema, cutoff, false)
	if err != nil || len(dropped) != 4 {
		t.Fatalf("prune: %v %v", dropped, err)
	}
	// 2025-12 must remain; 2025-10 and 2025-11 must be gone.
	var existsOld, existsNew bool
	_ = pool.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL, to_regclass($2) IS NOT NULL`,
		schema+".audit_events_y2025m10", schema+".audit_events_y2025m12").Scan(&existsOld, &existsNew)
	if existsOld || !existsNew {
		t.Fatalf("after prune: old=%v new=%v", existsOld, existsNew)
	}
}

func TestMigrateSchemaConcurrent(t *testing.T) {
	url := os.Getenv("AUDIT_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("AUDIT_TEST_DATABASE_URL is unset")
	}
	ctx := context.Background()
	schema := fmt.Sprintf("audit_test_concurrent_%d", time.Now().UnixNano())
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
	})

	const workers = 6
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- db.MigrateSchema(ctx, pool, schema)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent MigrateSchema: %v", err)
		}
	}
}
