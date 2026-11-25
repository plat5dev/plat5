package dbx

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/plat5dev/plat5/audit/metrics"
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

// Fail records error metrics/span and wraps err with the op name.
func (o *Op) Fail(err error) error {
	metrics.RecordDBOperation(o.name, time.Since(o.start), err)
	o.span.SetStatus(codes.Error, err.Error())
	o.span.RecordError(err)
	return fmt.Errorf("%s: %w", o.name, err)
}

// Unavailable reports whether err means Postgres couldn't be reached or didn't
// answer in time: a failed connect, a closed connection, a network
// error, a timeout, or Postgres refusing connections. Those are 503s
// (docs/reference/errors.md); any other failure is a 500.
func Unavailable(err error) bool {
	if err == nil {
		return false
	}
	var connectErr *pgconn.ConnectError
	var netErr net.Error
	var pgErr *pgconn.PgError
	switch {
	case errors.As(err, &connectErr), errors.As(err, &netErr):
		return true
	case errors.Is(err, context.DeadlineExceeded), pgconn.Timeout(err):
		return true
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, net.ErrClosed):
		return true
	case errors.As(err, &pgErr):
		// Class 08, connection exception; 53300, too many connections;
		// 57P01 to 57P03, an admin or crash shutdown, or not accepting yet.
		switch {
		case len(pgErr.Code) == 5 && pgErr.Code[:2] == "08":
			return true
		case pgErr.Code == "53300", pgErr.Code == "57P01", pgErr.Code == "57P02", pgErr.Code == "57P03":
			return true
		}
	}
	return false
}

func IsNoRows(err error) bool {
	return errors.Is(err, pgx.ErrNoRows)
}

// Scannable is pgx.Row / pgx.Rows.
type Scannable interface {
	Scan(dest ...any) error
}
