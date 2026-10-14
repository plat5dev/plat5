package orgs

import (
	"context"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/plat5dev/plat5/identity/directory"
	"github.com/plat5dev/plat5/identity/errors"
	"github.com/plat5dev/plat5/identity/internal/auditx"
	"github.com/plat5dev/plat5/identity/internal/httpx"
	"github.com/plat5dev/plat5/identity/metrics"
)

// CreateMemberRequest names the person by IdP subject. Their user_id stays
// internal: an admin never needs to know it.
type CreateMemberRequest struct {
	IdP  *IdPSubject `json:"idp"`
	Role *string     `json:"role"`
}

type IdPSubject struct {
	Issuer  string `json:"issuer"`
	Subject string `json:"subject"`
}

// UpdateMemberRequest is the self address. It does not change role.
type UpdateMemberRequest struct {
	Status      *string        `json:"status"`
	DisplayName httpx.Nullable `json:"display_name"`
}

// UpdateOrgMemberRequest is the org address: the org acting on one of its members.
type UpdateOrgMemberRequest struct {
	Status      *string        `json:"status"`
	Role        *string        `json:"role"`
	DisplayName httpx.Nullable `json:"display_name"`
}

// MemberResponse has no user_id: an org sees its members, not the directory.
type MemberResponse struct {
	ID               string  `json:"id"`
	OrganizationID   string  `json:"organization_id"`
	Principal        string  `json:"principal"`
	ServiceAccountID *string `json:"service_account_id"`
	DisplayName      *string `json:"display_name"`
	Role             *string `json:"role"`
	Status           string  `json:"status"`
	CreatedAt        string  `json:"created_at"`
	UpdatedAt        string  `json:"updated_at"`
}

// FormerMemberResponse is what is left of a removed member: enough to show its name.
type FormerMemberResponse struct {
	ID             string  `json:"id"`
	OrganizationID string  `json:"organization_id"`
	Principal      string  `json:"principal"`
	DisplayName    *string `json:"display_name"`
	Status         string  `json:"status"`
}

type ListMembersResponse struct {
	Members []MemberResponse `json:"members"`
	HasMore bool             `json:"has_more"`
}

func (h *Handler) ListMembers(c fiber.Ctx) error {
	ctx := c.Context()
	orgID := httpx.PathParam(c, "organization_id")

	if err := h.requireOrganization(ctx, orgID); err != nil {
		return err
	}

	limit, startingAfter, err := httpx.ParseListParams(c)
	if err != nil {
		return err
	}

	list, hasMore, err := h.store.ListMembers(ctx, orgID, limit, startingAfter)
	if err != nil {
		return httpx.MapDB(ctx, err, "failed to list members", httpx.DBErr{})
	}

	out := ListMembersResponse{
		Members: make([]MemberResponse, 0, len(list)),
		HasMore: hasMore,
	}
	for _, m := range list {
		out.Members = append(out.Members, h.toMemberResponse(m))
	}
	return c.JSON(out)
}

func (h *Handler) CreateMember(c fiber.Ctx) error {
	ctx := c.Context()
	orgID := httpx.PathParam(c, "organization_id")

	if err := h.requireOrganization(ctx, orgID); err != nil {
		return err
	}

	var req CreateMemberRequest
	if err := c.Bind().Body(&req); err != nil {
		return err
	}

	issuer, subject, err := parseIdPSubject(req.IdP)
	if err != nil {
		return err
	}
	role, err := h.roles.Set().Choose(req.Role)
	if err != nil {
		return err
	}
	targetUser, _, err := h.people.Resolve(ctx, issuer, subject)
	if err != nil {
		return httpx.MapDB(ctx, err, "failed to resolve person", httpx.DBErr{})
	}

	now := time.Now().UTC()
	m := &Member{
		ID:             NewULID(),
		OrganizationID: orgID,
		UserID:         &targetUser,
		Role:           role,
		Status:         StatusActive,
		CreatedAt:      now,
		UpdatedAt:      now,
	}

	if err := h.store.CreateUserMember(ctx, m); err != nil {
		return httpx.MapDB(ctx, err, "failed to create member", httpx.DBErr{
			NotFound: ErrNotFound, Resource: "organization", ResourceID: orgID,
			Conflict: ErrConflict, Field: "idp", FieldValue: subject,
			Message: "This person is already a member.",
		})
	}
	// Read back for the display name the person already has.
	created, err := h.store.GetMember(ctx, m.ID)
	if err != nil {
		return httpx.MapDB(ctx, err, "failed to read member", httpx.DBErr{})
	}

	metrics.RecordMemberOp("create")
	auditx.Set(c, auditx.Details{"member_id": m.ID, "role": m.Role})
	auditx.SetCreated(c, m.ID)
	return c.Status(fiber.StatusCreated).JSON(h.toMemberResponse(created))
}

func (h *Handler) GetMember(c fiber.Ctx) error {
	ctx := c.Context()
	memberID := httpx.PathParam(c, "member_id")

	m, err := h.visibleMember(ctx, memberID)
	if err != nil {
		return err
	}
	return c.JSON(h.toMemberResponse(m))
}

// UpdateMember is the self address: status and the display override.
func (h *Handler) UpdateMember(c fiber.Ctx) error {
	ctx := c.Context()
	memberID := httpx.PathParam(c, "member_id")

	var req UpdateMemberRequest
	if err := c.Bind().Body(&req); err != nil {
		return err
	}
	if req.Status == nil && !req.DisplayName.Set {
		return errors.FieldError("body", "Nothing to update.")
	}
	var status Status
	if req.Status != nil {
		parsed, err := ParsePatchStatus(*req.Status)
		if err != nil {
			return err
		}
		status = parsed
	}
	name, err := parseDisplayOverride(req.DisplayName)
	if err != nil {
		return err
	}

	var prior Member
	m, err := h.store.MutateMember(ctx, "", memberID, func(target *Member, _ []*Member) error {
		prior = *target
		if status != "" {
			target.Status = status
		}
		name.apply(target)
		return nil
	})
	if err != nil {
		return httpx.MapDB(ctx, err, "failed to update member", httpx.DBErr{
			NotFound: ErrNotFound, Resource: "member", ResourceID: memberID,
		})
	}

	metrics.RecordMemberOp("update")
	details := auditx.Details{}
	details.Changed("status", string(prior.Status), string(m.Status))
	details.ChangedPtr("display_name", prior.DisplayOverride, m.DisplayOverride)
	auditx.Set(c, details)
	return h.respondMember(c, m)
}

// DeleteMember is the self address: a member leaves.
func (h *Handler) DeleteMember(c fiber.Ctx) error {
	return h.removeMember(c, "")
}

// GetOrgMember is the org address. A member in another org is not found. A
// removed member answers as a former member: its contributions can outlive it.
func (h *Handler) GetOrgMember(c fiber.Ctx) error {
	ctx := c.Context()
	orgID := httpx.PathParam(c, "organization_id")
	memberID := httpx.PathParam(c, "member_id")

	m, err := h.store.GetMember(ctx, memberID)
	if err != nil {
		return httpx.MapDB(ctx, err, "failed to get member", httpx.DBErr{
			NotFound: ErrNotFound, Resource: "member", ResourceID: memberID,
		})
	}
	if m.OrganizationID != orgID {
		return errors.NotFoundError("member", memberID)
	}
	if m.Status == StatusRemoved {
		return c.JSON(FormerMemberResponse{
			ID:             m.ID,
			OrganizationID: m.OrganizationID,
			Principal:      m.Principal(),
			DisplayName:    m.DisplayName,
			Status:         string(m.Status),
		})
	}
	return c.JSON(h.toMemberResponse(m))
}

// UpdateOrgMember is the org acting on one of its members: status and role.
func (h *Handler) UpdateOrgMember(c fiber.Ctx) error {
	ctx := c.Context()
	orgID := httpx.PathParam(c, "organization_id")
	memberID := httpx.PathParam(c, "member_id")

	var req UpdateOrgMemberRequest
	if err := c.Bind().Body(&req); err != nil {
		return err
	}
	if req.Status == nil && req.Role == nil && !req.DisplayName.Set {
		return errors.FieldError("body", "Nothing to update.")
	}
	var status Status
	if req.Status != nil {
		parsed, err := ParsePatchStatus(*req.Status)
		if err != nil {
			return err
		}
		status = parsed
	}
	// One document for the whole request: the role chosen and the last-creator
	// rule read the same roles.
	set := h.roles.Set()
	var role *string
	if req.Role != nil {
		chosen, err := set.Choose(req.Role)
		if err != nil {
			return err
		}
		role = chosen
	}
	name, err := parseDisplayOverride(req.DisplayName)
	if err != nil {
		return err
	}
	var prior Member
	m, err := h.store.MutateMember(ctx, orgID, memberID, func(target *Member, members []*Member) error {
		prior = *target
		if status != "" {
			target.Status = status
		}
		if role != nil {
			target.Role = role
		}
		name.apply(target)
		return rejectLastCreator(set, members, prior, "role")
	})
	if err != nil {
		return httpx.MapDB(ctx, err, "failed to update member", httpx.DBErr{
			NotFound: ErrNotFound, Resource: "member", ResourceID: memberID,
		})
	}

	metrics.RecordMemberOp("update")
	details := auditx.Details{}
	details.ChangedPtr("role", prior.Role, m.Role)
	details.Changed("status", string(prior.Status), string(m.Status))
	details.ChangedPtr("display_name", prior.DisplayOverride, m.DisplayOverride)
	auditx.Set(c, details)
	return h.respondMember(c, m)
}

// DeleteOrgMember is the org removing one of its members.
func (h *Handler) DeleteOrgMember(c fiber.Ctx) error {
	return h.removeMember(c, httpx.PathParam(c, "organization_id"))
}

// removeMember removes at either address. orgID "" is the self address.
// Both keep the last member and the last creator_role holder.
func (h *Handler) removeMember(c fiber.Ctx, orgID string) error {
	ctx := c.Context()
	memberID := httpx.PathParam(c, "member_id")

	_, err := h.store.MutateMember(ctx, orgID, memberID, func(target *Member, members []*Member) error {
		if err := rejectLastMember(countNonRemoved(members), "member_id"); err != nil {
			return err
		}
		prior := *target
		target.Status = StatusRemoved
		return rejectLastCreator(h.roles.Set(), members, prior, "member_id")
	})
	if err != nil {
		return httpx.MapDB(ctx, err, "failed to remove member", httpx.DBErr{
			NotFound: ErrNotFound, Resource: "member", ResourceID: memberID,
		})
	}

	metrics.RecordMemberOp("remove")
	return c.SendStatus(fiber.StatusNoContent)
}

func (h *Handler) visibleMember(ctx context.Context, memberID string) (*Member, error) {
	m, err := h.store.GetMember(ctx, memberID)
	if err != nil {
		return nil, httpx.MapDB(ctx, err, "failed to get member", httpx.DBErr{
			NotFound: ErrNotFound, Resource: "member", ResourceID: memberID,
		})
	}
	if m.Status == StatusRemoved {
		return nil, errors.NotFoundError("member", memberID)
	}
	return m, nil
}

// respondMember answers a write with the member as it now reads, so the
// display name reflects a changed or cleared override.
func (h *Handler) respondMember(c fiber.Ctx, m *Member) error {
	ctx := c.Context()
	fresh, err := h.store.GetMember(ctx, m.ID)
	if err != nil {
		return httpx.MapDB(ctx, err, "failed to read member", httpx.DBErr{
			NotFound: ErrNotFound, Resource: "member", ResourceID: m.ID,
		})
	}
	return c.JSON(h.toMemberResponse(fresh))
}

// displayOverride is a parsed display_name patch.
type displayOverride struct {
	set   bool
	value *string
}

func (d displayOverride) apply(m *Member) {
	if d.set {
		m.DisplayOverride = d.value
	}
}

func parseDisplayOverride(n httpx.Nullable) (displayOverride, error) {
	if !n.Set {
		return displayOverride{}, nil
	}
	name, _, err := directory.ParseDisplayName(n, "display_name")
	if err != nil {
		return displayOverride{}, err
	}
	return displayOverride{set: true, value: name}, nil
}

func parseIdPSubject(p *IdPSubject) (issuer, subject string, err error) {
	if p == nil {
		return "", "", errors.FieldError("idp", "Choose someone to add.")
	}
	issuer = strings.TrimSpace(p.Issuer)
	subject = strings.TrimSpace(p.Subject)
	if issuer == "" {
		return "", "", errors.FieldError("idp.issuer", "Issuer is required.")
	}
	if subject == "" {
		return "", "", errors.FieldError("idp.subject", "Subject is required.")
	}
	if len(issuer) > directory.MaxIssuerLen {
		return "", "", errors.FieldError("idp.issuer", "Issuer is too long.")
	}
	if len(subject) > directory.MaxSubjectLen {
		return "", "", errors.FieldError("idp.subject", "Subject is too long.")
	}
	return issuer, subject, nil
}

func (h *Handler) toMemberResponse(m *Member) MemberResponse {
	return MemberResponse{
		ID:               m.ID,
		OrganizationID:   m.OrganizationID,
		Principal:        m.Principal(),
		ServiceAccountID: m.ServiceAccountID,
		DisplayName:      m.DisplayName,
		Role:             m.Role,
		Status:           string(m.Status),
		CreatedAt:        httpx.FormatTime(m.CreatedAt),
		UpdatedAt:        httpx.FormatTime(m.UpdatedAt),
	}
}

// GetMemberUser names the person a member is. Not in the customer catalog: an
// org sees its members, not the directory. Staff use it to reach the person.
func (h *Handler) GetMemberUser(c fiber.Ctx) error {
	ctx := c.Context()
	memberID := httpx.PathParam(c, "member_id")
	m, err := h.store.GetMember(ctx, memberID)
	if err != nil {
		return httpx.MapDB(ctx, err, "failed to get member", httpx.DBErr{
			NotFound: ErrNotFound, Resource: "member", ResourceID: memberID,
		})
	}
	if m.UserID == nil {
		return errors.NotFoundError("user", nil)
	}
	return c.JSON(fiber.Map{"user_id": *m.UserID})
}
