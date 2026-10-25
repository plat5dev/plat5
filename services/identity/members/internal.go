package members

import (
	"github.com/gofiber/fiber/v3"

	"github.com/plat5dev/plat5/identity/internal/httpx"
)

// MountInternal serves admit on the internal port
// (docs/reference/api/internal.md#admit).
func (h *Handler) MountInternal(r fiber.Router) {
	r.Post("/admit", h.Admit)
}

type AdmitRequest struct {
	Credential *string `json:"credential"`
	IdP        *struct {
		Issuer  string  `json:"issuer"`
		Subject string  `json:"subject"`
		Name    *string `json:"name"`
	} `json:"idp"`
	Chain []struct {
		Kind string `json:"kind"`
		Ref  string `json:"ref"`
	} `json:"chain"`
	AllowDeleted bool `json:"allow_deleted"`
}

type AdmitNode struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

// AdmitResponse has every field in every answer: the gateway reads a missing
// one as identity failing.
type AdmitResponse struct {
	Valid       bool        `json:"valid"`
	Principal   *string     `json:"principal"`
	KeyID       *string     `json:"key_id"`
	Nodes       []AdmitNode `json:"nodes"`
	MemberID    *string     `json:"member_id"`
	UserID      *string     `json:"user_id"`
	Permissions []string    `json:"permissions"`
}

func (h *Handler) Admit(c fiber.Ctx) error {
	var req AdmitRequest
	if err := c.Bind().Body(&req); err != nil {
		return err
	}
	in := AdmitInput{Credential: req.Credential, AllowDeleted: req.AllowDeleted}
	if req.IdP != nil {
		in.IdP = &IdPClaims{Issuer: req.IdP.Issuer, Subject: req.IdP.Subject, Name: req.IdP.Name}
	}
	for _, ref := range req.Chain {
		in.Chain = append(in.Chain, ChainRef{Kind: ref.Kind, Ref: ref.Ref})
	}
	a, err := h.store.Admit(c.Context(), h.src.Current(), in)
	if err != nil {
		return httpx.MapDB(c.Context(), err, "failed to admit", httpx.DBErr{})
	}
	return c.JSON(admitResponse(a))
}

func admitResponse(a *Admitted) AdmitResponse {
	out := AdmitResponse{
		Valid:       a.Valid,
		Principal:   nullable(a.Principal),
		KeyID:       nullable(a.KeyID),
		Nodes:       make([]AdmitNode, 0, len(a.Nodes)),
		MemberID:    nullable(a.MemberID),
		UserID:      nullable(a.UserID),
		Permissions: a.Permissions,
	}
	if out.Permissions == nil {
		out.Permissions = []string{}
	}
	for _, n := range a.Nodes {
		out.Nodes = append(out.Nodes, AdmitNode{Kind: n.Kind, ID: n.ID})
	}
	return out
}

// nullable is s, or nil for "".
func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
