package middleware

import (
	"time"

	"github.com/gofiber/fiber/v3"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/plat5dev/plat5/identity/errors"
	"github.com/plat5dev/plat5/identity/metrics"
	"github.com/plat5dev/plat5/identity/telemetry"
)

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
		if userID := c.Get("X-User-Id"); userID != "" {
			reqLogger = reqLogger.With().Str("user_id", userID).Logger()
		}
		c.SetContext(reqLogger.WithContext(c.Context()))

		err := c.Next()

		// ErrorHandler runs after middleware; response status is still default
		// when handlers return *ApiError. Prefer the error's status for logs/metrics.
		status := resolveStatus(c, err)
		duration := time.Since(start)

		routePattern := "unknown"
		if r := c.Route(); r != nil && r.Path != "" {
			routePattern = r.Path
		}

		metrics.ObserveRequest(routePattern, c.Method(), status, duration)

		logger := reqLogger.With().
			Str("route", routePattern).
			Str("method", c.Method()).
			Int("status", status).
			Float64("duration_ms", float64(duration.Microseconds())/1000.0).
			Logger()

		if err != nil {
			if apiErr, ok := err.(*errors.ApiError); ok && apiErr.Status >= 500 {
				kind := apiErr.Kind.String()
				if kind == "" {
					kind = errors.KindInternal.String()
				}
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

func resolveStatus(c fiber.Ctx, err error) int {
	if err != nil {
		switch e := err.(type) {
		case *errors.ApiError:
			return e.Status
		case *fiber.Error:
			return e.Code
		default:
			if code := c.Response().StatusCode(); code >= 400 {
				return code
			}
			return fiber.StatusInternalServerError
		}
	}
	return c.Response().StatusCode()
}
