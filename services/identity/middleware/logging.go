package middleware

import (
	"time"

	"github.com/gofiber/fiber/v3"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/plat5dev/plat5/identity/errors"
	"github.com/plat5dev/plat5/identity/metrics"
	"github.com/plat5dev/plat5/identity/telemetry"
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

// FailureLogger is for long-polled reads: no span, no request metric, and a
// log line only for a 5xx. A long-poll as a trace or a request metric would be
// one long span every cycle and a latency that measures nothing.
func FailureLogger(telem *telemetry.Telemetry) fiber.Handler {
	return func(c fiber.Ctx) error {
		reqLogger := telem.LoggerWithContext(c.Context())
		if requestID := c.Get("X-Request-ID"); requestID != "" {
			reqLogger = reqLogger.With().Str("request_id", requestID).Logger()
		}
		c.SetContext(reqLogger.WithContext(c.Context()))

		err := c.Next()

		status := c.Response().StatusCode()
		kind := errors.KindInternal.String()
		if err != nil {
			apiErr := errors.FromError(err)
			status = apiErr.Status
			if apiErr.Kind.String() != "" {
				kind = apiErr.Kind.String()
			}
		}
		if status >= 500 {
			routePattern := "unknown"
			if r := c.Route(); r != nil && r.Path != "" {
				routePattern = r.Path
			}
			event := reqLogger.Error().
				Bool("error", true).
				Str("route", routePattern).
				Str("method", c.Method()).
				Int("status", status).
				Str("error_kind", kind)
			if err != nil {
				event = event.Str("error_message", err.Error())
			}
			event.Msg("request completed with error")
		}
		return err
	}
}
