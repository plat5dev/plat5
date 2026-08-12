package dbx

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/plat5dev/plat5/identity/metrics"
)

const DefaultTimeout = 5 * time.Second

// Op tracks one DB operation's span, timing, and metrics.
type Op struct {
	span  trace.Span
	name  string
	start time.Time
}

// Begin starts a db.* span with standard attributes. Caller must defer op.End().
func Begin(ctx context.Context, tracer trace.Tracer, name string, attrs ...attribute.KeyValue) (context.Context, *Op) {
	ctx, span := tracer.Start(ctx, "db."+name)
	all := make([]attribute.KeyValue, 0, 2+len(attrs))
	all = append(all,
		attribute.String("db.system", "postgresql"),
		attribute.String("db.operation.name", name),
	)
	all = append(all, attrs...)
	span.SetAttributes(all...)
	return ctx, &Op{span: span, name: name, start: time.Now()}
}

// BeginTimeout is Begin plus a deadline. Caller must defer cancel and op.End().
func BeginTimeout(ctx context.Context, tracer trace.Tracer, name string, timeout time.Duration, attrs ...attribute.KeyValue) (context.Context, context.CancelFunc, *Op) {
	ctx, op := Begin(ctx, tracer, name, attrs...)
	ctx, cancel := context.WithTimeout(ctx, timeout)
	return ctx, cancel, op
}

func (o *Op) End() {
	o.span.End()
}

func (o *Op) Span() trace.Span {
	return o.span
}

// OK records success metrics and sets span status.
func (o *Op) OK(msg string) {
	if msg == "" {
		msg = "ok"
	}
	metrics.RecordDBOperation(o.name, time.Since(o.start), nil)
	o.span.SetStatus(codes.Ok, msg)
}

// Expected is a non-error outcome (not found, already revoked). Metrics count success.
func (o *Op) Expected(msg string, err error) error {
	metrics.RecordDBOperation(o.name, time.Since(o.start), nil)
	o.span.SetStatus(codes.Ok, msg)
	return err
}

// SoftFail records metricErr on the error counter but Ok span status (e.g. unique conflict).
func (o *Op) SoftFail(msg string, metricErr, ret error) error {
	metrics.RecordDBOperation(o.name, time.Since(o.start), metricErr)
	o.span.SetStatus(codes.Ok, msg)
	return ret
}

// Fail records error metrics/span and wraps err with the op name.
func (o *Op) Fail(err error) error {
	metrics.RecordDBOperation(o.name, time.Since(o.start), err)
	o.span.SetStatus(codes.Error, err.Error())
	o.span.RecordError(err)
	return fmt.Errorf("%s: %w", o.name, err)
}

// Attr sets extra span attributes mid-op.
func (o *Op) Attr(attrs ...attribute.KeyValue) {
	o.span.SetAttributes(attrs...)
}

func IsUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

func IsNoRows(err error) bool {
	return errors.Is(err, pgx.ErrNoRows)
}

// Scannable is pgx.Row / pgx.Rows.
type Scannable interface {
	Scan(dest ...any) error
}
