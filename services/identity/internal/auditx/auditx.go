// Package auditx says what an identity write changed, for the audit log
// (docs/concepts/audit.md#details). The gateway reads the headers on audited
// routes, stores them as sent, and strips them. Identity does not know whether
// a route is audited or who called; it only reports the change.
package auditx

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/gofiber/fiber/v3"

	"github.com/plat5dev/plat5/identity/internal/httpx"
)

const (
	Header = "X-Plat5-Audit-Details"
	// CreatedHeader is the id a create produced.
	CreatedHeader = "X-Plat5-Audit-Created"
	// NodesHeader and MemberHeader name the chain a user route changed, root
	// first, and the person's member in that tree. The event goes in those
	// nodes' logs too.
	NodesHeader  = "X-Plat5-Audit-Nodes"
	MemberHeader = "X-Plat5-Audit-Member"
	// MaxBytes is the gateway's cap. Over it, the gateway drops the details.
	MaxBytes = 4096
)

// Details is one write's change. Never a secret: no key, token, or invite token.
type Details map[string]any

// Change is a field's old and new value.
type Change struct {
	From any `json:"from"`
	To   any `json:"to"`
}

// Changed records field when from and to differ.
func (d Details) Changed(field string, from, to string) {
	if from != to {
		d[field] = Change{From: from, To: to}
	}
}

// ChangedPtr is Changed for a nullable value. Nil is null.
func (d Details) ChangedPtr(field string, from, to *string) {
	if (from == nil) != (to == nil) || (from != nil && *from != *to) {
		d[field] = Change{From: from, To: to}
	}
}

// Set puts d on the response. Empty sets nothing. Over the cap logs and sets nothing:
// the event still records the request, just not the change.
func Set(c fiber.Ctx, d Details) {
	if len(d) == 0 {
		return
	}
	value, err := Encode(d)
	if err != nil {
		httpx.Logger(c.Context()).Warn().Err(err).Msg("audit details not sent")
		return
	}
	c.Set(Header, value)
}

// SetCreated names the id a create produced.
func SetCreated(c fiber.Ctx, id string) {
	c.Set(CreatedHeader, id)
}

// SetNamed names the chain a user route changed, root first, and the member
// in that tree. Only for user routes: elsewhere the route's nodes already
// name the tree.
func SetNamed(c fiber.Ctx, nodeIDs []string, memberID string) {
	c.Set(NodesHeader, strings.Join(nodeIDs, ","))
	c.Set(MemberHeader, memberID)
}

// Encode is d as one JSON object of visible ASCII within MaxBytes. Non-ASCII is
// escaped as \u, which is still the same JSON.
func Encode(d Details) (string, error) {
	raw, err := json.Marshal(d)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.Grow(len(raw))
	for _, r := range string(raw) {
		switch {
		case r < utf8.RuneSelf:
			b.WriteRune(r)
		case r > 0xFFFF:
			r -= 0x10000
			fmt.Fprintf(&b, `\u%04x\u%04x`, 0xD800+(r>>10), 0xDC00+(r&0x3FF))
		default:
			fmt.Fprintf(&b, `\u%04x`, r)
		}
	}
	if b.Len() > MaxBytes {
		return "", fmt.Errorf("audit details are %d bytes, over %d", b.Len(), MaxBytes)
	}
	return b.String(), nil
}
