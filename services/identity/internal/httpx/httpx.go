package httpx

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/rs/zerolog"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/plat5dev/plat5/identity/errors"
)

const (
	DefaultListLimit = 50
	MaxListLimit     = 100
)

func ParseListParams(c fiber.Ctx) (limit, offset int, err error) {
	limit = DefaultListLimit
	if v := strings.TrimSpace(c.Query("limit")); v != "" {
		n, parseErr := strconv.Atoi(v)
		if parseErr != nil || n < 1 {
			return 0, 0, errors.FieldError("limit", "must be a positive integer")
		}
		limit = n
	}
	if limit > MaxListLimit {
		limit = MaxListLimit
	}
	if v := strings.TrimSpace(c.Query("offset")); v != "" {
		n, parseErr := strconv.Atoi(v)
		if parseErr != nil || n < 0 {
			return 0, 0, errors.FieldError("offset", "must be a non-negative integer")
		}
		offset = n
	}
	return limit, offset, nil
}

func FormatTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}

func FormatTimePtr(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := FormatTime(*t)
	return &s
}

// LogError records span error state and logs via the provided logger factory.
func LogError(ctx context.Context, span trace.Span, logger zerolog.Logger, msg string, err error, kind errors.ErrorKind) {
	if span != nil {
		span.SetStatus(codes.Error, msg)
		span.SetAttributes(
			attribute.String("error.kind", kind.String()),
			attribute.String("error.message", err.Error()),
		)
		span.RecordError(err)
	}
	logger.Error().
		Str("error_kind", kind.String()).
		Str("error_message", err.Error()).
		Msg(msg)
}
