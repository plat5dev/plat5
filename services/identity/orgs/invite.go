package orgs

import (
	"encoding/json"
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

type InviteStatus string

const (
	InviteStatusActive   InviteStatus = "active"
	InviteStatusRedeemed InviteStatus = "redeemed"
	InviteStatusRevoked  InviteStatus = "revoked"
	InviteStatusExpired  InviteStatus = "expired"
)

// Invite is an org membership token. No pending member row is created until
// redeem; redeem inserts an active member.
type Invite struct {
	ID             string
	OrganizationID string
	Role           Role
	Email          *string
	Token          *string
	TokenHash      string
	TokenPrefix    string
	Status         InviteStatus
	MaxUses        *int
	UseCount       int
	CreatedBy      string
	ExpiresAt      time.Time
	CreatedAt      time.Time
}

// InviteDeadError is a hash hit on a terminal invite (redeemed/revoked/expired).
type InviteDeadError struct {
	Status InviteStatus
}

func (e *InviteDeadError) Error() string {
	return "invite " + string(e.Status)
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

// maxUsesField: omitted → default 1; JSON null → unlimited; 0/negative → 422.
type maxUsesField struct {
	present   bool
	unlimited bool
	n         int
}

func (m *maxUsesField) UnmarshalJSON(b []byte) error {
	m.present = true
	if string(b) == "null" {
		m.unlimited = true
		return nil
	}
	var n int
	if err := json.Unmarshal(b, &n); err != nil {
		return err
	}
	m.n = n
	return nil
}

func ParseMaxUses(f maxUsesField) (*int, error) {
	if !f.present {
		one := 1
		return &one, nil
	}
	if f.unlimited {
		return nil, nil
	}
	if f.n < 1 {
		return nil, errors.FieldError("max_uses", "Max uses must be at least 1.")
	}
	n := f.n
	return &n, nil
}

// ParseMaxUsesJSON: omitted → 1; JSON null → unlimited; 0/negative → 422.
func ParseMaxUsesJSON(body []byte) (*int, error) {
	if len(body) == 0 {
		return ParseMaxUses(maxUsesField{})
	}
	var probe struct {
		MaxUses json.RawMessage `json:"max_uses"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return nil, errors.InvalidRequestError()
	}
	if len(probe.MaxUses) == 0 {
		return ParseMaxUses(maxUsesField{})
	}
	var f maxUsesField
	if err := f.UnmarshalJSON(probe.MaxUses); err != nil {
		return nil, errors.FieldError("max_uses", "Max uses must be at least 1.")
	}
	return ParseMaxUses(f)
}

func InviteRedeemable(inv *Invite, now time.Time) bool {
	if inv == nil {
		return false
	}
	if inv.Status != InviteStatusActive {
		return false
	}
	if !now.Before(inv.ExpiresAt) {
		return false
	}
	if inv.MaxUses != nil && inv.UseCount >= *inv.MaxUses {
		return false
	}
	return true
}

func expireInvite(inv *Invite) {
	inv.Status = InviteStatusExpired
	inv.Token = nil
}
