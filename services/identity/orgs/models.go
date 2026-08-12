package orgs

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strings"
	"time"

	"github.com/plat5dev/plat5/identity/internal/id"
)

const (
	MaxOrgNameLen    = 128
	MaxSANameLen     = 128
	MaxUserIDLen     = 128
	MaxSettingsBytes = 16 << 10 // 16 KiB

	PrincipalUser           = "user"
	PrincipalServiceAccount = "service_account"
)

type Role string

const (
	RoleMember Role = "member"
	RoleAdmin  Role = "admin"
	RoleOwner  Role = "owner"
)

func (r Role) Valid() bool {
	switch r {
	case RoleMember, RoleAdmin, RoleOwner:
		return true
	default:
		return false
	}
}

type Status string

const (
	StatusPending   Status = "pending"
	StatusActive    Status = "active"
	StatusSuspended Status = "suspended"
	StatusRemoved   Status = "removed"
)

func (s Status) Valid() bool {
	switch s {
	case StatusPending, StatusActive, StatusSuspended, StatusRemoved:
		return true
	default:
		return false
	}
}

type Organization struct {
	ID        string
	Name      string
	Slug      string
	Settings  []byte
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Member is an org principal: exactly one of UserID or ServiceAccountID.
type Member struct {
	ID               string
	OrganizationID   string
	UserID           *string
	ServiceAccountID *string
	Role             Role
	Status           Status
	InvitedBy        *string
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// ServiceAccount is a non-human identity owned by an organization.
// Always paired with a member row in home_organization_id.
type ServiceAccount struct {
	ID              string
	OrganizationID  string // home_organization_id
	MemberID        string // filled on read via join
	Name            string
	CreatedByUserID *string
	DisabledAt      *time.Time
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

func (m *Member) Principal() string {
	if m.ServiceAccountID != nil {
		return PrincipalServiceAccount
	}
	return PrincipalUser
}

func (m *Member) IsUser(userID string) bool {
	return m.UserID != nil && *m.UserID == userID
}

func NewULID() string {
	return id.New()
}

var (
	slugSanitize = regexp.MustCompile(`[^a-z0-9]+`)
	slugValid    = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)
)

func Slugify(name string) string {
	s := strings.ToLower(strings.TrimSpace(name))
	s = slugSanitize.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if s == "" {
		s = "org"
	}
	if len(s) > 64 {
		s = s[:64]
		s = strings.Trim(s, "-")
	}
	return s
}

func ValidSlug(slug string) bool {
	if slug == "" || len(slug) > 64 {
		return false
	}
	return slugValid.MatchString(slug)
}

// ValidSettingsObject reports whether b is empty or a JSON object (not array/scalar/null).
func ValidSettingsObject(b []byte) bool {
	if len(b) == 0 {
		return true
	}
	if len(b) > MaxSettingsBytes {
		return false
	}
	trim := bytes.TrimSpace(b)
	if len(trim) == 0 || trim[0] != '{' {
		return false
	}
	var obj map[string]json.RawMessage
	return json.Unmarshal(trim, &obj) == nil
}
