package orgs

import (
	"net/url"
	"strings"
	"time"

	"github.com/plat5dev/plat5/identity/errors"
	"github.com/plat5dev/plat5/identity/internal/apikey"
)

const (
	InviteTokenPrefix = "inv_"
	DefaultInviteTTL  = 7 * 24 * time.Hour
	MinInviteTTL      = 60 * time.Second
	MaxInviteTTL      = 30 * 24 * time.Hour
	MaxInviteEmailLen = 320
)

// Invite is a one-shot org membership token. No pending member row is created
// until redeem; redeem inserts an active member.
type Invite struct {
	ID             string
	OrganizationID string
	Role           Role
	Email          *string
	TokenHash      string
	TokenPrefix    string
	CreatedBy      string
	ExpiresAt      time.Time
	RedeemedAt     *time.Time
	RedeemedBy     *string
	RevokedAt      *time.Time
	CreatedAt      time.Time
}

func GenerateInviteToken() (string, error) {
	return apikey.Generate(InviteTokenPrefix)
}

func HashInviteToken(token string) string {
	return apikey.Hash(token)
}

func LooksLikeInviteToken(token string) bool {
	return apikey.LooksLike(token, InviteTokenPrefix)
}

func InviteDisplayPrefix(token string) string {
	return apikey.DisplayPrefix(token, InviteTokenPrefix)
}

func ParseInviteTTL(seconds *int) (time.Duration, error) {
	if seconds == nil {
		return DefaultInviteTTL, nil
	}
	min := int(MinInviteTTL / time.Second)
	max := int(MaxInviteTTL / time.Second)
	if *seconds < min || *seconds > max {
		return 0, errors.FieldError("expires_in_seconds", "Expiry must be between 60 seconds and 30 days.")
	}
	return time.Duration(*seconds) * time.Second, nil
}

func ParseInviteEmail(raw string) (*string, error) {
	email := strings.TrimSpace(raw)
	if email == "" {
		return nil, nil
	}
	if len(email) > MaxInviteEmailLen {
		return nil, errors.FieldError("email", "That email is too long.")
	}
	return &email, nil
}

// InviteRedeemable is true when the token can still be consumed.
// Unknown / expired / revoked / already used all map to the same 404 at the HTTP layer
// so a used token does not leak org internals.
func InviteRedeemable(inv *Invite, now time.Time) bool {
	if inv == nil {
		return false
	}
	if inv.RevokedAt != nil || inv.RedeemedAt != nil {
		return false
	}
	return now.Before(inv.ExpiresAt)
}

// BuildInviteURL appends invite= to an operator-configured Auth authorize URL.
// Empty or unparseable authorizeURL yields "".
func BuildInviteURL(authorizeURL, token string) string {
	authorizeURL = strings.TrimSpace(authorizeURL)
	if authorizeURL == "" || token == "" {
		return ""
	}
	u, err := url.Parse(authorizeURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	q := u.Query()
	q.Set("invite", token)
	u.RawQuery = q.Encode()
	return u.String()
}
