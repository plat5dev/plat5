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
	Scope string `json:"scope"`
	Chain []struct {
		Kind string `json:"kind"`
		Ref  string `json:"ref"`
	} `json:"chain"`
	ResolveDeleted bool `json:"resolve_deleted"`
}

type AdmitNode struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

// AdmitResponse has every field in every answer: the gateway reads a missing
// one as identity failing.
type AdmitResponse struct {
	Valid     bool        `json:"valid"`
	Found     bool        `json:"found"`
	Principal *string     `json:"principal"`
	Nodes     []AdmitNode `json:"nodes"`
	MemberID  *string     `json:"member_id"`
	Labels    []string    `json:"labels"`
	GoodUntil *string     `json:"good_until"`
}

func (h *Handler) Admit(c fiber.Ctx) error {
	var req AdmitRequest
	if err := c.Bind().Body(&req); err != nil {
		return err
	}
	in := AdmitInput{Credential: req.Credential, Scope: req.Scope, ResolveDeleted: req.ResolveDeleted}
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
		Valid: a.Valid, Found: a.Found,
		Nodes: make([]AdmitNode, 0, len(a.Nodes)), Labels: a.Labels,
		GoodUntil: httpx.FormatTimePtr(a.GoodUntil),
	}
	if out.Labels == nil {
		out.Labels = []string{}
	}
	if a.Valid {
		out.Principal = &a.Principal
	}
	if a.MemberID != "" {
		out.MemberID = &a.MemberID
	}
	for _, n := range a.Nodes {
		out.Nodes = append(out.Nodes, AdmitNode{Kind: n.Kind, ID: n.ID})
	}
	return out
}
