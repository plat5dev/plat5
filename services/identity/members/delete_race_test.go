package members

import (
	"fmt"
	"testing"
	"time"

	"github.com/plat5dev/plat5/identity/nodes"
)

// noGhosts fails if any active member has no principal, or any removed member
// has no member.removed.
func (e *env) noGhosts() {
	e.t.Helper()
	if n := e.count(`SELECT count(*) FROM members WHERE removed_at IS NULL AND principal_id IS NULL`); n != 0 {
		e.t.Fatalf("%d active members with no principal", n)
	}
	if n := e.count(`
		SELECT count(*) FROM members m WHERE m.removed_at IS NOT NULL
		AND NOT EXISTS (SELECT 1 FROM events ev WHERE ev.type = 'member.removed' AND ev.member_id = m.id)
	`); n != 0 {
		e.t.Fatalf("%d removed members with no member.removed", n)
	}
}

func (e *env) principalOf(userID string) string {
	e.t.Helper()
	var principalID string
	if err := e.pool.QueryRow(e.ctx, `SELECT id FROM principals WHERE user_id = $1`, userID).Scan(&principalID); err != nil {
		e.t.Fatal(err)
	}
	return principalID
}

// waitForLockWait returns once some other session waits on a lock.
func (e *env) waitForLockWait() {
	e.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if e.count(`SELECT count(*) FROM pg_stat_activity WHERE wait_event_type = 'Lock' AND datname = current_database()`) > 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	e.t.Fatal("no session waited on a lock")
}

// joins are the three ways a person gains a member while their account may
// be deleting: a redeem, an admin add, and a root create. Each returns the
// status. ok is the status of a join that landed; gone is the answer once the
// account is deleted.
type join struct {
	name     string
	ok, gone int
	run      func(e *env, userID, subject string, other *nodes.Node) int
}

var joins = []join{
	{"redeem", 200, 404, func(e *env, userID, _ string, other *nodes.Node) int {
		inv := e.invite(other, "member")
		return e.callUser("POST", "/users/"+userID+"/invites/redeem", userID, `{"token": "`+inv.Token+`"}`).status
	}},
	{"admin add", 201, 201, func(e *env, _, subject string, other *nodes.Node) int {
		return e.call("POST", "/nodes/"+other.ID+"/members", "",
			`{"idp": {"issuer": "https://idp.test", "subject": "`+subject+`"}, "role": "member"}`).status
	}},
	{"org create", 201, 400, func(e *env, userID, _ string, _ *nodes.Node) int {
		return e.callUser("POST", "/roots/organization", userID, `{"display_name": "Late"}`).status
	}},
}

// An account deletion racing a join never leaves an active member without a
// principal, and every member it removes gets its member.removed.
func TestDeleteAccountRacesJoins(t *testing.T) {
	e := setup(t)
	owner := e.person("owner")
	other, _ := e.org(owner, "Other")
	for _, j := range joins {
		for i := range 20 {
			subject := fmt.Sprintf("race-%s-%d", j.name, i)
			userID := e.person(subject)
			e.org(userID, "Theirs")
			deleted := make(chan int)
			joined := make(chan int)
			go func() { deleted <- e.call("DELETE", "/users/"+userID, "", "").status }()
			go func() { joined <- j.run(e, userID, subject, other) }()
			if s := <-deleted; s != 204 {
				t.Fatalf("%s %d: delete %d", j.name, i, s)
			}
			if s := <-joined; s != j.ok && s != j.gone {
				t.Fatalf("%s %d: join %d, want %d or %d", j.name, i, s, j.ok, j.gone)
			}
			e.noGhosts()
		}
	}
}

// A join that inserted a member before the deletion locked the principal is
// waited for, and its member is removed with the rest.
func TestDeleteAccountRemovesALateMember(t *testing.T) {
	e := setup(t)
	owner := e.person("owner")
	other, _ := e.org(owner, "Other")
	userID := e.person("late")
	_, known := e.org(userID, "Theirs")
	principalID := e.principalOf(userID)
	before := e.lastEvent()

	// A join, mid-flight: its tree locked and its member inserted, not committed.
	w, err := e.store.begin(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer w.rollback(e.ctx)
	if err := nodes.LockTrees(e.ctx, w.tx, []string{other.ID}); err != nil {
		t.Fatal(err)
	}
	late, err := e.store.insertMember(e.ctx, w, principalID, PrincipalPerson, other.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.insertBinding(e.ctx, w, late, other.ID, "member"); err != nil {
		t.Fatal(err)
	}

	deleted := make(chan int)
	go func() { deleted <- e.call("DELETE", "/users/"+userID, "", "").status }()
	e.waitForLockWait()
	if err := w.commit(e.ctx); err != nil {
		t.Fatal(err)
	}
	if s := <-deleted; s != 204 {
		t.Fatalf("delete %d", s)
	}
	for _, m := range []string{known, late} {
		if !e.removed(m) {
			t.Fatalf("member %s not removed", m)
		}
	}
	e.noGhosts()
	if n := e.count(`SELECT count(*) FROM events WHERE id > $1 AND type = 'member.removed' AND member_id = $2`, before, late); n != 1 {
		t.Fatalf("late member.removed events %d", n)
	}
	if n := e.count(`SELECT count(*) FROM bindings WHERE member_id = $1`, late); n != 0 {
		t.Fatalf("late member keeps %d bindings", n)
	}
}

// A join that reaches its insert after the deletion locked the principal waits,
// then answers as it would once the account is gone.
func TestJoinAfterDeleteLockedThePrincipal(t *testing.T) {
	for _, j := range joins {
		t.Run(j.name, func(t *testing.T) {
			e := setup(t)
			owner := e.person("owner")
			other, _ := e.org(owner, "Other")
			subject := "locked"
			userID := e.person(subject)
			principalID := e.principalOf(userID)

			// The deletion, past its principal lock: it holds the row.
			tx, err := e.pool.Begin(e.ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(e.ctx)
			if _, err := tx.Exec(e.ctx, `SELECT 1 FROM principals WHERE id = $1 FOR UPDATE`, principalID); err != nil {
				t.Fatal(err)
			}
			joined := make(chan int)
			go func() { joined <- j.run(e, userID, subject, other) }()
			e.waitForLockWait()
			if _, err := tx.Exec(e.ctx, `DELETE FROM principals WHERE id = $1`, principalID); err != nil {
				t.Fatal(err)
			}
			if err := tx.Commit(e.ctx); err != nil {
				t.Fatal(err)
			}
			if s := <-joined; s != j.gone {
				t.Fatalf("join %d, want %d", s, j.gone)
			}
			e.noGhosts()
			if j.name == "admin add" {
				if n := e.count(`SELECT count(*) FROM principals p JOIN idp_links l ON l.principal_id = p.id WHERE l.subject = $1`, subject); n != 1 {
					t.Fatalf("admin add made %d people for the subject", n)
				}
			}
		})
	}
}
