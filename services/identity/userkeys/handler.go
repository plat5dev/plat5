package userkeys

import (
	"context"
	stderrors "errors"
	"strings"

	"github.com/gofiber/fiber/v3"

	"github.com/plat5dev/plat5/identity/errors"
	"github.com/plat5dev/plat5/identity/internal/apikey"
	"github.com/plat5dev/plat5/identity/internal/httpx"
	"github.com/plat5dev/plat5/identity/metrics"
)

type keyStore interface {
	Create(ctx context.Context, key *APIKey) error
	GetByHash(ctx context.Context, keyHash string) (*APIKey, error)
	List(ctx context.Context, userID string, limit int, startingAfter string) ([]*APIKey, bool, error)
	Revoke(ctx context.Context, userID, keyID string) (*APIKey, error)
}

type Handler struct {
	store  keyStore
	prefix string
}

func NewHandler(store *Store, prefix string) *Handler {
	return &Handler{store: store, prefix: prefix}
}

type CreateRequest struct {
	Name string `json:"name"`
	// Scopes is refused when present. See apikey.ScopesRefused.
	Scopes *[]string `json:"scopes"`
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
	ctx := c.Context()
	userID := httpx.PathParam(c, "user_id")

	var req CreateRequest
	if err := c.Bind().Body(&req); err != nil {
		return err
	}

	name, err := apikey.NormalizeName(req.Name)
	if err != nil {
		return errors.FieldError("name", "Name is too long.")
	}
	if req.Scopes != nil {
		return errors.FieldError("scopes", apikey.ScopesRefused)
	}

	plaintext, err := apikey.Generate(h.prefix)
	if err != nil {
		httpx.LogError(ctx, "failed to generate key", err, errors.KindInternal)
		return errors.InternalError()
	}

	apiKey := New(userID, name, plaintext, h.prefix)
	if err := h.store.Create(ctx, apiKey); err != nil {
		return httpx.MapDB(ctx, err, "failed to store user key", httpx.DBErr{})
	}

	httpx.Logger(ctx).Info().
		Str("user_id", userID).
		Str("key_id", apiKey.ID).
		Str("key_prefix", apiKey.KeyPrefix).
		Msg("user api key created")

	metrics.RecordKeyCreated(metrics.KeyScopeUser)
	return c.Status(fiber.StatusCreated).JSON(CreateResponse{
		ID:        apiKey.ID,
		Key:       plaintext,
		KeyPrefix: apiKey.KeyPrefix,
		Name:      apiKey.Name,
		CreatedAt: httpx.FormatTime(apiKey.CreatedAt),
	})
}

func (h *Handler) List(c fiber.Ctx) error {
	ctx := c.Context()
	userID := httpx.PathParam(c, "user_id")

	limit, startingAfter, err := httpx.ParseListParams(c)
	if err != nil {
		return err
	}

	list, hasMore, err := h.store.List(ctx, userID, limit, startingAfter)
	if err != nil {
		return httpx.MapDB(ctx, err, "failed to list user keys", httpx.DBErr{})
	}

	out := ListResponse{
		Keys:    make([]KeyResponse, 0, len(list)),
		HasMore: hasMore,
	}
	for _, k := range list {
		out.Keys = append(out.Keys, toKeyResponse(k))
	}
	return c.JSON(out)
}

func (h *Handler) Revoke(c fiber.Ctx) error {
	ctx := c.Context()
	userID := httpx.PathParam(c, "user_id")
	keyID := c.Params("key_id")
	if keyID == "" {
		return errors.FieldError("key_id", errors.FallbackValidation)
	}

	key, err := h.store.Revoke(ctx, userID, keyID)
	if err != nil {
		return httpx.MapDB(ctx, err, "failed to revoke user key", httpx.DBErr{
			NotFound: ErrNotFound, Resource: "api_key", ResourceID: keyID,
		})
	}

	httpx.Logger(ctx).Info().
		Str("user_id", userID).
		Str("key_id", key.ID).
		Msg("user api key revoked")

	metrics.RecordKeyRevoked(metrics.KeyScopeUser)
	return c.SendStatus(fiber.StatusNoContent)
}

func (h *Handler) Validate(c fiber.Ctx) error {
	ctx := c.Context()

	var req ValidateRequest
	if err := c.Bind().Body(&req); err != nil {
		return err
	}
	key := strings.TrimSpace(req.Key)
	if key == "" {
		return errors.FieldError("key", errors.FallbackValidation)
	}
	if !apikey.LooksLike(key, h.prefix) {
		return h.invalid(c)
	}

	userKey, err := h.store.GetByHash(ctx, HashKey(key))
	if err != nil {
		if stderrors.Is(err, ErrNotFound) {
			return h.invalid(c)
		}
		return httpx.MapDB(ctx, err, "failed to get user key", httpx.DBErr{})
	}
	if userKey.IsRevoked() {
		return h.invalid(c)
	}

	metrics.RecordKeyValidation(metrics.KeyScopeUser, true)
	return c.JSON(ValidateResponse{Valid: true, UserID: userKey.UserID})
}

func (h *Handler) invalid(c fiber.Ctx) error {
	metrics.RecordKeyValidation(metrics.KeyScopeUser, false)
	return c.JSON(ValidateResponse{Valid: false})
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
