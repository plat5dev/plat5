package sessions

import (
	stderrors "errors"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/plat5dev/plat5/identity/errors"
	"github.com/plat5dev/plat5/identity/internal/apikey"
	"github.com/plat5dev/plat5/identity/internal/httpx"
	"github.com/plat5dev/plat5/identity/metrics"
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

type CreateResponse struct {
	Token          string `json:"token"`
	ExpiresAt      string `json:"expires_at"`
	MemberID       string `json:"member_id"`
	OrganizationID string `json:"organization_id"`
}

type ValidateRequest struct {
	Token string `json:"token"`
}

// validPayload is the validate hit. scopes is null. No user_id.
func validPayload(memberID, organizationID string) fiber.Map {
	return fiber.Map{
		"valid":           true,
		"member_id":       memberID,
		"organization_id": organizationID,
		"scopes":          nil,
	}
}

func (h *Handler) Create(c fiber.Ctx) error {
	ctx := c.Context()
	userID := strings.TrimSpace(httpx.PathParam(c, "user_id"))
	orgID := strings.TrimSpace(httpx.PathParam(c, "organization_id"))
	if userID == "" {
		return errors.FieldError("user_id", errors.FallbackValidation)
	}
	if len(userID) > orgs.MaxUserIDLen {
		return errors.FieldError("user_id", "That user ID is too long.")
	}
	if orgID == "" {
		return errors.FieldError("organization_id", errors.FallbackValidation)
	}

	member, err := h.orgStore.ResolveMember(ctx, userID, orgID)
	if err != nil {
		if stderrors.Is(err, orgs.ErrNotFound) {
			return errors.NotFoundError("member", userID+":"+orgID)
		}
		return httpx.MapDB(ctx, err, "failed to resolve member for session", httpx.DBErr{})
	}
	if member.Status != orgs.StatusActive {
		return errors.NotFoundError("member", userID+":"+orgID)
	}

	plaintext, err := apikey.Generate(h.prefix)
	if err != nil {
		httpx.LogError(ctx, "failed to generate session token", err, errors.KindInternal)
		return errors.InternalError()
	}

	now := time.Now().UTC()
	session := New(member.ID, plaintext, h.prefix, now)
	if err := h.store.Create(ctx, session); err != nil {
		return httpx.MapDB(ctx, err, "failed to store member session", httpx.DBErr{})
	}

	httpx.Logger(ctx).Info().
		Str("user_id", userID).
		Str("organization_id", orgID).
		Str("member_id", member.ID).
		Str("session_id", session.ID).
		Msg("member session created")

	metrics.RecordSessionCreated()
	return c.Status(fiber.StatusCreated).JSON(CreateResponse{
		Token:          plaintext,
		ExpiresAt:      httpx.FormatTime(session.ExpiresAt),
		MemberID:       member.ID,
		OrganizationID: member.OrganizationID,
	})
}

func (h *Handler) Validate(c fiber.Ctx) error {
	ctx := c.Context()

	var req ValidateRequest
	if err := c.Bind().Body(&req); err != nil {
		return err
	}
	token := strings.TrimSpace(req.Token)
	if token == "" {
		return errors.FieldError("token", errors.FallbackValidation)
	}
	if !apikey.LooksLike(token, h.prefix) {
		return h.invalid(c)
	}

	found, err := h.store.GetByHash(ctx, HashToken(token))
	if err != nil {
		if stderrors.Is(err, ErrNotFound) {
			return h.invalid(c)
		}
		return httpx.MapDB(ctx, err, "failed to get member session", httpx.DBErr{})
	}
	if found.Session.Expired(time.Now()) || found.MemberStatus != string(orgs.StatusActive) {
		return h.invalid(c)
	}

	metrics.RecordSessionValidation(true)
	return c.JSON(validPayload(found.Session.MemberID, found.OrganizationID))
}

func (h *Handler) invalid(c fiber.Ctx) error {
	metrics.RecordSessionValidation(false)
	return c.JSON(fiber.Map{"valid": false})
}
