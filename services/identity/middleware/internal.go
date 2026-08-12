package middleware

import (
	"crypto/subtle"

	"github.com/gofiber/fiber/v3"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/plat5dev/plat5/identity/errors"
)

const InternalTokenHeader = "X-Plat5-Internal-Token"

// RequireInternalToken enforces expected when non-empty.
// Empty token = network-trust only (dev). Health/metrics stay ungated by the caller.
func RequireInternalToken(expected string) fiber.Handler {
	return func(c fiber.Ctx) error {
		if expected == "" {
			return c.Next()
		}
		got := c.Get(InternalTokenHeader)
		if subtle.ConstantTimeCompare([]byte(got), []byte(expected)) != 1 {
			span := trace.SpanFromContext(c.Context())
			span.SetAttributes(attribute.String("error.kind", errors.KindAuth.String()))
			return errors.UnauthorizedError("invalid_internal_token")
		}
		return c.Next()
	}
}
