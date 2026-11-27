package db

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// migrateLockSpace keys the per-schema session advisory lock that serializes
// concurrent migrations across replicas.
const migrateLockSpace int32 = 0x706c6d67 // "plmg"

//go:embed migrations/*.sql
var migrationFS embed.FS

var roleRe = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)

// ErrNotWriter means serve's role could change or delete history, or cannot write.
var ErrNotWriter = errors.New("not the writer role")

// LatestVersion is the newest migration this binary carries.
func LatestVersion() string {
	versions, err := migrationFiles()
	if err != nil || len(versions) == 0 {
		return ""
	}
	return strings.TrimSuffix(versions[len(versions)-1], ".sql")
}

func migrationFiles() ([]string, error) {
	entries, err := migrationFS.ReadDir("migrations")
	if err != nil {
		return nil, fmt.Errorf("read migrations: %w", err)
	}
	var versions []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		versions = append(versions, e.Name())
	}
	sort.Strings(versions)
	return versions, nil
}

// Migrate applies migrations as the schema owner, then grants writerRole
// exactly the writer's rights (docs/internals/audit-storage.md#roles).
func Migrate(ctx context.Context, pool *pgxpool.Pool, writerRole string) error {
	if !roleRe.MatchString(writerRole) {
		return fmt.Errorf("WRITER_ROLE %q is not a plain lowercase role name", writerRole)
	}
	var me string
	var writerExists bool
	if err := pool.QueryRow(ctx,
		`SELECT current_user, EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1)`,
		writerRole,
	).Scan(&me, &writerExists); err != nil {
		return err
	}
	if me == writerRole {
		return errors.New("WRITER_ROLE must not be the role migrate runs as")
	}
	if !writerExists {
		return fmt.Errorf("role %q does not exist; the deployment creates the writer role", writerRole)
	}
	return migrateSchema(ctx, pool, Schema, writerRole)
}

// MigrateSchema applies migrations to schema without configuring writer role
// grants; used by schema-isolated tests.
func MigrateSchema(ctx context.Context, pool *pgxpool.Pool, schema string) error {
	return migrateSchema(ctx, pool, schema, "")
}

func migrateSchema(ctx context.Context, pool *pgxpool.Pool, schema, writerRole string) error {
	if !schemaNameRe.MatchString(schema) {
		return fmt.Errorf("invalid schema name %q", schema)
	}
	quoted := pgx.Identifier{schema}.Sanitize()

	conn, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire migration connection: %w", err)
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1, hashtext($2))`, migrateLockSpace, schema); err != nil {
		return fmt.Errorf("lock migrations for schema %s: %w", schema, err)
	}
	defer func() {
		unlockCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := conn.Exec(unlockCtx, `SELECT pg_advisory_unlock($1, hashtext($2))`, migrateLockSpace, schema); err != nil {
			_ = conn.Conn().Close(unlockCtx)
		}
	}()

	// A deployment may pre-create the schema for the owner, who then needs no
	// CREATE on the database. IF NOT EXISTS would still check CREATE, so look first.
	var schemaExists bool
	if err := conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_namespace WHERE nspname = $1)`, schema).Scan(&schemaExists); err != nil {
		return fmt.Errorf("check schema %s: %w", schema, err)
	}
	if !schemaExists {
		if _, err := conn.Exec(ctx, "CREATE SCHEMA "+quoted); err != nil {
			return fmt.Errorf("create schema %s: %w", schema, err)
		}
	}

	migrations := quoted + ".schema_migrations"
	if _, err := conn.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS `+migrations+` (
			version TEXT PRIMARY KEY,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)
	`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	versions, err := migrationFiles()
	if err != nil {
		return err
	}
	if err := refuseUnknown(ctx, conn, migrations, versions); err != nil {
		return err
	}

	for _, name := range versions {
		version := strings.TrimSuffix(name, ".sql")
		var exists bool
		if err := conn.QueryRow(ctx,
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

		tx, err := conn.Begin(ctx)
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

	if writerRole != "" {
		if err := grantWriter(ctx, conn, quoted, writerRole); err != nil {
			return err
		}
	}
	return nil
}

func grantWriter(ctx context.Context, conn *pgxpool.Conn, quotedSchema, writerRole string) error {
	writer := pgx.Identifier{writerRole}.Sanitize()
	grants := []string{
		"REVOKE ALL ON SCHEMA " + quotedSchema + " FROM " + writer,
		"REVOKE ALL ON ALL TABLES IN SCHEMA " + quotedSchema + " FROM " + writer,
		"REVOKE ALL ON ALL SEQUENCES IN SCHEMA " + quotedSchema + " FROM " + writer,
		"REVOKE ALL ON ALL FUNCTIONS IN SCHEMA " + quotedSchema + " FROM " + writer,
		"GRANT USAGE ON SCHEMA " + quotedSchema + " TO " + writer,
		"GRANT SELECT ON " + quotedSchema + ".schema_migrations TO " + writer,
		"GRANT SELECT, INSERT ON " + quotedSchema + ".audit_events TO " + writer,
		"GRANT UPDATE (outcome, status, details, created_id, named_node_ids, named_member_id, finalized_seq) ON " + quotedSchema + ".audit_events TO " + writer,
		"GRANT SELECT, INSERT ON " + quotedSchema + ".audit_log_keys TO " + writer,
		"GRANT USAGE, SELECT ON SEQUENCE " + quotedSchema + ".export_seq TO " + writer,
		"GRANT EXECUTE ON FUNCTION " + quotedSchema + ".ensure_partitions(timestamptz) TO " + writer,
	}
	return pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
		for _, g := range grants {
			if _, err := tx.Exec(ctx, g); err != nil {
				return fmt.Errorf("%s: %w", g, err)
			}
		}
		return nil
	})
}

// CheckWriter refuses a role that owns schema audit or can delete or truncate
// events or log keys, and a schema this binary's migrations have not reached.
func CheckWriter(ctx context.Context, pool *pgxpool.Pool) error {
	var table *string
	if err := pool.QueryRow(ctx, `SELECT to_regclass('audit.audit_events')::text`).Scan(&table); err != nil {
		return err
	}
	if table == nil {
		return errors.New("audit.audit_events does not exist; run audit migrate first")
	}
	var applied bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM audit.schema_migrations WHERE version = $1)`,
		LatestVersion(),
	).Scan(&applied); err != nil {
		return fmt.Errorf("%w: cannot read audit.schema_migrations; run audit migrate with WRITER_ROLE set to this role: %v", ErrNotWriter, err)
	}
	if !applied {
		return fmt.Errorf("migration %s is not applied; run audit migrate first", LatestVersion())
	}
	var me string
	var owns, canDelEvents, canTruncEvents, canDelKeys, canTruncKeys bool
	var canInsEvents, canInsKeys, canSeq, canPartition bool
	if err := pool.QueryRow(ctx, `
		SELECT current_user,
			pg_has_role(current_user, n.nspowner, 'MEMBER'),
			has_table_privilege('audit.audit_events', 'DELETE'),
			has_table_privilege('audit.audit_events', 'TRUNCATE'),
			has_table_privilege('audit.audit_log_keys', 'DELETE'),
			has_table_privilege('audit.audit_log_keys', 'TRUNCATE'),
			has_table_privilege('audit.audit_events', 'INSERT'),
			has_table_privilege('audit.audit_log_keys', 'INSERT'),
			has_sequence_privilege('audit.export_seq', 'USAGE'),
			has_function_privilege('audit.ensure_partitions(timestamptz)', 'EXECUTE')
		FROM pg_namespace n WHERE n.nspname = 'audit'
	`).Scan(&me, &owns, &canDelEvents, &canTruncEvents, &canDelKeys, &canTruncKeys,
		&canInsEvents, &canInsKeys, &canSeq, &canPartition); err != nil {
		return err
	}
	if owns || canDelEvents || canTruncEvents || canDelKeys || canTruncKeys {
		return fmt.Errorf("%w: role %q can delete audit events; run serve as the writer role (docs/internals/audit-storage.md#roles)", ErrNotWriter, me)
	}
	if !canInsEvents || !canInsKeys || !canSeq || !canPartition {
		return fmt.Errorf("%w: role %q cannot write audit events; run audit migrate with WRITER_ROLE=%s", ErrNotWriter, me, me)
	}
	return nil
}

// refuseUnknown stops migrate on a database that recorded migrations this build
// doesn't have: one from another build. Such a database is reset or moved by
// hand, never migrated.
func refuseUnknown(ctx context.Context, conn *pgxpool.Conn, migrations string, files []string) error {
	known := make(map[string]bool, len(files))
	for _, name := range files {
		known[strings.TrimSuffix(name, ".sql")] = true
	}
	rows, err := conn.Query(ctx, `SELECT version FROM `+migrations+` ORDER BY version`)
	if err != nil {
		return fmt.Errorf("read schema_migrations: %w", err)
	}
	applied, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return fmt.Errorf("read schema_migrations: %w", err)
	}
	for _, v := range applied {
		if !known[v] {
			return fmt.Errorf("schema has migration %q that this build doesn't know; drop the schema or use a fresh database", v)
		}
	}
	return nil
}
