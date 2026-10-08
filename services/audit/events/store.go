package events

import (
	"context"
	"encoding/json"
	stderrors "errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/plat5dev/plat5/audit/internal/dbx"
	"github.com/plat5dev/plat5/audit/internal/id"
	"github.com/plat5dev/plat5/audit/metrics"
)

var ErrNotFound = stderrors.New("not found")

type Store struct {
	pool   *pgxpool.Pool
	tracer trace.Tracer
}

func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{
		pool:   pool,
		tracer: otel.Tracer("audit.store"),
	}
}

const eventColumns = `id, occurred_at, request_id, organization_id, actor_member_id, actor_auth_type,
	actor_key_prefix, service, method, route, params, ip, user_agent, outcome, status, details`

// CreateIntent writes a pending event unless one exists for the request id.
// created is false for a retry. The intent the caller holds is not updated.
func (s *Store) CreateIntent(ctx context.Context, in *Intent) (created bool, err error) {
	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "create_intent", dbx.DefaultTimeout,
		attribute.String("request_id", in.RequestID),
		attribute.String("organization.id", in.OrganizationID),
	)
	defer cancel()
	defer op.End()

	params, err := json.Marshal(in.Params)
	if err != nil {
		return false, op.Fail(err)
	}
	insert := func() (pgconn.CommandTag, error) {
		// request_id alone is the dedupe. The unique index has occurred_at
		// too (the partition key), so check request_id here and let the
		// index catch two identical intents racing.
		return s.pool.Exec(ctx, `
			INSERT INTO audit_events (id, occurred_at, request_id, organization_id, actor_member_id,
				actor_auth_type, actor_key_prefix, service, method, route, params, ip, user_agent)
			SELECT $1::text, $2::timestamptz, $3::text, $4::text, $5::text, $6::text, $7::text,
				$8::text, $9::text, $10::text, $11::jsonb, $12::text, $13::text
			WHERE NOT EXISTS (SELECT 1 FROM audit_events WHERE request_id = $3::text)
			ON CONFLICT DO NOTHING
		`, id.NewAt(in.OccurredAt), in.OccurredAt, in.RequestID, in.OrganizationID, in.Actor.MemberID,
			in.Actor.AuthType, in.Actor.KeyPrefix, in.Service, in.Method, in.Route, string(params), in.IP, in.UserAgent)
	}

	tag, err := insert()
	if isNoPartition(err) {
		// Maintenance runs ahead of the clock; this is a boot or skew race.
		if err := s.EnsurePartitions(ctx, in.OccurredAt, in.OccurredAt); err != nil {
			return false, op.Fail(err)
		}
		tag, err = insert()
	}
	if err != nil {
		return false, op.Fail(err)
	}
	op.OK("")
	return tag.RowsAffected() == 1, nil
}

// ApplyOutcome moves a pending event to its outcome. It returns
// metrics.OutcomeApplied, metrics.OutcomeFinal (left unchanged), or ErrNotFound.
func (s *Store) ApplyOutcome(ctx context.Context, requestID string, out *Outcome) (string, error) {
	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "apply_outcome", dbx.DefaultTimeout,
		attribute.String("request_id", requestID),
		attribute.String("audit.outcome", out.Outcome),
	)
	defer cancel()
	defer op.End()

	var details any
	if out.Details != nil {
		details = string(out.Details)
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE audit_events
		SET outcome = $2, status = $3, details = $4::jsonb
		WHERE request_id = $1 AND outcome = 'pending'
	`, requestID, out.Outcome, out.Status, details)
	if err != nil {
		return "", op.Fail(err)
	}
	if tag.RowsAffected() > 0 {
		op.OK("")
		return metrics.OutcomeApplied, nil
	}

	var exists bool
	if err := s.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM audit_events WHERE request_id = $1)`, requestID,
	).Scan(&exists); err != nil {
		return "", op.Fail(err)
	}
	if !exists {
		return "", op.Expected("not found", ErrNotFound)
	}
	op.OK("already final")
	return metrics.OutcomeFinal, nil
}

// List returns one page of an org's events, newest first.
func (s *Store) List(ctx context.Context, f Filter) ([]*Event, bool, error) {
	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "list_events", dbx.DefaultTimeout,
		attribute.String("organization.id", f.OrganizationID),
	)
	defer cancel()
	defer op.End()

	where := []string{"organization_id = $1"}
	args := []any{f.OrganizationID}
	add := func(cond string, v any) {
		args = append(args, v)
		where = append(where, strings.ReplaceAll(cond, "?", "$"+strconv.Itoa(len(args))))
	}
	if f.StartingAfter != "" {
		add("id < ?", f.StartingAfter)
		// The id's time is occurred_at to the millisecond. Saying so lets
		// Postgres skip newer partitions.
		add("occurred_at < ?", id.Time(f.StartingAfter).Add(time.Millisecond))
	}
	if f.ActorMemberID != "" {
		add("actor_member_id = ?", f.ActorMemberID)
	}
	if f.Service != "" {
		add("service = ?", f.Service)
	}
	if f.Method != "" {
		add("method = ?", f.Method)
	}
	if f.Route != "" {
		add("route = ?", f.Route)
	}
	if f.Outcome != "" {
		add("outcome = ?", f.Outcome)
	}
	if f.RequestID != "" {
		add("request_id = ?", f.RequestID)
	}
	if len(f.Params) > 0 {
		p, err := json.Marshal(f.Params)
		if err != nil {
			return nil, false, op.Fail(err)
		}
		add("params @> ?::jsonb", string(p))
	}
	if f.OccurredAfter != nil {
		add("occurred_at >= ?", *f.OccurredAfter)
	}
	if f.OccurredBefore != nil {
		add("occurred_at < ?", *f.OccurredBefore)
	}
	args = append(args, f.Limit+1)

	rows, err := s.pool.Query(ctx, `SELECT `+eventColumns+` FROM audit_events
		WHERE `+strings.Join(where, " AND ")+`
		ORDER BY id DESC
		LIMIT $`+strconv.Itoa(len(args)), args...)
	if err != nil {
		return nil, false, op.Fail(err)
	}
	defer rows.Close()

	var out []*Event
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			return nil, false, op.Fail(err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, false, op.Fail(err)
	}
	hasMore := len(out) > f.Limit
	if hasMore {
		out = out[:f.Limit]
	}
	op.OK("")
	return out, hasMore, nil
}

func scanEvent(row dbx.Scannable) (*Event, error) {
	var e Event
	var params []byte
	var details []byte
	if err := row.Scan(&e.ID, &e.OccurredAt, &e.RequestID, &e.OrganizationID, &e.Actor.MemberID,
		&e.Actor.AuthType, &e.Actor.KeyPrefix, &e.Service, &e.Method, &e.Route, &params, &e.IP,
		&e.UserAgent, &e.Outcome.Outcome, &e.Status, &details); err != nil {
		return nil, err
	}
	e.OccurredAt = e.OccurredAt.UTC()
	if err := json.Unmarshal(params, &e.Params); err != nil {
		return nil, fmt.Errorf("params: %w", err)
	}
	if details != nil {
		e.Details = json.RawMessage(details)
	}
	return &e, nil
}

// EnsurePartitions creates the monthly partitions covering from..to (inclusive
// months). Replicas serialize on an advisory lock; existing partitions are kept.
func (s *Store) EnsurePartitions(ctx context.Context, from, to time.Time) error {
	ctx, op := dbx.Begin(ctx, s.tracer, "ensure_partitions")
	defer op.End()

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return op.Fail(err)
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('audit.audit_events.partitions'))`); err != nil {
		return op.Fail(err)
	}
	for m := monthStart(from); !m.After(monthStart(to)); m = m.AddDate(0, 1, 0) {
		name := pgx.Identifier{fmt.Sprintf("audit_events_y%04dm%02d", m.Year(), int(m.Month()))}.Sanitize()
		if _, err := tx.Exec(ctx, fmt.Sprintf(
			`CREATE TABLE IF NOT EXISTS %s PARTITION OF audit_events FOR VALUES FROM ('%s') TO ('%s')`,
			name, m.Format(time.RFC3339), m.AddDate(0, 1, 0).Format(time.RFC3339),
		)); err != nil {
			return op.Fail(err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return op.Fail(err)
	}
	op.OK("")
	return nil
}

// MaintainPartitions keeps last month through two months ahead in place, now
// and every interval, until ctx ends. Intents are within a day of now, so this
// always runs ahead of them.
func (s *Store) MaintainPartitions(ctx context.Context, interval time.Duration, onErr func(error)) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := s.EnsureAround(ctx, time.Now()); err != nil {
				onErr(err)
			}
		}
	}
}

// EnsureAround creates partitions from the month before now to two months after.
func (s *Store) EnsureAround(ctx context.Context, now time.Time) error {
	m := monthStart(now)
	return s.EnsurePartitions(ctx, m.AddDate(0, -1, 0), m.AddDate(0, 2, 0))
}

func monthStart(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
}

// isNoPartition is Postgres refusing a row no partition covers.
func isNoPartition(err error) bool {
	var pgErr *pgconn.PgError
	return stderrors.As(err, &pgErr) && pgErr.Code == "23514" && strings.Contains(pgErr.Message, "no partition")
}
