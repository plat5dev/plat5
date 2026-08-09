package keys

import (
	"context"
	stderrors "errors"
	"strconv"
	"strings"

	"github.com/gofiber/fiber/v3"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/plat5dev/plat5/api-keys/errors"
	"github.com/plat5dev/plat5/api-keys/metrics"
	"github.com/plat5dev/plat5/api-keys/middleware"
	"github.com/plat5dev/plat5/api-keys/telemetry"
)

const tracerName = "api-keys.keys"

// Handler handles API key HTTP endpoints
type Handler struct {
	store  KeyStore
	tracer trace.Tracer
	telem  *telemetry.Telemetry
}

// NewHandler creates a new Handler
func NewHandler(store KeyStore, telem *telemetry.Telemetry) *Handler {
	return &Handler{
		store:  store,
		tracer: otel.Tracer(tracerName),
		telem:  telem,
	}
}

// CreateRequest is the request body for creating a key
type CreateRequest struct {
	Name string `json:"name"`
}

// CreateResponse is the response for creating a key
type CreateResponse struct {
	ID        string `json:"id"`
	Key       string `json:"key"`
	KeyPrefix string `json:"key_prefix"`
	Name      string `json:"name"`
	CreatedAt string `json:"created_at"`
}

// ListResponse is the response for listing keys
type ListResponse struct {
	Keys    []KeyResponse `json:"keys"`
	HasMore bool          `json:"has_more"`
}

// KeyResponse is a single key in list response
type KeyResponse struct {
	ID        string  `json:"id"`
	KeyPrefix string  `json:"key_prefix"`
	Name      string  `json:"name"`
	CreatedAt string  `json:"created_at"`
	RevokedAt *string `json:"revoked_at"`
}

// RevokeResponse is the response for revoking a key
type RevokeResponse struct {
	Revoked bool `json:"revoked"`
}

// ValidateRequest is the request body for validating a key
type ValidateRequest struct {
	Key string `json:"key"`
}

// ValidateResponse is the response for validating a key
type ValidateResponse struct {
	Valid  bool    `json:"valid"`
	UserID *string `json:"user_id,omitempty"`
}

// Create handles POST /api/keys
// Requires RequireUserID middleware
func (h *Handler) Create(c fiber.Ctx) error {
	ctx, span := h.tracer.Start(c.Context(), "api-keys.create")
	defer span.End()

	userID := middleware.GetUserID(c)

	var req CreateRequest
	if err := c.Bind().Body(&req); err != nil {
		return err
	}

	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = "Unnamed Key"
	}
	if len(name) > MaxKeyNameLen {
		return errors.ValidationError("Request validation failed", map[string]interface{}{
			"fields": []map[string]string{
				{"path": "name", "message": "must be at most 128 characters"},
			},
		})
	}
	req.Name = name
	span.SetAttributes(attribute.String("key.name", req.Name))

	// Generate the key
	key, err := GenerateKey()
	if err != nil {
		h.logError(ctx, span, "failed to generate key", err, errors.KindInternal)
		return errors.InternalError()
	}

	apiKey := NewAPIKey(userID, req.Name, key)
	span.SetAttributes(attribute.String("key.id", apiKey.ID))

	// Store the key
	if err := h.store.Create(ctx, apiKey); err != nil {
		h.logError(ctx, span, "failed to store key", err, errors.KindDB)
		return errors.InternalError()
	}

	logger := h.telem.LoggerWithContext(ctx)
	logger.Info().
		Str("user_id", userID).
		Str("key_id", apiKey.ID).
		Str("key_prefix", apiKey.KeyPrefix).
		Msg("api key created")

	metrics.RecordKeyCreated()
	span.SetStatus(codes.Ok, "key created")

	return c.Status(fiber.StatusCreated).JSON(CreateResponse{
		ID:        apiKey.ID,
		Key:       key,
		KeyPrefix: apiKey.KeyPrefix,
		Name:      apiKey.Name,
		CreatedAt: apiKey.CreatedAt.Format("2006-01-02T15:04:05Z07:00"),
	})
}

// List handles GET /api/keys
// Requires RequireUserID middleware
func (h *Handler) List(c fiber.Ctx) error {
	ctx, span := h.tracer.Start(c.Context(), "api-keys.list")
	defer span.End()

	userID := middleware.GetUserID(c)

	limit, offset, err := parseListParams(c)
	if err != nil {
		return err
	}

	keys, hasMore, err := h.store.ListByUser(ctx, userID, limit, offset)
	if err != nil {
		h.logError(ctx, span, "failed to list keys", err, errors.KindDB)
		return errors.InternalError()
	}

	span.SetAttributes(attribute.Int("keys.count", len(keys)))
	span.SetStatus(codes.Ok, "keys listed")

	response := ListResponse{
		Keys:    make([]KeyResponse, len(keys)),
		HasMore: hasMore,
	}
	for i, k := range keys {
		response.Keys[i] = KeyResponse{
			ID:        k.ID,
			KeyPrefix: k.KeyPrefix,
			Name:      k.Name,
			CreatedAt: k.CreatedAt.Format("2006-01-02T15:04:05Z07:00"),
		}
		if k.RevokedAt != nil {
			revokedAt := k.RevokedAt.Format("2006-01-02T15:04:05Z07:00")
			response.Keys[i].RevokedAt = &revokedAt
		}
	}

	return c.JSON(response)
}

// Revoke handles DELETE /api/keys/:id
// Requires RequireUserID middleware
func (h *Handler) Revoke(c fiber.Ctx) error {
	ctx, span := h.tracer.Start(c.Context(), "api-keys.revoke")
	defer span.End()

	userID := middleware.GetUserID(c)

	keyID := c.Params("id")
	if keyID == "" {
		h.logClientError(ctx, span, "missing key id", nil)
		return errors.ValidationError("Request validation failed", map[string]interface{}{
			"fields": []map[string]string{
				{"path": "id", "message": "missing key id"},
			},
		})
	}
	span.SetAttributes(attribute.String("key.id", keyID))

	// Find the key
	key, err := h.store.GetByID(ctx, userID, keyID)
	if err != nil {
		if stderrors.Is(err, ErrNotFound) {
			h.logClientError(ctx, span, "key not found", nil)
			return errors.NotFoundError("api key", keyID)
		}
		h.logError(ctx, span, "failed to find key", err, errors.KindDB)
		return errors.InternalError()
	}

	if key.IsRevoked() {
		logger := h.telem.LoggerWithContext(ctx)
		logger.Info().Str("user_id", userID).Str("key_id", keyID).Msg("key already revoked")
		span.SetStatus(codes.Ok, "key already revoked")
		return c.JSON(RevokeResponse{Revoked: true})
	}

	// Revoke the key
	if err := h.store.Revoke(ctx, key); err != nil {
		h.logError(ctx, span, "failed to revoke key", err, errors.KindDB)
		return errors.InternalError()
	}

	logger := h.telem.LoggerWithContext(ctx)
	logger.Info().
		Str("user_id", userID).
		Str("key_id", keyID).
		Msg("api key revoked")

	metrics.RecordKeyRevoked()
	span.SetStatus(codes.Ok, "key revoked")

	return c.JSON(RevokeResponse{Revoked: true})
}

// Validate handles POST /internal/keys/validate (internal listener; gateway only).
func (h *Handler) Validate(c fiber.Ctx) error {
	ctx, span := h.tracer.Start(c.Context(), "api-keys.validate")
	defer span.End()

	var req ValidateRequest
	if err := c.Bind().Body(&req); err != nil {
		return err
	}

	if req.Key == "" {
		h.logClientError(ctx, span, "missing key", nil)
		return errors.ValidationError("Request validation failed", map[string]interface{}{
			"fields": []map[string]string{
				{"path": "key", "message": "missing key"},
			},
		})
	}

	keyHash := HashKey(req.Key)
	span.SetAttributes(attribute.String("key.hash_prefix", keyHash[:8]))

	key, err := h.store.GetByHash(ctx, keyHash)
	if err != nil {
		if stderrors.Is(err, ErrNotFound) {
			h.logClientError(ctx, span, "key not found", nil)
			span.SetAttributes(
				attribute.Bool("key.valid", false),
				attribute.String("key.invalid_reason", "not_found"),
			)
			span.SetStatus(codes.Ok, "key not found")
			metrics.RecordKeyValidation(false)
			return c.JSON(ValidateResponse{Valid: false})
		}
		h.logError(ctx, span, "failed to get key", err, errors.KindDB)
		return errors.InternalError()
	}

	if key.IsRevoked() {
		logger := h.telem.LoggerWithContext(ctx)
		logger.Info().
			Str("key_hash_prefix", keyHash[:8]).
			Str("user_id", key.UserID).
			Msg("key revoked")
		span.SetAttributes(
			attribute.Bool("key.valid", false),
			attribute.String("key.invalid_reason", "revoked"),
			attribute.String("user.id", key.UserID),
		)
		span.SetStatus(codes.Ok, "key revoked")
		metrics.RecordKeyValidation(false)
		return c.JSON(ValidateResponse{Valid: false})
	}

	logger := h.telem.LoggerWithContext(ctx)
	logger.Info().
		Str("key_hash_prefix", keyHash[:8]).
		Str("user_id", key.UserID).
		Str("key_id", key.ID).
		Msg("key validated")

	span.SetAttributes(
		attribute.Bool("key.valid", true),
		attribute.String("user.id", key.UserID),
		attribute.String("key.id", key.ID),
	)
	span.SetStatus(codes.Ok, "key valid")
	metrics.RecordKeyValidation(true)

	return c.JSON(ValidateResponse{
		Valid:  true,
		UserID: &key.UserID,
	})
}

func parseListParams(c fiber.Ctx) (limit, offset int, err error) {
	limit = DefaultListLimit
	if v := strings.TrimSpace(c.Query("limit")); v != "" {
		n, parseErr := strconv.Atoi(v)
		if parseErr != nil || n < 1 {
			return 0, 0, errors.ValidationError("Request validation failed", map[string]interface{}{
				"fields": []map[string]string{{"path": "limit", "message": "must be a positive integer"}},
			})
		}
		limit = n
	}
	if limit > MaxListLimit {
		limit = MaxListLimit
	}

	if v := strings.TrimSpace(c.Query("offset")); v != "" {
		n, parseErr := strconv.Atoi(v)
		if parseErr != nil || n < 0 {
			return 0, 0, errors.ValidationError("Request validation failed", map[string]interface{}{
				"fields": []map[string]string{{"path": "offset", "message": "must be a non-negative integer"}},
			})
		}
		offset = n
	}
	return limit, offset, nil
}

// logError logs an unexpected error, marks the span as failed, and records the exception.
func (h *Handler) logError(ctx context.Context, span trace.Span, msg string, err error, kind errors.ErrorKind) {
	span.SetStatus(codes.Error, msg)
	span.SetAttributes(
		attribute.String("error.kind", kind.String()),
		attribute.String("error.message", err.Error()),
	)
	span.RecordError(err)

	logger := h.telem.LoggerWithContext(ctx)
	logger.Error().
		Str("error_kind", kind.String()).
		Str("error_message", err.Error()).
		Msg(msg)
}

// logClientError logs a client-side issue. It does not mark the span as failed
// and does not set error.kind — 4xx responses are normal business outcomes.
func (h *Handler) logClientError(ctx context.Context, span trace.Span, msg string, err error) {
	if err != nil {
		span.RecordError(err)
	}

	logger := h.telem.LoggerWithContext(ctx)
	logger.Warn().Msg(msg)
}
