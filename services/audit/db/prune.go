package db

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	beforeRe    = regexp.MustCompile(`^(\d{4})-(0[1-9]|1[0-2])$`)
	olderThanRe = regexp.MustCompile(`^([1-9][0-9]*)m$`)
	partNameRe  = regexp.MustCompile(`^(audit_(?:events|log_keys))_y(\d{4})m(0[1-9]|1[0-2])$`)
)

// ParsePruneCutoff parses --before YYYY-MM or --older-than <N>m into a UTC
// month-start cutoff. Partitions strictly older than cutoff are pruned.
// Refuses cutoffs past the current UTC month so live or future partitions
// cannot be dropped.
func ParsePruneCutoff(before, olderThan string, now time.Time) (time.Time, error) {
	before = strings.TrimSpace(before)
	olderThan = strings.TrimSpace(olderThan)
	if (before == "") == (olderThan == "") {
		return time.Time{}, errors.New("specify exactly one of --before YYYY-MM or --older-than <N>m")
	}
	cur := monthStartUTC(now)
	var cutoff time.Time
	if before != "" {
		m := beforeRe.FindStringSubmatch(before)
		if m == nil {
			return time.Time{}, fmt.Errorf("invalid --before %q (want YYYY-MM)", before)
		}
		year, _ := strconv.Atoi(m[1])
		month, _ := strconv.Atoi(m[2])
		cutoff = time.Date(year, time.Month(month), 1, 0, 0, 0, 0, time.UTC)
	} else {
		m := olderThanRe.FindStringSubmatch(olderThan)
		if m == nil {
			return time.Time{}, fmt.Errorf("invalid --older-than %q (want positive integer months, e.g. 12m)", olderThan)
		}
		months, err := strconv.Atoi(m[1])
		if err != nil || months < 1 || months > 1200 {
			return time.Time{}, fmt.Errorf("invalid --older-than %q (must be between 1m and 1200m)", olderThan)
		}
		cutoff = cur.AddDate(0, -months, 0)
	}
	if cutoff.After(cur) {
		return time.Time{}, fmt.Errorf("cutoff %s is after the current month %s; refusing to prune live or future partitions",
			cutoff.Format("2006-01"), cur.Format("2006-01"))
	}
	return cutoff, nil
}

// Prune drops monthly partitions of audit_log_keys and audit_events strictly
// older than cutoff in schema audit. Must run as the schema owner role.
// When dryRun is true, returns the matching partition names without dropping.
func Prune(ctx context.Context, pool *pgxpool.Pool, cutoff time.Time, dryRun bool) ([]string, error) {
	return PruneSchema(ctx, pool, Schema, cutoff, dryRun)
}

// PruneSchema drops monthly partitions strictly older than cutoff in schema.
func PruneSchema(ctx context.Context, pool *pgxpool.Pool, schema string, cutoff time.Time, dryRun bool) ([]string, error) {
	if !schemaNameRe.MatchString(schema) {
		return nil, fmt.Errorf("invalid schema name %q", schema)
	}
	cutoff = monthStartUTC(cutoff)

	var me string
	var owns bool
	if err := pool.QueryRow(ctx, `
		SELECT current_user, pg_has_role(current_user, n.nspowner, 'MEMBER')
		FROM pg_namespace n WHERE n.nspname = $1
	`, schema).Scan(&me, &owns); err != nil {
		return nil, fmt.Errorf("check schema owner for %s: %w", schema, err)
	}
	if !owns {
		return nil, fmt.Errorf("role %q does not own schema %s; prune must run as the owner role", me, schema)
	}

	conn, err := pool.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock(hashtext('audit.audit_events.partitions'))`); err != nil {
		return nil, fmt.Errorf("lock audit partitions: %w", err)
	}
	defer func() {
		unlockCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := conn.Exec(unlockCtx, `SELECT pg_advisory_unlock(hashtext('audit.audit_events.partitions'))`); err != nil {
			_ = conn.Conn().Close(unlockCtx)
		}
	}()

	rows, err := conn.Query(ctx, `
		SELECT c.relname
		FROM pg_inherits i
		JOIN pg_class p ON p.oid = i.inhparent
		JOIN pg_class c ON c.oid = i.inhrelid
		JOIN pg_namespace n ON n.oid = p.relnamespace
		WHERE n.nspname = $1
		  AND p.relname IN ('audit_events', 'audit_log_keys')
	`, schema)
	if err != nil {
		return nil, fmt.Errorf("list partitions: %w", err)
	}
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("list partitions: %w", err)
	}

	type candidate struct {
		name  string
		table string
		month time.Time
	}
	var targets []candidate
	for _, name := range names {
		m := partNameRe.FindStringSubmatch(name)
		if m == nil {
			continue
		}
		year, _ := strconv.Atoi(m[2])
		month, _ := strconv.Atoi(m[3])
		partMonth := time.Date(year, time.Month(month), 1, 0, 0, 0, 0, time.UTC)
		if partMonth.Before(cutoff) {
			targets = append(targets, candidate{name: name, table: m[1], month: partMonth})
		}
	}
	// Oldest month first; within a month drop audit_log_keys before audit_events.
	sort.Slice(targets, func(i, j int) bool {
		if !targets[i].month.Equal(targets[j].month) {
			return targets[i].month.Before(targets[j].month)
		}
		if targets[i].table != targets[j].table {
			return targets[i].table == "audit_log_keys"
		}
		return targets[i].name < targets[j].name
	})

	quotedSchema := pgx.Identifier{schema}.Sanitize()
	dropped := make([]string, 0, len(targets))
	for _, t := range targets {
		if !dryRun {
			stmt := "DROP TABLE IF EXISTS " + quotedSchema + "." + pgx.Identifier{t.name}.Sanitize()
			if _, err := conn.Exec(ctx, stmt); err != nil {
				return dropped, fmt.Errorf("drop %s.%s: %w", schema, t.name, err)
			}
		}
		dropped = append(dropped, t.name)
	}
	return dropped, nil
}

func monthStartUTC(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
}
