package db

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const Schema = "api_keys"

var schemaNameRe = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

func Connect(ctx context.Context) (*pgxpool.Pool, error) {
	if !schemaNameRe.MatchString(Schema) {
		return nil, fmt.Errorf("invalid schema name %q", Schema)
	}

	url := os.Getenv("DATABASE_URL")
	if url == "" {
		url = "postgres://plat5:plat5@localhost:5432/plat5?sslmode=disable"
	}

	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("parse DATABASE_URL: %w", err)
	}

	cfg.MaxConns = 10
	cfg.MinConns = 1
	cfg.MaxConnLifetime = time.Hour

	quoted := pgx.Identifier{Schema}.Sanitize()
	cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		_, err := conn.Exec(ctx, "SET search_path TO "+quoted)
		return err
	}

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connect postgres: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}

	return pool, nil
}
