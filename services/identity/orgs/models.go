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

// Membership is a user's member row joined to its organization.
// Not a table. Active user memberships only.
type Membership struct {
	ID               string
	Role             *string
	Status           Status
	OrganizationID   string
	OrganizationName string
	OrganizationSlug string
}

// Member is an org membership: a user or a service account. At most one of
// UserID or ServiceAccountID is set; neither once a removed person's account
// is deleted. Kind says which it was.
type Member struct {
	ID               string
	OrganizationID   string
	Kind             string
	UserID           *string
	ServiceAccountID *string
	// DisplayOverride is this org's name for the member, if set.
	DisplayOverride *string
	// DisplayName is what a roster shows: the override, else the person's
	// global name or the service account's name. Filled on read.
	DisplayName *string
	// Role is a slug from the roles file. Nil only when roles are off (docs/roles.md).
	Role      *string
	Status    Status
	CreatedAt time.Time
	UpdatedAt time.Time
}

// ServiceAccount is a non-human identity owned by one organization.
// Always paired with exactly one member row in that org.
// Status is the joined member’s status (active or suspended; removed is unlistable).
type ServiceAccount struct {
	ID             string
	OrganizationID string
	MemberID       string // filled on read via join
	Name           string
	Role           *string // filled on read via join
	Status         Status  // filled on read via join
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

func (m *Member) Principal() string {
	if m.Kind != "" {
		return m.Kind
	}
	if m.ServiceAccountID != nil {
		return PrincipalServiceAccount
	}
	return PrincipalUser
}

// IdentityID is the person or service account, or nil.
func (m *Member) IdentityID() *string {
	if m.ServiceAccountID != nil {
		return m.ServiceAccountID
	}
	return m.UserID
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
