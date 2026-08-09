package keys

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/oklog/ulid/v2"
)

const (
	KeyPrefix           = "plat5-sk-1-"
	KeyRandomBytes      = 32
	KeyPrefixDisplayLen = 4
	MaxKeyNameLen       = 128
	DefaultListLimit    = 50
	MaxListLimit        = 100
)

type APIKey struct {
	ID        string     `json:"id"`
	UserID    string     `json:"user_id"`
	Name      string     `json:"name"`
	KeyPrefix string     `json:"key_prefix"`
	KeyHash   string     `json:"-"`
	CreatedAt time.Time  `json:"created_at"`
	RevokedAt *time.Time `json:"revoked_at,omitempty"`
}

func GenerateKey() (key string, err error) {
	randomBytes := make([]byte, KeyRandomBytes)
	if _, err := rand.Read(randomBytes); err != nil {
		return "", fmt.Errorf("failed to generate random bytes: %w", err)
	}

	encoded := base64.RawURLEncoding.EncodeToString(randomBytes)
	return KeyPrefix + encoded, nil
}

func HashKey(key string) string {
	hash := sha256.Sum256([]byte(key))
	return hex.EncodeToString(hash[:])
}

func ExtractPrefix(key string) string {
	if len(key) <= len(KeyPrefix)+KeyPrefixDisplayLen {
		return key
	}
	return key[:len(KeyPrefix)+KeyPrefixDisplayLen]
}

func NewAPIKey(userID, name, key string) *APIKey {
	return &APIKey{
		ID:        ulid.Make().String(),
		UserID:    userID,
		Name:      name,
		KeyPrefix: ExtractPrefix(key),
		KeyHash:   HashKey(key),
		CreatedAt: time.Now().UTC(),
	}
}

func (k *APIKey) IsRevoked() bool {
	return k.RevokedAt != nil
}

func (k *APIKey) Revoke() {
	now := time.Now().UTC()
	k.RevokedAt = &now
}
