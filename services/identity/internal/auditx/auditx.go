// Package auditx says what an identity write changed, for the org's audit log
// (docs/audit.md#details). The gateway reads the header on audited routes,
// stores it as sent, and strips it. Identity does not know whether a route is
// audited or who called; it only reports the change.
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
