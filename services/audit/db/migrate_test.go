package db_test

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/plat5dev/plat5/audit/db"
)

// A v1 audit schema recorded 001_init and 002_personal_log. v2 is a hard
// cutover: boot refuses it rather than reading v1 events.
func TestMigrateRefusesAV1Schema(t *testing.T) {
	url := os.Getenv("AUDIT_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("AUDIT_TEST_DATABASE_URL is unset")
	}
	ctx := context.Background()
	schema := fmt.Sprintf("audit_test_v1_%d", time.Now().UnixNano())
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
	if _, err := pool.Exec(ctx, `INSERT INTO `+pgx.Identifier{schema}.Sanitize()+`.schema_migrations (version) VALUES ('002_personal_log')`); err != nil {
		t.Fatal(err)
	}
	err = db.MigrateSchema(ctx, pool, schema)
	if err == nil || !strings.Contains(err.Error(), "002_personal_log") {
		t.Fatalf("want refusal naming the v1 migration, got %v", err)
	}
}
