package credentials

import (
	"context"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/plat5dev/plat5/identity/directory"
	"github.com/plat5dev/plat5/identity/errors"
	"github.com/plat5dev/plat5/identity/internal/apikey"
	"github.com/plat5dev/plat5/identity/internal/auditx"
	"github.com/plat5dev/plat5/identity/internal/httpx"
	"github.com/plat5dev/plat5/identity/metrics"
	"github.com/plat5dev/plat5/identity/orgs"
	"github.com/plat5dev/plat5/identity/roles"
)

type credentialStore interface {
	Create(ctx context.Context, c *Credential) error
	Lookup(ctx context.Context, hash string) (*Found, error)
	ListKeys(ctx context.Context, membershipID string, limit int, startingAfter string) ([]*Credential, bool, error)
	RevokeKey(ctx context.Context, membershipID, keyID string) (*Credential, error)
}

type orgReader interface {
	GetMember(ctx context.Context, memberID string) (*orgs.Member, error)
	GetServiceAccount(ctx context.Context, organizationID, serviceAccountID string) (*orgs.ServiceAccount, error)
	ResolveMember(ctx context.Context, userID, organizationID string) (*orgs.Member, error)
}

type people interface {
	Get(ctx context.Context, userID string) (*directory.User, error)
	Resolve(ctx context.Context, issuer, subject string) (userID, status string, err error)
}

type Handler struct {
	store  credentialStore
	orgs   orgReader
	people people
	// brand is APIKEY_BRAND: identity mints every credential with it.
	brand string
	// roles resolves a member's labels at validate and mint.
	roles *roles.Set
	now   func() time.Time
}

func NewHandler(store *Store, orgStore *orgs.Store, dir *directory.Store, brand string, roleSet *roles.Set) *Handler {
	return &Handler{store: store, orgs: orgStore, people: dir, brand: brand, roles: roleSet, now: time.Now}
}

// MountUser registers the person's own keys and session mint. Mount under /users.
func (h *Handler) MountUser(router fiber.Router) {
	router.Post("/:user_id/api-keys", h.CreateUserKey)
	router.Get("/:user_id/api-keys", h.ListUserKeys)
	router.Delete("/:user_id/api-keys/:key_id", h.RevokeUserKey)
	router.Post("/:user_id/organizations/:organization_id/session", h.CreateSession)
}

// MountMembers registers a member's keys at its self address. Mount under /members.
func (h *Handler) MountMembers(router fiber.Router) {
	router.Post("/:member_id/api-keys", h.CreateMemberKey)
	router.Get("/:member_id/api-keys", h.ListMemberKeys)
	router.Delete("/:member_id/api-keys/:key_id", h.RevokeMemberKey)
}

// MountServiceAccountKeys registers the org address for a service account's
// member keys: the same rows as its self address. Mount on the application root.
func (h *Handler) MountServiceAccountKeys(router fiber.Router) {
	router.Post("/organizations/:organization_id/service-accounts/:service_account_id/api-keys", h.CreateServiceAccountKey)
	router.Get("/organizations/:organization_id/service-accounts/:service_account_id/api-keys", h.ListServiceAccountKeys)
	router.Delete("/organizations/:organization_id/service-accounts/:service_account_id/api-keys/:key_id", h.RevokeServiceAccountKey)
}

// MountInternal registers validate on a router scoped under /internal.
func (h *Handler) MountInternal(router fiber.Router) {
	router.Post("/credentials/validate", h.Validate)
}

type CreateKeyRequest struct {
	Name string `json:"name"`
}

type CreateKeyResponse struct {
	ID        string `json:"id"`
	Key       string `json:"key"`
	KeyPrefix string `json:"key_prefix"`
	Name      string `json:"name"`
	CreatedAt string `json:"created_at"`
}

type ListKeysResponse struct {
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

// A person's directory membership has the person's id, so a user key's
// membership is the path's user_id.

func (h *Handler) CreateUserKey(c fiber.Ctx) error {
	userID := httpx.PathParam(c, "user_id")
	if err := h.requireUser(c.Context(), userID); err != nil {
		return err
	}
	return h.createKey(c, userID, metrics.KeyScopeUser)
}

func (h *Handler) ListUserKeys(c fiber.Ctx) error {
	userID := httpx.PathParam(c, "user_id")
	if err := h.requireUser(c.Context(), userID); err != nil {
		return err
	}
	return h.listKeys(c, userID)
}

func (h *Handler) RevokeUserKey(c fiber.Ctx) error {
	userID := httpx.PathParam(c, "user_id")
	if err := h.requireUser(c.Context(), userID); err != nil {
		return err
	}
	return h.revokeKey(c, userID, metrics.KeyScopeUser)
}

func (h *Handler) CreateMemberKey(c fiber.Ctx) error {
	m, err := h.visibleMember(c)
	if err != nil {
		return err
	}
	return h.createKey(c, m.ID, metrics.KeyScopeMember)
}

func (h *Handler) ListMemberKeys(c fiber.Ctx) error {
	m, err := h.visibleMember(c)
	if err != nil {
		return err
	}
	return h.listKeys(c, m.ID)
}

func (h *Handler) RevokeMemberKey(c fiber.Ctx) error {
	m, err := h.visibleMember(c)
	if err != nil {
		return err
	}
	return h.revokeKey(c, m.ID, metrics.KeyScopeMember)
}

// CreateServiceAccountKey mints a key for the service account's member. The
// key carries the service account's role, not the caller's.
func (h *Handler) CreateServiceAccountKey(c fiber.Ctx) error {
	sa, err := h.serviceAccount(c)
	if err != nil {
		return err
	}
	return h.createKey(c, sa.MemberID, metrics.KeyScopeMember)
}

func (h *Handler) ListServiceAccountKeys(c fiber.Ctx) error {
	sa, err := h.serviceAccount(c)
	if err != nil {
		return err
	}
	return h.listKeys(c, sa.MemberID)
}

func (h *Handler) RevokeServiceAccountKey(c fiber.Ctx) error {
	sa, err := h.serviceAccount(c)
	if err != nil {
		return err
	}
	return h.revokeKey(c, sa.MemberID, metrics.KeyScopeMember)
}

func (h *Handler) createKey(c fiber.Ctx, membershipID, scope string) error {
	ctx := c.Context()

	var req CreateKeyRequest
	if err := c.Bind().Body(&req); err != nil {
		return err
	}
	name, err := apikey.NormalizeName(req.Name)
	if err != nil {
		return errors.FieldError("name", "Name is too long.")
	}

	prefix := apikey.Prefix(h.brand, apikey.KindKey)
	plaintext, err := apikey.Generate(prefix)
	if err != nil {
		httpx.LogError(ctx, "failed to generate key", err, errors.KindInternal)
		return errors.InternalError()
	}

	key := NewKey(membershipID, name, plaintext, prefix)
	if err := h.store.Create(ctx, key); err != nil {
		return httpx.MapDB(ctx, err, "failed to store key", httpx.DBErr{
			NotFound: ErrNoMembership, Resource: "membership", ResourceID: membershipID,
		})
	}

	logKey(c, "api key created", membershipID, key)
	metrics.RecordKeyCreated(scope)
	// The display prefix, never the key: it is what later events show as the actor's key_prefix.
	auditx.Set(c, auditx.Details{"key_id": key.ID, "key_prefix": key.KeyPrefix, "name": name})
	auditx.SetCreated(c, key.ID)
	return c.Status(fiber.StatusCreated).JSON(CreateKeyResponse{
		ID:        key.ID,
		Key:       plaintext,
		KeyPrefix: key.KeyPrefix,
		Name:      name,
		CreatedAt: httpx.FormatTime(key.CreatedAt),
	})
}

func (h *Handler) listKeys(c fiber.Ctx, membershipID string) error {
	ctx := c.Context()
	limit, startingAfter, err := httpx.ParseListParams(c)
	if err != nil {
		return err
	}
	list, hasMore, err := h.store.ListKeys(ctx, membershipID, limit, startingAfter)
	if err != nil {
		return httpx.MapDB(ctx, err, "failed to list keys", httpx.DBErr{})
	}
	out := ListKeysResponse{Keys: make([]KeyResponse, 0, len(list)), HasMore: hasMore}
	for _, k := range list {
		out.Keys = append(out.Keys, toKeyResponse(k))
	}
	return c.JSON(out)
}

func (h *Handler) revokeKey(c fiber.Ctx, membershipID, scope string) error {
	ctx := c.Context()
	keyID := httpx.PathParam(c, "key_id")
	if keyID == "" {
		return errors.FieldError("key_id", errors.FallbackValidation)
	}
	key, err := h.store.RevokeKey(ctx, membershipID, keyID)
	if err != nil {
		return httpx.MapDB(ctx, err, "failed to revoke key", httpx.DBErr{
			NotFound: ErrNotFound, Resource: "api_key", ResourceID: keyID,
		})
	}
	logKey(c, "api key revoked", membershipID, key)
	metrics.RecordKeyRevoked(scope)
	return c.SendStatus(fiber.StatusNoContent)
}

func (h *Handler) requireUser(ctx context.Context, userID string) error {
	if _, err := h.people.Get(ctx, userID); err != nil {
		return httpx.MapDB(ctx, err, "failed to get user", httpx.DBErr{
			NotFound: directory.ErrNotFound, Resource: "user", ResourceID: userID,
		})
	}
	return nil
}

// visibleMember is the path's member. Missing or removed is not found; a
// suspended member is addressable. Validate still refuses its credentials.
func (h *Handler) visibleMember(c fiber.Ctx) (*orgs.Member, error) {
	ctx := c.Context()
	memberID := httpx.PathParam(c, "member_id")
	m, err := h.orgs.GetMember(ctx, memberID)
	if err != nil {
		return nil, httpx.MapDB(ctx, err, "failed to load member", httpx.DBErr{
			NotFound: orgs.ErrNotFound, Resource: "member", ResourceID: memberID,
		})
	}
	if m.Status == orgs.StatusRemoved {
		return nil, errors.NotFoundError("member", memberID)
	}
	return m, nil
}

// serviceAccount resolves the org address to the service account and its
// member. Missing, the wrong org, and a removed member are not found.
func (h *Handler) serviceAccount(c fiber.Ctx) (*orgs.ServiceAccount, error) {
	ctx := c.Context()
	orgID := httpx.PathParam(c, "organization_id")
	saID := httpx.PathParam(c, "service_account_id")
	sa, err := h.orgs.GetServiceAccount(ctx, orgID, saID)
	if err != nil {
		return nil, httpx.MapDB(ctx, err, "failed to load service account", httpx.DBErr{
			NotFound: orgs.ErrNotFound, Resource: "service_account", ResourceID: saID,
		})
	}
	return sa, nil
}

func logKey(c fiber.Ctx, msg, membershipID string, key *Credential) {
	ev := httpx.Logger(c.Context()).Info().
		Str("membership_id", membershipID).
		Str("key_id", key.ID).
		Str("key_prefix", key.KeyPrefix)
	if orgID := httpx.PathParam(c, "organization_id"); orgID != "" {
		ev = ev.Str("organization_id", orgID)
	}
	if saID := httpx.PathParam(c, "service_account_id"); saID != "" {
		ev = ev.Str("service_account_id", saID)
	}
	ev.Msg(msg)
}

func toKeyResponse(k *Credential) KeyResponse {
	var name string
	if k.Name != nil {
		name = *k.Name
	}
	return KeyResponse{
		ID:        k.ID,
		KeyPrefix: k.KeyPrefix,
		Name:      name,
		CreatedAt: httpx.FormatTime(k.CreatedAt),
		RevokedAt: httpx.FormatTimePtr(k.RevokedAt),
	}
}
