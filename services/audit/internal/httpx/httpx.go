package httpx

import (
	"context"
	"strings"

	"github.com/gofiber/fiber/v3"
	"github.com/rs/zerolog"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/plat5dev/plat5/audit/errors"
)

const (
	DefaultListLimit = 50
	MaxListLimit     = 100
)

// PathParam copies a route parameter. Fiber's param strings alias the request
// buffer, which is reused on the next request.
func PathParam(c fiber.Ctx, name string) string {
	return strings.Clone(c.Params(name))
}

// Logger returns the request-scoped logger from ctx (see middleware.RequestLogger).
func Logger(ctx context.Context) *zerolog.Logger {
	return zerolog.Ctx(ctx)
}

// LogError records span error state and logs.
func LogError(ctx context.Context, msg string, err error, kind errors.ErrorKind) {
	span := trace.SpanFromContext(ctx)
	if span.IsRecording() {
		span.SetStatus(codes.Error, msg)
		span.SetAttributes(
			attribute.String("error.kind", kind.String()),
			attribute.String("error.message", err.Error()),
		)
		span.RecordError(err)
	}
	Logger(ctx).Error().
		Str("error_kind", kind.String()).
		Str("error_message", err.Error()).
		Msg(msg)
}
