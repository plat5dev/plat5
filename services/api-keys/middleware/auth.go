package middleware

import (
	"github.com/gofiber/fiber/v3"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/plat5dev/plat5/api-keys/errors"
)

const (
	UserIDHeader = "X-User-Id"
	UserIDKey    = "user_id"
)

// RequireUserID validates X-User-Id (gateway-injected). Missing header → INTERNAL_ERROR.
func RequireUserID() fiber.Handler {
	return func(c fiber.Ctx) error {
		userID := c.Get(UserIDHeader)
		if userID == "" {
			span := trace.SpanFromContext(c.Context())
			span.SetAttributes(
				attribute.String("error.kind", errors.KindInternal.String()),
			)
			return errors.InternalError()
		}

		c.Locals(UserIDKey, userID)
		span := trace.SpanFromContext(c.Context())
		span.SetAttributes(attribute.String("user.id", userID))

		return c.Next()
	}
}

func GetUserID(c fiber.Ctx) string {
	if userID, ok := c.Locals(UserIDKey).(string); ok {
		return userID
	}
	return ""
}
