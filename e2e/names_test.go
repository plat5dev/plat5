package e2e

import (
	"fmt"
	"os"
	"testing"
	"time"
)

type user struct {
	ID     string  `json:"id"`
	Name   *string `json:"name"`
	Status string  `json:"status"`
}

func showName(n *string) string {
	if n == nil {
		return "null"
	}
	return fmt.Sprintf("%q", *n)
}

// me is GET /user's name, with the admit cache waited out first so the
// sign-in reaches identity.
func (s *stack) me(who caller) *string {
	s.t.Helper()
	settle()
	var u user
	s.call(who, "GET", "/user", nil, 200, &u)
	return u.Name
}

// A person's name (docs/reference/idp.md#a-persons-name): seeded from
// AUTH_NAME_CLAIMS on first sign-in while empty, editable via PATCH /user,
// and never overwritten by later sign-ins.
func TestNames(t *testing.T) {
	s := newStack(t)
	run := fmt.Sprintf("%d", time.Now().UnixNano())
	subject := "e2e-names-" + run

	// No name claim: the name is null.
	bare := s.token(subject, nil)
	if got := s.me(bare); got != nil {
		t.Fatalf("no claim: name %s", showName(got))
	}

	// The person sets it.
	var u user
	s.call(bare, "PATCH", "/user", map[string]any{"name": "  Dana E2E "}, 200, &u)
	if u.Name == nil || *u.Name != "Dana E2E" || u.Status != "active" {
		t.Fatalf("PATCH /user: %+v", u)
	}
	s.call(bare, "PATCH", "/user", map[string]any{"name": ""}, 422, nil)

	// A later sign-in with a name claim leaves it.
	named := s.token(subject, map[string]any{"name": "From The IdP"})
	if got := s.me(named); got == nil || *got != "Dana E2E" {
		t.Fatalf("later claim overwrote name: %s", showName(got))
	}

	// Clearing lets the next sign-in with a claim seed it again.
	s.call(bare, "PATCH", "/user", map[string]any{"name": nil}, 200, &u)
	if u.Name != nil {
		t.Fatalf("clear name: %+v", u)
	}
	seeded := s.token(subject, map[string]any{"preferred_username": "idp-dana"})
	if got := s.me(seeded); got == nil || *got != "idp-dana" {
		t.Fatalf("re-seed after clear: name %s", showName(got))
	}

	// The edit is in the person's own log.
	if os.Getenv("AUDIT_ENABLED") != "false" {
		var set []auditEvent
		for _, e := range s.events(bare, "/user/audit-events", "PATCH /user") {
			if e.Method == "PATCH" && e.Route == "/user" && e.Status != nil && *e.Status == 200 {
				set = append(set, e)
			}
		}
		if len(set) < 1 {
			t.Fatalf("PATCH /user events: %+v", set)
		}
		found := false
		for _, ev := range set {
			change, _ := ev.Details["name"].(map[string]any)
			if ev.Outcome == "responded" && change != nil && change["from"] == nil && change["to"] == "Dana E2E" {
				found = true
			}
		}
		if !found {
			t.Fatalf("PATCH /user event with Dana E2E not found: %+v", set)
		}
	}
}
