package userkeys

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"
)

// Wire format is independent of member keys. Gateway routes by this prefix.
const (
	KeyPrefix           = "plat5-sk-1-"
	KeyRandomBytes      = 32
	KeyPrefixDisplayLen = 4
	MaxKeyNameLen       = 128
	DefaultListLimit    = 50
	MaxListLimit        = 100
)

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
	return strings.HasPrefix(key, KeyPrefix)
}

func GenerateKey() (string, error) {
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

func New(userID, name, key string) *APIKey {
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
