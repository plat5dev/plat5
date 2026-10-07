package httpx

import (
	"context"
	stderrors "errors"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/rs/zerolog"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/plat5dev/plat5/identity/errors"
	"github.com/plat5dev/plat5/identity/internal/apikey"
	"github.com/plat5dev/plat5/identity/internal/id"
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

func ParseListParams(c fiber.Ctx) (limit int, startingAfter string, err error) {
	limit = DefaultListLimit
	if v := strings.TrimSpace(c.Query("limit")); v != "" {
		n, parseErr := strconv.Atoi(v)
		if parseErr != nil || n < 1 {
			return 0, "", errors.FieldError("limit", errors.FallbackValidation)
		}
		limit = n
	}
	if limit > MaxListLimit {
		limit = MaxListLimit
	}
	startingAfter = strings.TrimSpace(c.Query("starting_after"))
	if startingAfter != "" && !id.Valid(startingAfter) {
		return 0, "", errors.FieldError("starting_after", errors.FallbackValidation)
	}
	return limit, startingAfter, nil
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

// DBErr maps store sentinel errors to API errors.
// NotFound/Conflict are the package sentinels to match (e.g. orgs.ErrNotFound).
// When a sentinel matches, Resource/ResourceID or Field/FieldValue fill the API details.
type DBErr struct {
	NotFound   error
	Resource   string
	ResourceID interface{}

	Conflict   error
	Field      string
	FieldValue interface{}
	Message    string
}

// ConstrainMint applies the caller credential's scopes to a key or session being created.
// requested nil means the client omitted scopes.
func ConstrainMint(c fiber.Ctx, requested []string) ([]string, error) {
	raw := c.Request().Header.Peek(apikey.CallerScopesHeader)
	if raw == nil {
		return apikey.ConstrainScopes(nil, requested)
	}
	caller, err := apikey.ParseCallerScopes(string(raw))
	if err != nil {
		return nil, err
	}
	return apikey.ConstrainScopes(caller, requested)
}

// RequireUnrestricted guards identity writes that manage the org (members,
// invites, service accounts and their keys, the org row). A restricted caller
// (X-Plat5-Scopes present, including "[]") gets 403 RESTRICTED_CREDENTIAL.
// Absent means a JWT or a key or session with scopes null: unchanged.
func RequireUnrestricted(c fiber.Ctx) error {
	if c.Request().Header.Peek(apikey.CallerScopesHeader) != nil {
		return errors.RestrictedCredential()
	}
	return c.Next()
}

// MapMintScopes turns a mint-cap failure into an API error.
// A label the caller lacks is 403. A malformed scopes header is 500.
func MapMintScopes(ctx context.Context, err error) error {
	var insufficient *apikey.InsufficientScopeError
	if stderrors.As(err, &insufficient) {
		return errors.InsufficientScope(insufficient.Missing)
	}
	LogError(ctx, "invalid caller scopes", err, errors.KindInternal)
	return errors.InternalError()
}

// MapDB maps store sentinels and *errors.ApiError through; logs unexpected errors as INTERNAL.
// NotFound always maps when the sentinel matches (Resource defaults to "resource").
// Conflict maps only when Conflict sentinel is set and matches.
func MapDB(ctx context.Context, err error, msg string, m DBErr) error {
	if err == nil {
		return nil
	}
	var apiErr *errors.ApiError
	if stderrors.As(err, &apiErr) {
		return apiErr
	}
	if m.NotFound != nil && stderrors.Is(err, m.NotFound) {
		resource := m.Resource
		if resource == "" {
			resource = "resource"
		}
		return errors.NotFoundError(resource, m.ResourceID)
	}
	if m.Conflict != nil && stderrors.Is(err, m.Conflict) {
		return errors.ConflictError(m.Message, m.Field, m.FieldValue)
	}
	LogError(ctx, msg, err, errors.KindDB)
	return errors.InternalError()
}
