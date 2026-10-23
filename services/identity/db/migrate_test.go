package db_test

import (
	"context"
	"os"
	"strings"
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
