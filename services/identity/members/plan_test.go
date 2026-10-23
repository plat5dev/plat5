package members

import (
	"slices"
	"sort"
	"testing"
	"time"
)

// POST /internal/plan (docs/reference/api/internal.md#plan-counts): what a
// kind removal would lose, and what's in the way of a narrowed parent. It
// lives with members' tests because the counts reach bindings, service
// accounts and keys.

func TestPlanCounts(t *testing.T) {
	e := setup(t)
	alice := e.person("alice")
	gruow, _ := e.org(alice, "Gruow")
	eng := e.child(gruow, "folder", "eng")
	p1 := e.child(eng, "project", "p1")
	p2 := e.child(eng, "project", "p2")
	gone := e.child(eng, "project", "gone")
	direct := e.child(gruow, "project", "direct")
	ops := e.child(gruow, "folder", "ops")
	var opsProjects []string
	for _, label := range []string{"o1", "o2", "o3", "o4"} {
		opsProjects = append(opsProjects, e.child(ops, "project", label).ID)
	}

	// Bindings on the folder's subtree: bob's owner on eng and viewer on p1;
	// carol's expired viewer on p1 doesn't count. A service account born at
	// p1 has developer there and member at the org, and two keys, one
	// revoked. One born at p2, which is then deleted: its binding is kept
	// for restore, its key revoked by the delete.
	bobM := e.join(gruow, "bob", "member")
	e.bind(eng, bobM, "owner", nil)
	e.bind(p1, bobM, "viewer", nil)
	carolM := e.join(gruow, "carol", "member")
	soon := e.clock.t.Add(time.Hour)
	expired := e.bind(p1, carolM, "viewer", &soon)
	e.exec(`UPDATE bindings SET expires_at = $2 WHERE id = $1`, expired.ID, e.clock.t.Add(-time.Minute))
	sa1, _ := e.serviceAccount(p1, "developer")
	e.bind(gruow, sa1, "member", nil)
	k, err := e.store.CreateKey(e.ctx, KeyOwner{NodeID: p1.ID, MemberID: sa1}, "old")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.store.RevokeKey(e.ctx, KeyOwner{NodeID: p1.ID, MemberID: sa1}, k.ID); err != nil {
		t.Fatal(err)
	}
	e.serviceAccount(p2, "developer")
	if _, err := e.nodes.Delete(e.ctx, e.snap(), p2.ID); err != nil {
		t.Fatal(err)
	}
	// A purged node is a tombstone: it never counts.
	e.exec(`UPDATE nodes SET status = 'purged' WHERE id = $1`, gone.ID)
	// Bindings outside the folders don't count: alice's owner at the org,
	// bob's member there, and someone on the direct project.
	e.bind(direct, bobM, "viewer", nil)

	plan := func(body string) response {
		t.Helper()
		r := e.callInternal("POST", "/internal/plan", body)
		e.expect(r, 200, body)
		return r
	}
	effects := func(r response, kind string) map[string]any {
		t.Helper()
		m, _ := r.body["kinds_removed"].(map[string]any)
		out, ok := m[kind].(map[string]any)
		if !ok {
			t.Fatalf("no entry for %s: %v", kind, r.body)
		}
		return out
	}
	inTheWay := func(r response, kind string) (float64, []string) {
		t.Helper()
		m, _ := r.body["parents_narrowed"].(map[string]any)
		w, ok := m[kind].(map[string]any)
		if !ok {
			t.Fatalf("no entry for %s: %v", kind, r.body)
		}
		sample := []string{}
		for _, id := range w["sample"].([]any) {
			sample = append(sample, id.(string))
		}
		return w["nodes"].(float64), sample
	}

	t.Run("kind removal counts every descendant and what hangs off them", func(t *testing.T) {
		r := plan(`{"kinds_removed": ["folder"], "parents_narrowed": []}`)
		got := effects(r, "folder")
		// eng, p1, p2 (deleted), ops and its four projects.
		want := map[string]any{"nodes": 8.0, "bindings": 5.0, "service_accounts": 2.0, "keys": 1.0}
		for k, v := range want {
			if got[k] != v {
				t.Fatalf("%s = %v, want %v (%v)", k, got[k], v, got)
			}
		}
	})

	t.Run("a kind with no nodes has zeros", func(t *testing.T) {
		got := effects(plan(`{"kinds_removed": ["nothing"], "parents_narrowed": []}`), "nothing")
		if got["nodes"] != 0.0 || got["bindings"] != 0.0 || got["service_accounts"] != 0.0 || got["keys"] != 0.0 {
			t.Fatalf("%v", got)
		}
	})

	t.Run("narrowed parent: nodes in the way, sample of five in id order", func(t *testing.T) {
		r := plan(`{"kinds_removed": [], "parents_narrowed": [{"kind": "project", "removed": ["folder"]}]}`)
		n, sample := inTheWay(r, "project")
		all := append([]string{p1.ID, p2.ID}, opsProjects...)
		sort.Strings(all)
		if n != 6 || !slices.Equal(sample, all[:5]) {
			t.Fatalf("nodes %v sample %v, want 6 %v", n, sample, all[:5])
		}
		n, sample = inTheWay(plan(`{"kinds_removed": [], "parents_narrowed": [{"kind": "project", "removed": ["organization"]}]}`), "project")
		if n != 1 || !slices.Equal(sample, []string{direct.ID}) {
			t.Fatalf("under organization: %v %v", n, sample)
		}
	})

	t.Run("a removal in the same plan takes nodes out of the way", func(t *testing.T) {
		r := plan(`{"kinds_removed": ["folder"], "parents_narrowed": [{"kind": "project", "removed": ["folder"]}]}`)
		if n, sample := inTheWay(r, "project"); n != 0 || len(sample) != 0 {
			t.Fatalf("%v %v", n, sample)
		}
		if effects(r, "folder")["nodes"] != 8.0 {
			t.Fatalf("%v", r.body)
		}
	})

	t.Run("empty request", func(t *testing.T) {
		r := plan(`{}`)
		if len(r.body["kinds_removed"].(map[string]any)) != 0 || len(r.body["parents_narrowed"].(map[string]any)) != 0 {
			t.Fatalf("%v", r.body)
		}
	})

	for _, body := range []string{
		`{"kinds_removed": [""]}`,
		`{"parents_narrowed": [{"kind": "", "removed": ["folder"]}]}`,
		`{"parents_narrowed": [{"kind": "project", "removed": [""]}]}`,
	} {
		if r := e.callInternal("POST", "/internal/plan", body); r.status != 422 {
			t.Fatalf("%s: status %d, want 422", body, r.status)
		}
	}
}
