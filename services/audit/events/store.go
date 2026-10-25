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

const eventColumns = `e.id, e.occurred_at, e.request_id, e.root_id, e.nodes, e.actor_member_id, e.actor_user_id,
	e.actor_principal, e.actor_auth_type, e.actor_key_id, e.service, e.method, e.route, e.params, e.ip,
	e.user_agent, e.outcome, e.status, e.details, e.created_id, e.named_nodes, e.named_member_id`

// CreateIntent writes the event unless one exists for the request id: pending,
// or final with a gateway refusal's outcome. It joins the chain's logs and,
// on a user route, the person's. created is false for a retry.
func (s *Store) CreateIntent(ctx context.Context, in *Intent, final *Outcome) (created bool, err error) {
	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "create_intent", dbx.DefaultTimeout,
		attribute.String("request_id", in.RequestID),
	)
	defer cancel()
	defer op.End()

	params, err := json.Marshal(in.Params)
	if err != nil {
		return false, op.Fail(err)
	}
	nodes, err := json.Marshal(in.Nodes)
	if err != nil {
		return false, op.Fail(err)
	}
	outcome, status := OutcomePending, (*int)(nil)
	if final != nil {
		outcome, status = final.Outcome, final.Status
	}
	logs := make([]string, 0, len(in.Nodes)+1)
	for _, n := range in.Nodes {
		logs = append(logs, n.ID)
	}
	if in.Actor.UserID != nil {
		logs = append(logs, UserLog(*in.Actor.UserID))
	}
	eventID := id.NewAt(in.OccurredAt)

	insert := func() (bool, error) {
		tx, err := s.pool.Begin(ctx)
		if err != nil {
			return false, err
		}
		defer tx.Rollback(ctx)
		// request_id alone is the dedupe. The unique index has occurred_at
		// too (the partition key), so check request_id here and let the
		// index catch two identical intents racing.
		tag, err := tx.Exec(ctx, `
			INSERT INTO audit_events (id, occurred_at, request_id, root_id, nodes, actor_member_id, actor_user_id,
				actor_principal, actor_auth_type, actor_key_id, service, method, route, params, ip, user_agent,
				outcome, status)
			SELECT $1::text, $2::timestamptz, $3::text, $4::text, $5::jsonb, $6::text, $7::text, $8::text,
				$9::text, $10::text, $11::text, $12::text, $13::text, $14::jsonb, $15::text, $16::text,
				$17::text, $18::integer
			WHERE NOT EXISTS (SELECT 1 FROM audit_events WHERE request_id = $3::text)
			ON CONFLICT DO NOTHING
		`, eventID, in.OccurredAt, in.RequestID, in.RootID, string(nodes),
			in.Actor.MemberID, in.Actor.UserID, in.Actor.Principal, in.Actor.AuthType, in.Actor.KeyID,
			in.Service, in.Method, in.Route, string(params), in.IP, in.UserAgent, outcome, status)
		if err != nil || tag.RowsAffected() == 0 {
			return false, err
		}
		if err := addLogs(ctx, tx, logs, eventID, in.OccurredAt); err != nil {
			return false, err
		}
		return true, tx.Commit(ctx)
	}

	created, err = insert()
	if isNoPartition(err) {
		// Maintenance runs ahead of the clock; this is a boot or skew race.
		if err := s.EnsurePartitions(ctx, in.OccurredAt, in.OccurredAt); err != nil {
			return false, op.Fail(err)
		}
		created, err = insert()
	}
	if err != nil {
		return false, op.Fail(err)
	}
	op.OK("")
	return created, nil
}

// addLogs puts the event in each log. A log it's already in is kept.
func addLogs(ctx context.Context, tx pgx.Tx, logs []string, eventID string, at time.Time) error {
	if len(logs) == 0 {
		return nil
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO audit_log_keys (log, event_id, occurred_at)
		SELECT DISTINCT unnest($1::text[]), $2::text, $3::timestamptz
		ON CONFLICT DO NOTHING
	`, logs, eventID, at)
	return err
}

// ApplyOutcome moves a pending event to its outcome, and adds it to the logs
// of the nodes the outcome names and the node it created. It returns
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
	named := out.NamedNodes
	if named == nil {
		named = []string{}
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", op.Fail(err)
	}
	defer tx.Rollback(ctx)

	// Only a user route's event names nodes. A tree route's event is in its
	// chain's logs, and named nodes sent for it are ignored.
	var eventID string
	var at time.Time
	var stored []string
	err = tx.QueryRow(ctx, `
		UPDATE audit_events
		SET outcome = $2, status = $3, details = $4::jsonb, created_id = $5,
			named_nodes = CASE WHEN root_id IS NULL THEN $6::text[] ELSE '{}' END,
			named_member_id = CASE WHEN root_id IS NULL THEN $7::text END
		WHERE request_id = $1 AND outcome = 'pending'
		RETURNING id, occurred_at, named_nodes
	`, requestID, out.Outcome, out.Status, details, out.CreatedID, named, out.NamedMemberID).Scan(&eventID, &at, &stored)
	if dbx.IsNoRows(err) {
		var exists bool
		if err := tx.QueryRow(ctx,
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
	if err != nil {
		return "", op.Fail(err)
	}

	logs := stored
	if out.CreatedID != nil {
		logs = append(logs, *out.CreatedID)
	}
	if err := addLogs(ctx, tx, logs, eventID, at); err != nil {
		return "", op.Fail(err)
	}
	if err := tx.Commit(ctx); err != nil {
		return "", op.Fail(err)
	}
	op.OK("")
	return metrics.OutcomeApplied, nil
}

// List returns one page of a log, newest first. In a node's log a user
// route's event shows the member the service named as its actor, and no user
// id: a user id never leaves the person's own log.
func (s *Store) List(ctx context.Context, f Filter) ([]*Event, bool, error) {
	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "list_events", dbx.DefaultTimeout)
	defer cancel()
	defer op.End()

	var where []string
	var args []any
	add := func(cond string, v any) {
		args = append(args, v)
		where = append(where, strings.ReplaceAll(cond, "?", "$"+strconv.Itoa(len(args))))
	}
	add("k.log = ?", f.Log)
	actor := "e.actor_member_id"
	if f.NodeRead {
		actor = "CASE WHEN e.root_id IS NULL THEN e.named_member_id ELSE e.actor_member_id END"
	}
	if f.StartingAfter != "" {
		add("k.event_id < ?", f.StartingAfter)
		// The id's time is occurred_at to the millisecond. Saying so lets
		// Postgres skip newer partitions.
		add("k.occurred_at < ?", id.Time(f.StartingAfter).Add(time.Millisecond))
	}
	if f.ActorMemberID != "" {
		add(actor+" = ?", f.ActorMemberID)
	}
	if f.Service != "" {
		add("e.service = ?", f.Service)
	}
	if f.Method != "" {
		add("e.method = ?", f.Method)
	}
	if f.Route != "" {
		add("e.route = ?", f.Route)
	}
	if f.Outcome != "" {
		add("e.outcome = ?", f.Outcome)
	}
	if f.Status != nil {
		add("e.status = ?", *f.Status)
	}
	if f.RequestID != "" {
		add("e.request_id = ?", f.RequestID)
	}
	if f.CreatedID != "" {
		add("e.created_id = ?", f.CreatedID)
	}
	if len(f.Params) > 0 {
		p, err := json.Marshal(f.Params)
		if err != nil {
			return nil, false, op.Fail(err)
		}
		add("e.params @> ?::jsonb", string(p))
	}
	if f.OccurredAfter != nil {
		add("k.occurred_at >= ?", *f.OccurredAfter)
	}
	if f.OccurredBefore != nil {
		add("k.occurred_at < ?", *f.OccurredBefore)
	}
	args = append(args, f.Limit+1)

	rows, err := s.pool.Query(ctx, `SELECT `+eventColumns+`
		FROM audit_log_keys k
		JOIN audit_events e ON e.id = k.event_id AND e.occurred_at = k.occurred_at
		WHERE `+strings.Join(where, " AND ")+`
		ORDER BY k.event_id DESC
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
		if f.NodeRead {
			if e.RootID == nil {
				e.Actor.MemberID = e.NamedMemberID
			}
			e.Actor.UserID = nil
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
	var nodes, params, details []byte
	if err := row.Scan(&e.ID, &e.OccurredAt, &e.RequestID, &e.RootID, &nodes, &e.Actor.MemberID, &e.Actor.UserID,
		&e.Actor.Principal, &e.Actor.AuthType, &e.Actor.KeyID, &e.Service, &e.Method, &e.Route, &params, &e.IP,
		&e.UserAgent, &e.Outcome.Outcome, &e.Status, &details, &e.CreatedID, &e.NamedNodes, &e.NamedMemberID); err != nil {
		return nil, err
	}
	e.OccurredAt = e.OccurredAt.UTC()
	if err := json.Unmarshal(nodes, &e.Nodes); err != nil {
		return nil, fmt.Errorf("nodes: %w", err)
	}
	if err := json.Unmarshal(params, &e.Params); err != nil {
		return nil, fmt.Errorf("params: %w", err)
	}
	if details != nil {
		e.Details = json.RawMessage(details)
	}
	return &e, nil
}

// EnsurePartitions creates the monthly partitions of the events and their log
// keys covering from..to (inclusive months). Replicas serialize on an advisory lock; existing partitions are kept.
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
		for _, table := range []string{"audit_events", "audit_log_keys"} {
			name := pgx.Identifier{fmt.Sprintf("%s_y%04dm%02d", table, m.Year(), int(m.Month()))}.Sanitize()
			if _, err := tx.Exec(ctx, fmt.Sprintf(
				`CREATE TABLE IF NOT EXISTS %s PARTITION OF %s FOR VALUES FROM ('%s') TO ('%s')`,
				name, table, m.Format(time.RFC3339), m.AddDate(0, 1, 0).Format(time.RFC3339),
			)); err != nil {
				return op.Fail(err)
			}
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return op.Fail(err)
	}
	op.OK("")
	return nil
}

// MaintainPartitions keeps last month through two months ahead in place, now
// and every interval, until ctx ends. An intent outside them creates its
// month.
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
