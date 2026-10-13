package credentials

import (
	stderrors "errors"
	"strings"

	"github.com/gofiber/fiber/v3"

	"github.com/plat5dev/plat5/identity/directory"
	"github.com/plat5dev/plat5/identity/errors"
	"github.com/plat5dev/plat5/identity/internal/apikey"
	"github.com/plat5dev/plat5/identity/internal/httpx"
	"github.com/plat5dev/plat5/identity/metrics"
	"github.com/plat5dev/plat5/identity/orgs"
)

const (
	RealmDirectory    = "directory"
	RealmOrganization = "organization"
	// KindIdP is a verified JWT's subject, resolved to its person.
	KindIdP = "idp"
)

// ValidateRequest is one of: an X-API-Key value, or a verified JWT's issuer
// and subject. The gateway verified the JWT; identity only resolves it.
type ValidateRequest struct {
	Credential *string     `json:"credential"`
	IdP        *IdPSubject `json:"idp"`
}

type IdPSubject struct {
	Issuer  string `json:"issuer"`
	Subject string `json:"subject"`
}

// ValidateResponse is what the gateway admits on. A directory result names the
// person. An organization result names the member and its labels, never the
// person. The role does not leave identity.
type ValidateResponse struct {
	Valid          bool   `json:"valid"`
	Realm          string `json:"realm,omitempty"`
	Kind           string `json:"kind,omitempty"`
	UserID         string `json:"user_id,omitempty"`
	MemberID       string `json:"member_id,omitempty"`
	OrganizationID string `json:"organization_id,omitempty"`
	// Labels is a pointer so an organization result's [] is sent, not dropped.
	Labels *[]string `json:"labels,omitempty"`
}

func (h *Handler) Validate(c fiber.Ctx) error {
	var req ValidateRequest
	if err := c.Bind().Body(&req); err != nil {
		return err
	}
	switch {
	case req.Credential != nil && req.IdP == nil:
		return h.validateCredential(c, strings.TrimSpace(*req.Credential))
	case req.IdP != nil && req.Credential == nil:
		return h.validateIdP(c, req.IdP)
	default:
		return errors.FieldError("body", "Send one of credential or idp.")
	}
}

func (h *Handler) validateIdP(c fiber.Ctx, p *IdPSubject) error {
	ctx := c.Context()
	issuer := strings.TrimSpace(p.Issuer)
	subject := strings.TrimSpace(p.Subject)
	if issuer == "" || subject == "" || len(issuer) > directory.MaxIssuerLen || len(subject) > directory.MaxSubjectLen {
		return c.JSON(ValidateResponse{Valid: false})
	}
	userID, status, err := h.people.Resolve(ctx, issuer, subject)
	if err != nil {
		return httpx.MapDB(ctx, err, "failed to resolve idp subject", httpx.DBErr{})
	}
	if status != directory.StatusActive {
		return c.JSON(ValidateResponse{Valid: false})
	}
	return c.JSON(ValidateResponse{Valid: true, Realm: RealmDirectory, Kind: KindIdP, UserID: userID})
}

func (h *Handler) validateCredential(c fiber.Ctx, value string) error {
	ctx := c.Context()
	kind, _, ok := apikey.Parse(h.brand, value)
	if !ok {
		return h.invalid(c, "")
	}
	found, err := h.store.Lookup(ctx, apikey.Hash(value))
	if err != nil {
		if stderrors.Is(err, ErrNotFound) {
			return h.invalid(c, kind)
		}
		return httpx.MapDB(ctx, err, "failed to look up credential", httpx.DBErr{})
	}
	cred := found.Credential
	if cred.Kind != kind || !cred.Usable(h.now()) || found.PersonStatus != directory.StatusActive {
		return h.invalid(c, kind)
	}

	if found.OrganizationID == nil {
		metrics.RecordKeyValidation(metrics.KeyScopeUser, true)
		return c.JSON(ValidateResponse{
			Valid: true, Realm: RealmDirectory, Kind: string(cred.Kind), UserID: found.IdentityID,
		})
	}
	if found.MemberStatus == nil || *found.MemberStatus != string(orgs.StatusActive) {
		return h.invalid(c, kind)
	}
	recordValid(cred.Kind)
	// The member's role labels: ["*"] is every label, [] is none. Never null.
	labels := h.roles.Grants(found.MemberRole)
	if labels == nil {
		labels = []string{}
	}
	return c.JSON(ValidateResponse{
		Valid:          true,
		Realm:          RealmOrganization,
		Kind:           string(cred.Kind),
		MemberID:       cred.MembershipID,
		OrganizationID: *found.OrganizationID,
		Labels:         &labels,
	})
}

func recordValid(kind apikey.Kind) {
	if kind == apikey.KindSession {
		metrics.RecordSessionValidation(true)
		return
	}
	metrics.RecordKeyValidation(metrics.KeyScopeMember, true)
}

// invalid answers valid: false. A rejected key is counted under the member
// scope: whose it was is not known.
func (h *Handler) invalid(c fiber.Ctx, kind apikey.Kind) error {
	if kind == apikey.KindSession {
		metrics.RecordSessionValidation(false)
	} else {
		metrics.RecordKeyValidation(metrics.KeyScopeMember, false)
	}
	return c.JSON(ValidateResponse{Valid: false})
}
