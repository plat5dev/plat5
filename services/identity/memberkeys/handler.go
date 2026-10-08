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
	"github.com/plat5dev/plat5/identity/orgs"
	"github.com/plat5dev/plat5/identity/roles"
)

type keyStore interface {
	Create(ctx context.Context, key *APIKey) error
	GetByHash(ctx context.Context, keyHash string) (*Validated, error)
	List(ctx context.Context, memberID string, limit int, startingAfter string) ([]*APIKey, bool, error)
	Revoke(ctx context.Context, memberID, keyID string) (*APIKey, error)
}

type orgReader interface {
	GetMember(ctx context.Context, memberID string) (*orgs.Member, error)
	GetServiceAccount(ctx context.Context, organizationID, serviceAccountID string) (*orgs.ServiceAccount, error)
}

type Handler struct {
	store    keyStore
	orgStore orgReader
	prefix   string
	// roles resolves the member's labels at validate.
	roles *roles.Set
}

func NewHandler(store *Store, orgStore *orgs.Store, prefix string, roleSet *roles.Set) *Handler {
	return &Handler{store: store, orgStore: orgStore, prefix: prefix, roles: roleSet}
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
	Valid          bool   `json:"valid"`
	MemberID       string `json:"member_id,omitempty"`
	OrganizationID string `json:"organization_id,omitempty"`
}

func (h *Handler) Create(c fiber.Ctx) error {
	ctx := c.Context()
	memberID := httpx.PathParam(c, "member_id")
	if _, err := h.visibleMember(ctx, memberID); err != nil {
		return err
	}
	return h.create(c, memberID)
}

// CreateForServiceAccount mints a key for the service account's member. The key
// carries the service account's role, not the caller's.
func (h *Handler) CreateForServiceAccount(c fiber.Ctx) error {
	sa, err := h.serviceAccount(c)
	if err != nil {
		return err
	}
	return h.create(c, sa.MemberID)
}

func (h *Handler) create(c fiber.Ctx, memberID string) error {
	ctx := c.Context()

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

	apiKey := New(memberID, name, plaintext, h.prefix)
	if err := h.store.Create(ctx, apiKey); err != nil {
		return httpx.MapDB(ctx, err, "failed to store member key", httpx.DBErr{})
	}

	logKeyEvent(c, "member api key created", memberID, apiKey.ID, apiKey.KeyPrefix)
	metrics.RecordKeyCreated(metrics.KeyScopeMember)
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
	memberID := httpx.PathParam(c, "member_id")
	if _, err := h.visibleMember(ctx, memberID); err != nil {
		return err
	}
	return h.list(c, memberID)
}

func (h *Handler) ListForServiceAccount(c fiber.Ctx) error {
	sa, err := h.serviceAccount(c)
	if err != nil {
		return err
	}
	return h.list(c, sa.MemberID)
}

func (h *Handler) list(c fiber.Ctx, memberID string) error {
	ctx := c.Context()

	limit, startingAfter, err := httpx.ParseListParams(c)
	if err != nil {
		return err
	}

	list, hasMore, err := h.store.List(ctx, memberID, limit, startingAfter)
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
	memberID := httpx.PathParam(c, "member_id")
	if _, err := h.visibleMember(ctx, memberID); err != nil {
		return err
	}
	return h.revoke(c, memberID, httpx.PathParam(c, "key_id"))
}

func (h *Handler) RevokeForServiceAccount(c fiber.Ctx) error {
	sa, err := h.serviceAccount(c)
	if err != nil {
		return err
	}
	return h.revoke(c, sa.MemberID, httpx.PathParam(c, "key_id"))
}

func (h *Handler) revoke(c fiber.Ctx, memberID, keyID string) error {
	ctx := c.Context()
	if keyID == "" {
		return errors.FieldError("key_id", errors.FallbackValidation)
	}

	key, err := h.store.Revoke(ctx, memberID, keyID)
	if err != nil {
		return httpx.MapDB(ctx, err, "failed to revoke member key", httpx.DBErr{
			NotFound: ErrNotFound, Resource: "api_key", ResourceID: keyID,
		})
	}

	logKeyEvent(c, "member api key revoked", memberID, key.ID, "")
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
	return c.JSON(fiber.Map{
		"valid":           true,
		"member_id":       memberKey.Key.MemberID,
		"organization_id": memberKey.OrganizationID,
		// The member's role labels. The role itself does not leave identity.
		"scopes": apikey.WireScopesJSON(h.roles.Grants(memberKey.MemberRole)),
	})
}

func (h *Handler) visibleMember(ctx context.Context, memberID string) (*orgs.Member, error) {
	target, err := h.orgStore.GetMember(ctx, memberID)
	if err != nil {
		return nil, httpx.MapDB(ctx, err, "failed to load member", httpx.DBErr{
			NotFound: orgs.ErrNotFound, Resource: "member", ResourceID: memberID,
		})
	}
	if target.Status == orgs.StatusRemoved {
		return nil, errors.NotFoundError("member", memberID)
	}
	return target, nil
}

// serviceAccount resolves the org address to the service account and its member.
// GetServiceAccount returns not found for a missing id, the wrong org, and a removed member.
// Suspended remains addressable.
func (h *Handler) serviceAccount(c fiber.Ctx) (*orgs.ServiceAccount, error) {
	ctx := c.Context()
	orgID := httpx.PathParam(c, "organization_id")
	saID := httpx.PathParam(c, "service_account_id")
	sa, err := h.orgStore.GetServiceAccount(ctx, orgID, saID)
	if err != nil {
		return nil, httpx.MapDB(ctx, err, "failed to load service account", httpx.DBErr{
			NotFound: orgs.ErrNotFound, Resource: "service_account", ResourceID: saID,
		})
	}
	return sa, nil
}

func (h *Handler) invalid(c fiber.Ctx) error {
	metrics.RecordKeyValidation(metrics.KeyScopeMember, false)
	return c.JSON(ValidateResponse{Valid: false})
}

func logKeyEvent(c fiber.Ctx, msg, memberID, keyID, keyPrefix string) {
	ev := httpx.Logger(c.Context()).Info().
		Str("member_id", memberID).
		Str("key_id", keyID)
	if keyPrefix != "" {
		ev = ev.Str("key_prefix", keyPrefix)
	}
	if orgID := httpx.PathParam(c, "organization_id"); orgID != "" {
		ev = ev.Str("organization_id", orgID)
	}
	if saID := httpx.PathParam(c, "service_account_id"); saID != "" {
		ev = ev.Str("service_account_id", saID)
	}
	ev.Msg(msg)
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
