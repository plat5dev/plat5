package db

import (
	"context"
	"fmt"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const Schema = "identity"

var schemaNameRe = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

func Connect(ctx context.Context, databaseURL string) (*pgxpool.Pool, error) {
	return ConnectSchema(ctx, databaseURL, Schema)
}

func ConnectSchema(ctx context.Context, databaseURL, schema string) (*pgxpool.Pool, error) {
	if !schemaNameRe.MatchString(schema) {
		return nil, fmt.Errorf("invalid schema name %q", schema)
	}
	if databaseURL == "" {
		return nil, fmt.Errorf("database URL is required")
	}

	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse DATABASE_URL: %w", err)
	}

	cfg.MaxConns = 10
	cfg.MinConns = 1
	cfg.MaxConnLifetime = time.Hour

	quoted := pgx.Identifier{schema}.Sanitize()
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
