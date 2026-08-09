package middleware

import (
	"crypto/subtle"
	"os"

	"github.com/gofiber/fiber/v3"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/plat5dev/plat5/api-keys/errors"
)

const InternalTokenHeader = "X-Plat5-Internal-Token"

// RequireInternalToken enforces INTERNAL_AUTH_TOKEN when set.
// Unset token = network-trust only (dev). Health/metrics stay ungated.
func RequireInternalToken() fiber.Handler {
	expected := os.Getenv("INTERNAL_AUTH_TOKEN")
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
