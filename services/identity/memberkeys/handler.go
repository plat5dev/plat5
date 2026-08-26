package memberkeys

import (
	"context"
	stderrors "errors"
	"strings"

	"github.com/gofiber/fiber/v3"

	"github.com/plat5dev/plat5/identity/errors"
	"github.com/plat5dev/plat5/identity/internal/apikey"
	"github.com/plat5dev/plat5/identity/internal/httpx"
	"github.com/plat5dev/plat5/identity/metrics"
	"github.com/plat5dev/plat5/identity/middleware"
	"github.com/plat5dev/plat5/identity/orgs"
)

type Handler struct {
	store    *Store
	orgStore *orgs.Store
	prefix   string
}

func NewHandler(store *Store, orgStore *orgs.Store, prefix string) *Handler {
	return &Handler{store: store, orgStore: orgStore, prefix: prefix}
}

type CreateRequest struct {
	Name   string    `json:"name"`
	Scopes *[]string `json:"scopes"`
}

type CreateResponse struct {
	ID        string    `json:"id"`
	Key       string    `json:"key"`
	KeyPrefix string    `json:"key_prefix"`
	Name      string    `json:"name"`
	Scopes    *[]string `json:"scopes"`
	CreatedAt string    `json:"created_at"`
}

type ListResponse struct {
	Keys    []KeyResponse `json:"keys"`
	HasMore bool          `json:"has_more"`
}

type KeyResponse struct {
	ID        string    `json:"id"`
	KeyPrefix string    `json:"key_prefix"`
	Name      string    `json:"name"`
	Scopes    *[]string `json:"scopes"`
	CreatedAt string    `json:"created_at"`
	RevokedAt *string   `json:"revoked_at"`
}

type ValidateRequest struct {
	Key string `json:"key"`
}

type ValidateResponse struct {
	Valid          bool      `json:"valid"`
	MemberID       string    `json:"member_id,omitempty"`
	OrganizationID string    `json:"organization_id,omitempty"`
	Scopes         *[]string `json:"scopes,omitempty"`
}

type validMemberKeyResponse struct {
	Valid          bool      `json:"valid"`
	MemberID       string    `json:"member_id"`
	OrganizationID string    `json:"organization_id"`
	Scopes         *[]string `json:"scopes"`
}

func (h *Handler) Create(c fiber.Ctx) error {
	ctx := c.Context()
	userID := middleware.GetUserID(c)
	orgID := c.Params("organization_id")
	memberID := c.Params("member_id")

	target, err := h.authorizeKeyManage(ctx, orgID, memberID, userID)
	if err != nil {
		return err
	}
	if target.Status != orgs.StatusActive {
		return errors.NotFoundError("member", memberID)
	}

	var req CreateRequest
	if err := c.Bind().Body(&req); err != nil {
		return err
	}
	name, err := apikey.NormalizeName(req.Name)
	if err != nil {
		return errors.FieldError("name", "Name is too long.")
	}
	scopes, err := apikey.ParseScopes(req.Scopes)
	if err != nil {
		return err
	}

	plaintext, err := apikey.Generate(h.prefix)
	if err != nil {
		httpx.LogError(ctx, "failed to generate key", err, errors.KindInternal)
		return errors.InternalError()
	}

	apiKey := New(memberID, name, plaintext, h.prefix, scopes)
	if err := h.store.Create(ctx, apiKey); err != nil {
		return httpx.MapDB(ctx, err, "failed to store member key", httpx.DBErr{})
	}

	httpx.Logger(ctx).Info().
		Str("organization_id", orgID).
		Str("member_id", memberID).
		Str("key_id", apiKey.ID).
		Str("key_prefix", apiKey.KeyPrefix).
		Msg("member api key created")

	metrics.RecordKeyCreated(metrics.KeyScopeMember)
	return c.Status(fiber.StatusCreated).JSON(CreateResponse{
		ID:        apiKey.ID,
		Key:       plaintext,
		KeyPrefix: apiKey.KeyPrefix,
		Name:      apiKey.Name,
		Scopes:    apikey.PointerForJSON(apiKey.Scopes),
		CreatedAt: httpx.FormatTime(apiKey.CreatedAt),
	})
}

func (h *Handler) List(c fiber.Ctx) error {
	ctx := c.Context()
	userID := middleware.GetUserID(c)
	orgID := c.Params("organization_id")
	memberID := c.Params("member_id")

	if _, err := h.authorizeKeyManage(ctx, orgID, memberID, userID); err != nil {
		return err
	}

	limit, offset, err := httpx.ParseListParams(c)
	if err != nil {
		return err
	}

	list, hasMore, err := h.store.List(ctx, memberID, limit, offset)
	if err != nil {
		return httpx.MapDB(ctx, err, "failed to list member keys", httpx.DBErr{})
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
	userID := middleware.GetUserID(c)
	orgID := c.Params("organization_id")
	memberID := c.Params("member_id")
	keyID := c.Params("key_id")

	if _, err := h.authorizeKeyManage(ctx, orgID, memberID, userID); err != nil {
		return err
	}
	if keyID == "" {
		return errors.FieldError("key_id", errors.FallbackValidation)
	}

	key, err := h.store.Revoke(ctx, memberID, keyID)
	if err != nil {
		return httpx.MapDB(ctx, err, "failed to revoke member key", httpx.DBErr{
			NotFound: ErrNotFound, Resource: "api_key", ResourceID: keyID,
		})
	}

	httpx.Logger(ctx).Info().
		Str("organization_id", orgID).
		Str("member_id", memberID).
		Str("key_id", key.ID).
		Msg("member api key revoked")

	metrics.RecordKeyRevoked(metrics.KeyScopeMember)
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

	memberKey, err := h.store.GetByHash(ctx, HashKey(key))
	if err != nil {
		if stderrors.Is(err, ErrNotFound) {
			return h.invalid(c)
		}
		return httpx.MapDB(ctx, err, "failed to get member key", httpx.DBErr{})
	}
	if memberKey.Key.IsRevoked() {
		return h.invalid(c)
	}
	if memberKey.MemberStatus != string(orgs.StatusActive) {
		return h.invalid(c)
	}

	metrics.RecordKeyValidation(metrics.KeyScopeMember, true)
	return c.JSON(validMemberKeyResponse{
		Valid:          true,
		MemberID:       memberKey.Key.MemberID,
		OrganizationID: memberKey.OrganizationID,
		Scopes:         apikey.PointerForJSON(memberKey.Key.Scopes),
	})
}

func (h *Handler) authorizeKeyManage(ctx context.Context, orgID, memberID, userID string) (*orgs.Member, error) {
	actor, err := orgs.RequireActiveMember(ctx, h.orgStore, orgID, userID)
	if err != nil {
		return nil, httpx.MapDB(ctx, err, "failed to load actor member", httpx.DBErr{})
	}

	target, err := h.orgStore.GetMember(ctx, orgID, memberID)
	if err != nil {
		return nil, httpx.MapDB(ctx, err, "failed to load target member", httpx.DBErr{
			NotFound: orgs.ErrNotFound, Resource: "member", ResourceID: memberID,
		})
	}
	if target.Status == orgs.StatusRemoved {
		return nil, errors.NotFoundError("member", memberID)
	}
	if err := orgs.CanManageMemberKeys(actor, target); err != nil {
		return nil, err
	}
	return target, nil
}

func (h *Handler) invalid(c fiber.Ctx) error {
	metrics.RecordKeyValidation(metrics.KeyScopeMember, false)
	return c.JSON(ValidateResponse{Valid: false})
}

func toKeyResponse(k *APIKey) KeyResponse {
	return KeyResponse{
		ID:        k.ID,
		KeyPrefix: k.KeyPrefix,
		Name:      k.Name,
		Scopes:    apikey.PointerForJSON(k.Scopes),
		CreatedAt: httpx.FormatTime(k.CreatedAt),
		RevokedAt: httpx.FormatTimePtr(k.RevokedAt),
	}
}
