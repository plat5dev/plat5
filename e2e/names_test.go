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

// A person's name (docs/reference/idp.md#a-persons-name). Under the e2e
// config's first_sign_in: no name claim leaves it null, PATCH /user sets it,
// and a later sign-in with a claim keeps it. Then with users.name.sync
// each_sign_in applied through the control plane: the claim wins, and the
// person's own edit is a 409. The e2e config is applied again at the end.
//
// The each_sign_in config is the e2e config with one setting changed, applied
// in this test, rather than a second stack: the harness applies its config
// through the admin API anyway, and a stack per config would double the run.
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
		t.Fatalf("first_sign_in, a later claim: name %s", showName(got))
	}

	// The edit is in the person's own log.
	if os.Getenv("AUDIT_ENABLED") != "false" {
		var set []auditEvent
		for _, e := range s.events(bare, "/user/audit-events", "PATCH /user") {
			if e.Method == "PATCH" && e.Route == "/user" && e.Status != nil && *e.Status == 200 {
				set = append(set, e)
			}
		}
		if len(set) != 1 {
			t.Fatalf("PATCH /user events: %+v", set)
		}
		change, _ := set[0].Details["name"].(map[string]any)
		if set[0].Outcome != "responded" || change == nil || change["from"] != nil || change["to"] != "Dana E2E" {
			t.Fatalf("PATCH /user event: %+v", set[0])
		}
	}

	// each_sign_in: the IdP owns names.
	each := s.config()
	each["users"] = map[string]any{"name": map[string]any{"sync": "each_sign_in"}}
	s.apply(each)
	t.Cleanup(func() { s.restoreFirstSignIn() })

	// The claim wins, once identity has the new config.
	idp := s.token(subject, map[string]any{"preferred_username": "idp-dana"})
	deadline := time.Now().Add(30 * time.Second)
	for got := s.me(idp); got == nil || *got != "idp-dana"; got = s.me(idp) {
		if time.Now().After(deadline) {
			t.Fatalf("each_sign_in: name %s, want the claim", showName(got))
		}
	}
	var refused apiError
	s.call(idp, "PATCH", "/user", map[string]any{"name": "Mine"}, 409, &refused)
	if refused.Error.Code != "CONFLICT" || refused.Error.Details["value"] != "each_sign_in" {
		t.Fatalf("own edit under each_sign_in: %+v", refused)
	}
	if got := s.me(bare); got == nil || *got != "idp-dana" {
		t.Fatalf("each_sign_in, no claim: name %s", showName(got))
	}
}

// restoreFirstSignIn applies the e2e config again and waits until identity
// has it: a fresh person's own edit is allowed again.
func (s *stack) restoreFirstSignIn() {
	s.apply(s.config())
	probe := s.token(fmt.Sprintf("e2e-names-probe-%d", time.Now().UnixNano()), nil)
	deadline := time.Now().Add(30 * time.Second)
	for {
		status, body, err := s.try(probe, "PATCH", "/user", map[string]any{"name": "Probe"})
		if err == nil && status == 200 {
			return
		}
		if time.Now().After(deadline) {
			s.t.Fatalf("first_sign_in not back: %v %d %s", err, status, body)
		}
		time.Sleep(500 * time.Millisecond)
	}
}
