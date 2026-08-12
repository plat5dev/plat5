package userkeys

import (
	"time"

	"github.com/plat5dev/plat5/identity/internal/apikey"
	"github.com/plat5dev/plat5/identity/internal/id"
)

// Wire format is independent of member keys. Gateway routes by this prefix.
const KeyPrefix = "plat5-sk-1-"

// APIKey is a person credential for user-scope routes.
type APIKey struct {
	ID        string
	UserID    string
	Name      string
	KeyPrefix string
	KeyHash   string
	CreatedAt time.Time
	RevokedAt *time.Time
}

func LooksLike(key string) bool {
	return apikey.LooksLike(key, KeyPrefix)
}

func GenerateKey() (string, error) {
	return apikey.Generate(KeyPrefix)
}

func HashKey(key string) string {
	return apikey.Hash(key)
}

func New(userID, name, key string) *APIKey {
	return &APIKey{
		ID:        id.New(),
		UserID:    userID,
		Name:      name,
		KeyPrefix: apikey.DisplayPrefix(key, KeyPrefix),
		KeyHash:   apikey.Hash(key),
		CreatedAt: time.Now().UTC(),
	}
}

func (k *APIKey) IsRevoked() bool {
	return k.RevokedAt != nil
}
