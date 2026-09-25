package sessions

import (
	"time"

	"github.com/plat5dev/plat5/identity/internal/apikey"
	"github.com/plat5dev/plat5/identity/internal/id"
)

// TTL is fixed. Not boot config.
const TTL = time.Hour

// Session is a short-lived credential for one user member in one org.
// Not an API key: no name, no scopes column, no revoke.
type Session struct {
	ID          string
	MemberID    string
	TokenPrefix string
	TokenHash   string
	ExpiresAt   time.Time
	CreatedAt   time.Time
}

func New(memberID, token, prefix string, now time.Time) *Session {
	now = now.UTC()
	return &Session{
		ID:          id.New(),
		MemberID:    memberID,
		TokenPrefix: apikey.DisplayPrefix(token, prefix),
		TokenHash:   apikey.Hash(token),
		ExpiresAt:   now.Add(TTL),
		CreatedAt:   now,
	}
}

// Expired is true at expires_at. now == expires_at is expired.
func (s *Session) Expired(now time.Time) bool {
	return !now.UTC().Before(s.ExpiresAt.UTC())
}
