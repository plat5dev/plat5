package memberkeys

import (
	"time"

	"github.com/plat5dev/plat5/identity/internal/apikey"
	"github.com/plat5dev/plat5/identity/internal/id"
)

// APIKey is an org-member credential for organization-scope routes.
type APIKey struct {
	ID        string
	MemberID  string
	Name      string
	KeyPrefix string
	KeyHash   string
	CreatedAt time.Time
	RevokedAt *time.Time
}

func HashKey(key string) string {
	return apikey.Hash(key)
}

func New(memberID, name, key, prefix string) *APIKey {
	return &APIKey{
		ID:        id.New(),
		MemberID:  memberID,
		Name:      name,
		KeyPrefix: apikey.DisplayPrefix(key, prefix),
		KeyHash:   apikey.Hash(key),
		CreatedAt: time.Now().UTC(),
	}
}

func (k *APIKey) IsRevoked() bool {
	return k.RevokedAt != nil
}
