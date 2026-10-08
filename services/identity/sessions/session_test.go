package sessions

import (
	"testing"
	"time"
)

func TestExpiredAtBoundary(t *testing.T) {
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	s := New("mem_1", "plat5-ms-1-secret", "plat5-ms-1-", now)
	if s.Expired(now) {
		t.Fatal("fresh session is not expired")
	}
	if s.ExpiresAt.Sub(now) != TTL {
		t.Fatalf("ttl: %s", s.ExpiresAt.Sub(now))
	}
	if !s.Expired(s.ExpiresAt) {
		t.Fatal("expires_at is expired")
	}
	if s.Expired(s.ExpiresAt.Add(-time.Nanosecond)) {
		t.Fatal("before expires_at is still valid")
	}
}
