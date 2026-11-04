package members

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/plat5dev/plat5/identity/internal/auditx"
)

// A person's name (docs/reference/idp.md#a-persons-name): seeded by sign-ins
// as users.name.sync says, set or cleared by the person with PATCH /user
// (X-Plat5-User-Id) and by an operator on the service API (no header).
// Which claim the gateway sends is its own: admit gets one name, the first
// non-empty claim, and the gateway's tests cover the order.

// eachSignInConfig is testConfig with users.name.sync each_sign_in.
var eachSignInConfig = strings.Replace(testConfig, `"sync": "first_sign_in"`, `"sync": "each_sign_in"`, 1)

// signIn admits subject on a user route, with name as the name claim, or no
// claim for nil.
func (e *env) signIn(subject string, name *string) {
	e.t.Helper()
	claim := ""
	if name != nil {
		raw, _ := json.Marshal(*name)
		claim = `, "name": ` + string(raw)
	}
	r := e.admit(admitBody(`"idp": {"issuer": "https://idp.test", "subject": "`+subject+`"`+claim+`}`, false))
	if r.status != 200 || r.body["valid"] != true {
		e.t.Fatalf("admit %s: %d %v", subject, r.status, r.body)
	}
}

// nameOf is the person's name on GET /users/{user_id}, nil for null.
func (e *env) nameOf(user string) *string {
	e.t.Helper()
	r := e.call("GET", "/users/"+user, "", "")
	e.expect(r, 200, "get user")
	if r.body["name"] == nil {
		return nil
	}
	s := r.body["name"].(string)
	return &s
}

func ptr(s string) *string { return &s }

func show(p *string) string {
	if p == nil {
		return "null"
	}
	return fmt.Sprintf("%q", *p)
}

func (e *env) wantName(user string, want *string, what string) {
	e.t.Helper()
	if got := e.nameOf(user); show(got) != show(want) {
		e.t.Fatalf("%s: name %s, want %s", what, show(got), show(want))
	}
}

func nameBody(name *string) string {
	raw, _ := json.Marshal(map[string]*string{"name": name})
	return string(raw)
}

func TestNameFirstSignIn(t *testing.T) {
	e := setup(t)

	// No claim: the name is null until it's known.
	e.signIn("dana", nil)
	user := e.userOf("dana")
	e.wantName(user, nil, "no claim")
	e.signIn("dana", ptr("   "))
	e.wantName(user, nil, "a blank claim")

	// The first non-empty claim sets it, trimmed; later ones never change it.
	e.signIn("dana", ptr("  Dana Q  "))
	e.wantName(user, ptr("Dana Q"), "first claim")
	e.signIn("dana", ptr("Someone Else"))
	e.wantName(user, ptr("Dana Q"), "a later claim")

	// The person sets it, and a sign-in keeps theirs.
	r := e.callUser("PATCH", "/users/"+user, user, nameBody(ptr(" Dana ")))
	e.expect(r, 200, "own edit")
	if r.str("id") != user || r.str("name") != "Dana" || r.str("status") != "active" {
		t.Fatalf("own edit: %v", r.body)
	}
	if got := r.headers[auditx.Header]; got != `{"name":{"from":"Dana Q","to":"Dana"}}` {
		t.Fatalf("audit details %s", got)
	}
	e.signIn("dana", ptr("From The IdP"))
	e.wantName(user, ptr("Dana"), "a claim after the person's edit")

	// The same name again changes nothing and says nothing changed.
	r = e.callUser("PATCH", "/users/"+user, user, nameBody(ptr("Dana")))
	e.expect(r, 200, "same name")
	if r.headers[auditx.Header] != "" {
		t.Fatalf("no change, details %s", r.headers[auditx.Header])
	}

	// Clearing it lets the claims set it again.
	r = e.callUser("PATCH", "/users/"+user, user, `{"name": null}`)
	e.expect(r, 200, "clear")
	if r.body["name"] != nil {
		t.Fatalf("cleared: %v", r.body)
	}
	if got := r.headers[auditx.Header]; got != `{"name":{"from":"Dana","to":null}}` {
		t.Fatalf("audit details %s", got)
	}
	e.signIn("dana", ptr(""))
	e.wantName(user, nil, "an empty claim after clearing")
	e.signIn("dana", ptr("Dana Again"))
	e.wantName(user, ptr("Dana Again"), "a claim after clearing")

	// An operator sets and clears it too, the same edit.
	r = e.call("PATCH", "/users/"+user, "", nameBody(ptr("Set By Ops")))
	e.expect(r, 200, "operator edit")
	e.wantName(user, ptr("Set By Ops"), "operator edit")
	e.signIn("dana", ptr("Dana"))
	e.wantName(user, ptr("Set By Ops"), "a claim after an operator's edit")
	e.expect(e.call("PATCH", "/users/"+user, "", `{"name": null}`), 200, "operator clear")
	e.wantName(user, nil, "operator clear")
	e.signIn("dana", ptr("Dana Back"))
	e.wantName(user, ptr("Dana Back"), "a claim after an operator's clear")

	// A name change isn't an event.
	if got := e.userEventsAfter(0); len(got) != 1 {
		t.Fatalf("user events %v", got)
	}
}

func TestNameEachSignIn(t *testing.T) {
	e := setupWith(t, eachSignInConfig)

	e.signIn("erin", nil)
	user := e.userOf("erin")
	e.wantName(user, nil, "no claim")
	e.signIn("erin", ptr("Erin"))
	e.wantName(user, ptr("Erin"), "first claim")
	e.signIn("erin", ptr("Erin Renamed"))
	e.wantName(user, ptr("Erin Renamed"), "a later claim wins")
	e.signIn("erin", ptr(""))
	e.wantName(user, ptr("Erin Renamed"), "an empty claim")
	e.signIn("erin", nil)
	e.wantName(user, ptr("Erin Renamed"), "no claim")

	// The IdP owns the name: an edit is refused, a clear too, whoever sends
	// it: the person (PATCH /user, with X-Plat5-User-Id) or an operator.
	for _, body := range []string{nameBody(ptr("Mine")), `{"name": null}`, nameBody(ptr(""))} {
		for who, r := range map[string]response{
			"person":   e.callUser("PATCH", "/users/"+user, user, body),
			"operator": e.call("PATCH", "/users/"+user, "", body),
		} {
			e.expect(r, 409, who+" edit "+body)
			if r.code() != "NAME_OWNED_BY_IDP" || r.message() != "This name comes from the sign-in provider." || r.body["error"].(map[string]any)["details"] != nil {
				t.Fatalf("%s edit: %v", who, r.body)
			}
			if r.headers[auditx.Header] != "" {
				t.Fatalf("%s edit: details %s", who, r.headers[auditx.Header])
			}
		}
	}
	e.wantName(user, ptr("Erin Renamed"), "after refused edits")
	e.expect(e.call("PATCH", "/users/01jbx9q3t5vk8m2n4p6r7s8t9w", "", nameBody(ptr("X"))), 404, "unknown user")
	e.signIn("erin", ptr("Erin"))
	e.wantName(user, ptr("Erin"), "the next claim wins")
}

// The name's shape: 1 to 128 characters after trimming, like other names.
func TestNameShape(t *testing.T) {
	e := setup(t)
	user := e.person("fay")
	for _, c := range []struct {
		body, message string
	}{
		{`{}`, "Nothing to update."},
		{`{"status": "suspended"}`, "Nothing to update."},
		{nameBody(ptr("")), "Name is required."},
		{nameBody(ptr("   ")), "Name is required."},
		{nameBody(ptr(strings.Repeat("é", 129))), "Name is too long."},
		{`{"name": 5}`, "That doesn't look right."},
	} {
		r := e.callUser("PATCH", "/users/"+user, user, c.body)
		e.expect(r, 422, c.body)
		if r.message() != c.message {
			t.Fatalf("%s: %q", c.body, r.message())
		}
	}
	e.wantName(user, nil, "after refused edits")
	long := strings.Repeat("é", 128)
	r := e.callUser("PATCH", "/users/"+user, user, nameBody(ptr("  "+long+"  ")))
	e.expect(r, 200, "128 characters")
	e.wantName(user, &long, "128 characters")

	// A claim past the limit is cut to it.
	e.signIn("gus", ptr(strings.Repeat("x", 200)))
	cut := strings.Repeat("x", 128)
	e.wantName(e.userOf("gus"), &cut, "a long claim")
	e.signIn("hal", ptr(strings.Repeat("y", 127)+" z"))
	cut = strings.Repeat("y", 127)
	e.wantName(e.userOf("hal"), &cut, "a long claim cut at a space")
}

// Identity doesn't read X-Plat5-User-Id on a name edit: the path names the
// person, whoever sends it.
func TestNameReadsNoCaller(t *testing.T) {
	e := setup(t)
	user, other := e.person("ida"), e.person("jo")
	e.expect(e.callUser("PATCH", "/users/"+user, other, nameBody(ptr("Ida"))), 200, "another user's header")
	e.expect(e.callUser("PATCH", "/users/"+user, "01jbx9q3t5vk8m2n4p6r7s8t9w", nameBody(ptr("Ida"))), 200, "a header naming no one")
	e.expect(e.call("PATCH", "/users/01jbx9q3t5vk8m2n4p6r7s8t9w", "", nameBody(ptr("Ida"))), 404, "unknown user")
	e.expect(e.call("PATCH", "/users/nope", "", nameBody(ptr("Ida"))), 404, "not an id")
	if _, _, err := e.store.SetStatus(e.ctx, user, StatusSuspended); err != nil {
		t.Fatal(err)
	}
	e.expect(e.call("PATCH", "/users/"+user, "", nameBody(ptr("Ida Q"))), 200, "a suspended person's name")
	e.wantName(user, ptr("Ida Q"), "edits")
	e.wantName(other, nil, "the other person")
}

// A person an admin added has no name until a sign-in or an edit gives one.
func TestNameAdminAdded(t *testing.T) {
	e := setup(t)
	org, _ := e.org(e.person("owner"), "Gruow")
	e.join(org, "kim", "member")
	user := e.userOf("kim")
	e.wantName(user, nil, "added")
	e.signIn("kim", ptr("Kim"))
	e.wantName(user, ptr("Kim"), "first sign-in")
}

// Concurrent first sign-ins of one subject, each with its own claim, make one
// person with one of the names, and later sign-ins keep it. The same for
// concurrent sign-ins of a person with no name yet.
func TestNameConcurrentFirstSignIns(t *testing.T) {
	e := setup(t)
	race := func(subject string) map[string]bool {
		var wg sync.WaitGroup
		sent := map[string]bool{}
		for i := range 8 {
			name := fmt.Sprintf("Racer %d", i)
			sent[name] = true
			wg.Add(1)
			go func() {
				defer wg.Done()
				e.admit(admitBody(`"idp": {"issuer": "https://idp.test", "subject": "`+subject+`", "name": "`+name+`"}`, false))
			}()
		}
		wg.Wait()
		return sent
	}

	sent := race("racer")
	if e.count(`SELECT count(*) FROM principals`) != 1 {
		t.Fatal("more than one person")
	}
	user := e.userOf("racer")
	won := e.nameOf(user)
	if won == nil || !sent[*won] {
		t.Fatalf("name %s", show(won))
	}
	e.signIn("racer", ptr("Late"))
	e.wantName(user, won, "a later sign-in")

	// The person exists with no name: one of the racing claims sets it.
	e.signIn("blank", nil)
	sent = race("blank")
	won = e.nameOf(e.userOf("blank"))
	if won == nil || !sent[*won] {
		t.Fatalf("name %s", show(won))
	}
}
