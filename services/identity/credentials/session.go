package credentials

import (
	stderrors "errors"
	"strings"

	"github.com/gofiber/fiber/v3"

	"github.com/plat5dev/plat5/identity/errors"
	"github.com/plat5dev/plat5/identity/internal/apikey"
	"github.com/plat5dev/plat5/identity/internal/auditx"
	"github.com/plat5dev/plat5/identity/internal/httpx"
	"github.com/plat5dev/plat5/identity/metrics"
	"github.com/plat5dev/plat5/identity/orgs"
)

type CreateSessionResponse struct {
	Token          string `json:"token"`
	ExpiresAt      string `json:"expires_at"`
	MemberID       string `json:"member_id"`
	OrganizationID string `json:"organization_id"`
	// Role is null when roles are off.
	Role *string `json:"role"`
	// Labels is the member's role labels at mint: what validate would return now.
	Labels []string `json:"labels"`
}

// CreateSession mints a session for the person's active member in the org.
// The session carries the member's role labels; validate resolves them every
// time, so the role is not stored on it.
func (h *Handler) CreateSession(c fiber.Ctx) error {
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

	member, err := h.orgs.ResolveMember(ctx, userID, orgID)
	if err != nil {
		if stderrors.Is(err, orgs.ErrNotFound) {
			return errors.NotFoundError("member", userID+":"+orgID)
		}
		return httpx.MapDB(ctx, err, "failed to resolve member for session", httpx.DBErr{})
	}
	if member.Status != orgs.StatusActive {
		return errors.NotFoundError("member", userID+":"+orgID)
	}

	prefix := apikey.Prefix(h.brand, apikey.KindSession)
	plaintext, err := apikey.Generate(prefix)
	if err != nil {
		httpx.LogError(ctx, "failed to generate session token", err, errors.KindInternal)
		return errors.InternalError()
	}

	session := NewSession(member.ID, plaintext, prefix, h.now())
	if err := h.store.Create(ctx, session); err != nil {
		return httpx.MapDB(ctx, err, "failed to store session", httpx.DBErr{})
	}

	httpx.Logger(ctx).Info().
		Str("user_id", userID).
		Str("organization_id", orgID).
		Str("member_id", member.ID).
		Str("session_id", session.ID).
		Msg("member session created")

	metrics.RecordSessionCreated()
	auditx.SetOrganization(c, member.OrganizationID, member.ID)
	return c.Status(fiber.StatusCreated).JSON(CreateSessionResponse{
		Token:          plaintext,
		ExpiresAt:      httpx.FormatTime(*session.ExpiresAt),
		MemberID:       member.ID,
		OrganizationID: member.OrganizationID,
		Role:           member.Role,
		Labels:         h.roles.Grants(member.Role),
	})
}
