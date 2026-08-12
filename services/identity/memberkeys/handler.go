package memberkeys

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
	"github.com/plat5dev/plat5/identity/orgs"
	"github.com/plat5dev/plat5/identity/telemetry"
)

const tracerName = "identity.memberkeys"

type Handler struct {
	store    *Store
	orgStore *orgs.Store
	tracer   trace.Tracer
	telem    *telemetry.Telemetry
}

func NewHandler(store *Store, orgStore *orgs.Store, telem *telemetry.Telemetry) *Handler {
	return &Handler{
		store:    store,
		orgStore: orgStore,
		tracer:   otel.Tracer(tracerName),
		telem:    telem,
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
	Valid            bool    `json:"valid"`
	MemberID         string  `json:"member_id,omitempty"`
	OrganizationID   string  `json:"organization_id,omitempty"`
	UserID           *string `json:"user_id,omitempty"`
	ServiceAccountID *string `json:"service_account_id,omitempty"`
}

func (h *Handler) Create(c fiber.Ctx) error {
	ctx, span := h.tracer.Start(c.Context(), "memberkeys.create")
	defer span.End()

	userID := middleware.GetUserID(c)
	orgID := c.Params("organization_id")
	memberID := c.Params("member_id")
	span.SetAttributes(
		attribute.String("organization.id", orgID),
		attribute.String("member.id", memberID),
	)

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
		return errors.FieldError("name", "must be at most 128 characters")
	}

	plaintext, err := GenerateKey()
	if err != nil {
		h.logError(ctx, span, "failed to generate key", err, errors.KindInternal)
		return errors.InternalError()
	}

	apiKey := New(memberID, name, plaintext)
	if err := h.store.Create(ctx, apiKey); err != nil {
		h.logError(ctx, span, "failed to store member key", err, errors.KindDB)
		return errors.InternalError()
	}

	logger := h.telem.LoggerWithContext(ctx)
	logger.Info().
		Str("organization_id", orgID).
		Str("member_id", memberID).
		Str("key_id", apiKey.ID).
		Str("key_prefix", apiKey.KeyPrefix).
		Msg("member api key created")

	metrics.RecordKeyCreated()
	span.SetAttributes(attribute.String("key.id", apiKey.ID))
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
	ctx, span := h.tracer.Start(c.Context(), "memberkeys.list")
	defer span.End()

	userID := middleware.GetUserID(c)
	orgID := c.Params("organization_id")
	memberID := c.Params("member_id")
	span.SetAttributes(
		attribute.String("organization.id", orgID),
		attribute.String("member.id", memberID),
	)

	if _, err := h.authorizeKeyManage(ctx, orgID, memberID, userID); err != nil {
		return err
	}

	limit, offset, err := httpx.ParseListParams(c)
	if err != nil {
		return err
	}

	list, hasMore, err := h.store.List(ctx, memberID, limit, offset)
	if err != nil {
		h.logError(ctx, span, "failed to list member keys", err, errors.KindDB)
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
	ctx, span := h.tracer.Start(c.Context(), "memberkeys.revoke")
	defer span.End()

	userID := middleware.GetUserID(c)
	orgID := c.Params("organization_id")
	memberID := c.Params("member_id")
	keyID := c.Params("key_id")
	span.SetAttributes(
		attribute.String("organization.id", orgID),
		attribute.String("member.id", memberID),
		attribute.String("key.id", keyID),
	)

	if _, err := h.authorizeKeyManage(ctx, orgID, memberID, userID); err != nil {
		return err
	}
	if keyID == "" {
		return errors.FieldError("key_id", "required")
	}

	key, err := h.store.Revoke(ctx, memberID, keyID)
	if err != nil {
		if stderrors.Is(err, ErrNotFound) {
			return errors.NotFoundError("api_key", keyID)
		}
		h.logError(ctx, span, "failed to revoke member key", err, errors.KindDB)
		return errors.InternalError()
	}

	logger := h.telem.LoggerWithContext(ctx)
	logger.Info().
		Str("organization_id", orgID).
		Str("member_id", memberID).
		Str("key_id", key.ID).
		Msg("member api key revoked")

	metrics.RecordKeyRevoked()
	span.SetStatus(codes.Ok, "revoked")
	return c.SendStatus(fiber.StatusNoContent)
}

func (h *Handler) Validate(c fiber.Ctx) error {
	ctx, span := h.tracer.Start(c.Context(), "memberkeys.validate")
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

	memberKey, err := h.store.GetByHash(ctx, keyHash)
	if err != nil {
		if stderrors.Is(err, ErrNotFound) {
			return h.invalid(c, span)
		}
		h.logError(ctx, span, "failed to get member key", err, errors.KindDB)
		return errors.InternalError()
	}
	if memberKey.Key.IsRevoked() {
		return h.invalid(c, span)
	}
	if memberKey.MemberStatus != string(orgs.StatusActive) {
		return h.invalid(c, span)
	}

	metrics.RecordKeyValidation(true)
	span.SetAttributes(
		attribute.Bool("key.valid", true),
		attribute.String("key.id", memberKey.Key.ID),
		attribute.String("member.id", memberKey.Key.MemberID),
		attribute.String("organization.id", memberKey.OrganizationID),
	)
	span.SetStatus(codes.Ok, "valid")

	return c.JSON(ValidateResponse{
		Valid:            true,
		MemberID:         memberKey.Key.MemberID,
		OrganizationID:   memberKey.OrganizationID,
		UserID:           memberKey.UserID,
		ServiceAccountID: memberKey.ServiceAccountID,
	})
}

func (h *Handler) authorizeKeyManage(ctx context.Context, orgID, memberID, userID string) (*orgs.Member, error) {
	actor, err := h.orgStore.GetActiveMemberForUser(ctx, orgID, userID)
	if err != nil {
		if stderrors.Is(err, orgs.ErrNotFound) {
			return nil, errors.NotFoundError("organization", orgID)
		}
		h.logError(ctx, nil, "failed to load actor member", err, errors.KindDB)
		return nil, errors.InternalError()
	}

	target, err := h.orgStore.GetMember(ctx, orgID, memberID)
	if err != nil {
		if stderrors.Is(err, orgs.ErrNotFound) {
			return nil, errors.NotFoundError("member", memberID)
		}
		h.logError(ctx, nil, "failed to load target member", err, errors.KindDB)
		return nil, errors.InternalError()
	}
	if target.Status == orgs.StatusRemoved {
		return nil, errors.NotFoundError("member", memberID)
	}
	if err := orgs.CanManageMemberKeys(actor, target); err != nil {
		return nil, err
	}
	return target, nil
}

func (h *Handler) invalid(c fiber.Ctx, span trace.Span) error {
	metrics.RecordKeyValidation(false)
	span.SetAttributes(attribute.Bool("key.valid", false))
	span.SetStatus(codes.Ok, "invalid")
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

func (h *Handler) logError(ctx context.Context, span trace.Span, msg string, err error, kind errors.ErrorKind) {
	httpx.LogError(ctx, span, h.telem.LoggerWithContext(ctx), msg, err, kind)
}
