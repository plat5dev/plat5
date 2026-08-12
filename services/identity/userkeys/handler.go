package userkeys

import (
	"context"
	stderrors "errors"
	"strings"

	"github.com/gofiber/fiber/v3"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/plat5dev/plat5/identity/errors"
	"github.com/plat5dev/plat5/identity/internal/apikey"
	"github.com/plat5dev/plat5/identity/internal/httpx"
	"github.com/plat5dev/plat5/identity/metrics"
	"github.com/plat5dev/plat5/identity/middleware"
	"github.com/plat5dev/plat5/identity/telemetry"
)

const tracerName = "identity.userkeys"

type Handler struct {
	store  *Store
	tracer trace.Tracer
	telem  *telemetry.Telemetry
}

func NewHandler(store *Store, telem *telemetry.Telemetry) *Handler {
	return &Handler{
		store:  store,
		tracer: otel.Tracer(tracerName),
		telem:  telem,
	}
}

type CreateRequest struct {
	Name string `json:"name"`
}

type CreateResponse struct {
	ID        string `json:"id"`
	Key       string `json:"key"`
	KeyPrefix string `json:"key_prefix"`
	Name      string `json:"name"`
	CreatedAt string `json:"created_at"`
}

type ListResponse struct {
	Keys    []KeyResponse `json:"keys"`
	HasMore bool          `json:"has_more"`
}

type KeyResponse struct {
	ID        string  `json:"id"`
	KeyPrefix string  `json:"key_prefix"`
	Name      string  `json:"name"`
	CreatedAt string  `json:"created_at"`
	RevokedAt *string `json:"revoked_at"`
}

type ValidateRequest struct {
	Key string `json:"key"`
}

type ValidateResponse struct {
	Valid  bool   `json:"valid"`
	UserID string `json:"user_id,omitempty"`
}

func (h *Handler) Create(c fiber.Ctx) error {
	ctx, span := h.tracer.Start(c.Context(), "userkeys.create")
	defer span.End()

	if err := requirePathUser(c); err != nil {
		return err
	}
	userID := middleware.GetUserID(c)

	var req CreateRequest
	if err := c.Bind().Body(&req); err != nil {
		return err
	}

	name, err := apikey.NormalizeName(req.Name)
	if err != nil {
		return errors.FieldError("name", "must be at most 128 characters")
	}
	span.SetAttributes(attribute.String("key.name", name))

	plaintext, err := GenerateKey()
	if err != nil {
		h.logError(ctx, span, "failed to generate key", err, errors.KindInternal)
		return errors.InternalError()
	}

	apiKey := New(userID, name, plaintext)
	span.SetAttributes(attribute.String("key.id", apiKey.ID))

	if err := h.store.Create(ctx, apiKey); err != nil {
		h.logError(ctx, span, "failed to store user key", err, errors.KindDB)
		return errors.InternalError()
	}

	logger := h.telem.LoggerWithContext(ctx)
	logger.Info().
		Str("user_id", userID).
		Str("key_id", apiKey.ID).
		Str("key_prefix", apiKey.KeyPrefix).
		Msg("user api key created")

	metrics.RecordKeyCreated()
	span.SetStatus(codes.Ok, "created")

	return c.Status(fiber.StatusCreated).JSON(CreateResponse{
		ID:        apiKey.ID,
		Key:       plaintext,
		KeyPrefix: apiKey.KeyPrefix,
		Name:      apiKey.Name,
		CreatedAt: httpx.FormatTime(apiKey.CreatedAt),
	})
}

func (h *Handler) List(c fiber.Ctx) error {
	ctx, span := h.tracer.Start(c.Context(), "userkeys.list")
	defer span.End()

	if err := requirePathUser(c); err != nil {
		return err
	}
	userID := middleware.GetUserID(c)

	limit, offset, err := httpx.ParseListParams(c)
	if err != nil {
		return err
	}

	list, hasMore, err := h.store.List(ctx, userID, limit, offset)
	if err != nil {
		h.logError(ctx, span, "failed to list user keys", err, errors.KindDB)
		return errors.InternalError()
	}

	out := ListResponse{
		Keys:    make([]KeyResponse, 0, len(list)),
		HasMore: hasMore,
	}
	for _, k := range list {
		out.Keys = append(out.Keys, toKeyResponse(k))
	}

	span.SetAttributes(attribute.Int("keys.count", len(out.Keys)))
	span.SetStatus(codes.Ok, "ok")
	return c.JSON(out)
}

func (h *Handler) Revoke(c fiber.Ctx) error {
	ctx, span := h.tracer.Start(c.Context(), "userkeys.revoke")
	defer span.End()

	if err := requirePathUser(c); err != nil {
		return err
	}
	userID := middleware.GetUserID(c)
	keyID := c.Params("key_id")
	if keyID == "" {
		return errors.FieldError("key_id", "required")
	}
	span.SetAttributes(attribute.String("key.id", keyID))

	key, err := h.store.Revoke(ctx, userID, keyID)
	if err != nil {
		if stderrors.Is(err, ErrNotFound) {
			return errors.NotFoundError("api_key", keyID)
		}
		h.logError(ctx, span, "failed to revoke user key", err, errors.KindDB)
		return errors.InternalError()
	}

	logger := h.telem.LoggerWithContext(ctx)
	logger.Info().
		Str("user_id", userID).
		Str("key_id", key.ID).
		Msg("user api key revoked")

	metrics.RecordKeyRevoked()
	span.SetStatus(codes.Ok, "revoked")
	return c.SendStatus(fiber.StatusNoContent)
}

func (h *Handler) Validate(c fiber.Ctx) error {
	ctx, span := h.tracer.Start(c.Context(), "userkeys.validate")
	defer span.End()

	var req ValidateRequest
	if err := c.Bind().Body(&req); err != nil {
		return err
	}
	key := strings.TrimSpace(req.Key)
	if key == "" {
		return errors.FieldError("key", "required")
	}
	if !LooksLike(key) {
		return h.invalid(c, span)
	}

	keyHash := HashKey(key)
	span.SetAttributes(attribute.String("key.hash_prefix", keyHash[:8]))

	userKey, err := h.store.GetByHash(ctx, keyHash)
	if err != nil {
		if stderrors.Is(err, ErrNotFound) {
			return h.invalid(c, span)
		}
		h.logError(ctx, span, "failed to get user key", err, errors.KindDB)
		return errors.InternalError()
	}
	if userKey.IsRevoked() {
		return h.invalid(c, span)
	}

	metrics.RecordKeyValidation(true)
	span.SetAttributes(
		attribute.Bool("key.valid", true),
		attribute.String("key.id", userKey.ID),
		attribute.String("user.id", userKey.UserID),
	)
	span.SetStatus(codes.Ok, "valid")
	return c.JSON(ValidateResponse{
		Valid:  true,
		UserID: userKey.UserID,
	})
}

func (h *Handler) invalid(c fiber.Ctx, span trace.Span) error {
	metrics.RecordKeyValidation(false)
	span.SetAttributes(attribute.Bool("key.valid", false))
	span.SetStatus(codes.Ok, "invalid")
	return c.JSON(ValidateResponse{Valid: false})
}

func requirePathUser(c fiber.Ctx) error {
	caller := middleware.GetUserID(c)
	pathUser := c.Params("user_id")
	if pathUser == "" || pathUser != caller {
		return errors.NotFoundError("user", pathUser)
	}
	return nil
}

func toKeyResponse(k *APIKey) KeyResponse {
	return KeyResponse{
		ID:        k.ID,
		KeyPrefix: k.KeyPrefix,
		Name:      k.Name,
		CreatedAt: httpx.FormatTime(k.CreatedAt),
		RevokedAt: httpx.FormatTimePtr(k.RevokedAt),
	}
}

func (h *Handler) logError(ctx context.Context, span trace.Span, msg string, err error, kind errors.ErrorKind) {
	httpx.LogError(ctx, span, h.telem.LoggerWithContext(ctx), msg, err, kind)
}
