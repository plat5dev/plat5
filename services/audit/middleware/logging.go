package middleware

import (
	"time"

	"github.com/gofiber/fiber/v3"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/plat5dev/plat5/audit/errors"
	"github.com/plat5dev/plat5/audit/metrics"
	"github.com/plat5dev/plat5/audit/telemetry"
)

// HTTPSpanName is `{method} {route}` when a template exists, else `{method}`.
func HTTPSpanName(c fiber.Ctx) string {
	method := c.Method()
	if r := c.Route(); r != nil && r.Path != "" {
		return method + " " + r.Path
	}
	return method
}

func RequestLogger(telem *telemetry.Telemetry) fiber.Handler {
	return func(c fiber.Ctx) error {
		start := time.Now()

		// Inject request-scoped logger before handlers run.
		reqLogger := telem.LoggerWithContext(c.Context())
		if requestID := c.Get("X-Request-ID"); requestID != "" {
			reqLogger = reqLogger.With().Str("request_id", requestID).Logger()
			span := trace.SpanFromContext(c.Context())
			span.SetAttributes(attribute.String("request_id", requestID))
		}
		c.SetContext(reqLogger.WithContext(c.Context()))

		err := c.Next()

		// ErrorHandler runs after middleware, so the response status is not set yet
		// when a handler returns an error. Map it the same way the ErrorHandler does.
		status := c.Response().StatusCode()
		var apiErr *errors.ApiError
		if err != nil {
			apiErr = errors.FromError(err)
			status = apiErr.Status
		}
		duration := time.Since(start)

		routePattern := "unknown"
		if r := c.Route(); r != nil && r.Path != "" {
			routePattern = r.Path
		}

		metrics.ObserveRequest(routePattern, c.Method(), status, duration)

		kind := errors.KindInternal.String()
		if apiErr != nil && apiErr.Kind.String() != "" {
			kind = apiErr.Kind.String()
		}
		if status >= 500 {
			span := trace.SpanFromContext(c.Context())
			span.SetAttributes(attribute.String("error.kind", kind))
			span.SetStatus(codes.Error, "")
		}

		logger := reqLogger.With().
			Str("route", routePattern).
			Str("method", c.Method()).
			Int("status", status).
			Float64("duration_ms", float64(duration.Microseconds())/1000.0).
			Logger()

		if err != nil {
			if status >= 500 {
				logger.Error().
					Bool("error", true).
					Str("error_kind", kind).
					Str("error_message", err.Error()).
					Msg("request completed with error")
			} else {
				logger.Warn().Msg("request completed with client error")
			}
			return err
		}

		logger.Info().Msg("request completed")
		return nil
	}
}
