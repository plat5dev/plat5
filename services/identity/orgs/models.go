package orgs

import (
	"regexp"
	"strings"
	"time"

	"github.com/plat5dev/plat5/identity/internal/id"
)

const (
	MaxOrgNameLen = 128
	MaxSANameLen  = 128
	MaxUserIDLen  = 128

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
	StatusActive    Status = "active"
	StatusSuspended Status = "suspended"
	StatusRemoved   Status = "removed"
)

func (s Status) Valid() bool {
	switch s {
	case StatusActive, StatusSuspended, StatusRemoved:
		return true
	default:
		return false
	}
}

type Organization struct {
	ID        string
	Name      string
	Slug      string
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
	AddedBy          *string
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// ServiceAccount is a non-human identity owned by one organization.
// Always paired with exactly one member row in that org.
// Status is the joined member’s status (active or suspended; removed is unlistable).
type ServiceAccount struct {
	ID              string
	OrganizationID  string
	MemberID        string // filled on read via join
	Name            string
	Status          Status // filled on read via join
	CreatedByUserID *string
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
