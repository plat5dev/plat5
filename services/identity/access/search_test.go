package access_test

import (
	"encoding/json"
	"fmt"
	"slices"
	"testing"

	"github.com/plat5dev/plat5/identity/internal/id"
	"github.com/plat5dev/plat5/identity/members"
	"github.com/plat5dev/plat5/identity/nodes"
)

func (e *env) searchResources(body string) (all bool, nodeIDs []string, nextToken string) {
	e.t.Helper()
	r := e.callInternal("POST", "/access/v1/search/resource", body)
	if r.status != 200 {
		e.t.Fatalf("search/resource %s: %d %v", body, r.status, r.body)
	}
	all, ok := r.body["all"].(bool)
	if !ok {
		e.t.Fatalf("missing all bool: %v", r.body)
	}
	for _, it := range r.list("results") {
		nodeIDs = append(nodeIDs, it["id"].(string))
	}
	if pg, ok := r.body["page"].(map[string]any); ok && pg != nil {
		nextToken, _ = pg["next_token"].(string)
	}
	return all, nodeIDs, nextToken
}

func TestSearchResources(t *testing.T) {
	e := setup(t)
	org, alice := e.org(e.person("alice"), "Gruow")
	f1 := e.child(org, "folder", "F1")
	f2 := e.child(org, "folder", "F2")
	fDel := e.child(org, "folder", "FDel")
	p1 := e.child(f1, "project", "P1")
	p2 := e.child(f1, "project", "P2")
	p3 := e.child(f2, "project", "P3")
	p4 := e.child(f2, "project", "P4")
	pDeleted := e.child(f1, "project", "PDeleted")
	pUnderDeletedFolder := e.child(fDel, "project", "PUnderDel")
	other, erin := e.org(e.person("erin"), "Other")

	// bob: developer on org (all: true across org).
	bob := e.join(org, "bob", "developer")
	// dana: nobody on org, developer on f1 (covers p1, p2), viewer on p3.
	dana := e.join(org, "dana", "nobody")
	e.bind(f1, dana, "developer")
	e.bind(p3, dana, "viewer")
	e.bind(pDeleted, dana, "viewer")
	e.bind(pUnderDeletedFolder, dana, "viewer")

	// carol: guest via group bound on f2 as developer (covers p3, p4).
	carol := e.join(org, "carol", "nobody")
	g := e.call("POST", "/nodes/"+org.ID+"/groups", "", `{"name": "TeamF2"}`).str("id")
	e.expect(e.call("POST", "/nodes/"+org.ID+"/groups/"+g+"/members", "", fmt.Sprintf(`{"member_id": %q}`, carol)), 201, "add carol")
	e.expect(e.call("POST", "/nodes/"+f2.ID+"/bindings", "", fmt.Sprintf(`{"group_id": %q, "role": "developer"}`, g)), 201, "bind g on f2")
	// Overlapping binding on p3 to verify deduplication.
	e.bind(p3, carol, "viewer")

	// frank: suspended developer on org.
	frank := e.join(org, "frank", "developer")
	if _, _, err := e.members.SetStatus(e.ctx, e.userOf("frank"), members.StatusSuspended); err != nil {
		t.Fatal(err)
	}

	if _, err := e.nodes.Delete(e.ctx, e.snap(), pDeleted.ID); err != nil {
		t.Fatal(err)
	}
	// Mark fDel deleted directly while leaving pUnderDeletedFolder active to test ancestor check.
	e.exec(`UPDATE nodes SET status = 'deleted' WHERE id = $1`, fDel.ID)

	req := func(member, kind, perm string, scope *nodes.Node, extra string) string {
		m := map[string]any{
			"subject":  map[string]any{"type": "member", "id": member},
			"action":   map[string]any{"name": perm},
			"resource": map[string]any{"type": kind},
		}
		if scope != nil {
			m["scope"] = map[string]any{"node_id": scope.ID}
		}
		if extra != "" {
			var pg map[string]any
			_ = json.Unmarshal([]byte(extra), &pg)
			m["page"] = pg
		}
		b, _ := json.Marshal(m)
		return string(b)
	}

	// Short-circuit all: true for org-level admin & developer (scope omitted or explicit).
	all, got, next := e.searchResources(req(alice, "project", "anything:custom", nil, ""))
	if !all || len(got) != 0 || next != "" {
		t.Fatalf("alice all: %v %v %q", all, got, next)
	}
	all, got, _ = e.searchResources(req(bob, "project", "project:write", org, ""))
	if !all || len(got) != 0 {
		t.Fatalf("bob all: %v %v", all, got)
	}
	// Short-circuit all: true when scope is f1 and dana holds developer on f1.
	all, got, _ = e.searchResources(req(dana, "project", "project:write", f1, ""))
	if !all || len(got) != 0 {
		t.Fatalf("dana on f1 should short-circuit all:true: %v %v", all, got)
	}
	// Short-circuit via group binding on f2 for carol.
	all, got, _ = e.searchResources(req(carol, "project", "project:write", f2, ""))
	if !all || len(got) != 0 {
		t.Fatalf("carol on f2 via group should short-circuit all:true: %v %v", all, got)
	}

	// Sparse results across org for dana: p1, p2 (via f1), p3 (direct), excluding deleted & under-deleted-folder.
	all, got, next = e.searchResources(req(dana, "project", "project:read", org, ""))
	if all || !slices.Equal(got, sorted(p1.ID, p2.ID, p3.ID)) || next != "" {
		t.Fatalf("dana project:read across org: all=%v got=%v next=%q", all, got, next)
	}
	// For project:write across org, dana only reaches p1 and p2 (viewer on p3 doesn't grant write).
	all, got, _ = e.searchResources(req(dana, "project", "project:write", nil, ""))
	if all || !slices.Equal(got, sorted(p1.ID, p2.ID)) {
		t.Fatalf("dana project:write: all=%v got=%v", all, got)
	}

	// Sparse results via group + deduplication for carol across org: p3 and p4 once each.
	all, got, _ = e.searchResources(req(carol, "project", "project:read", org, ""))
	if all || !slices.Equal(got, sorted(p3.ID, p4.ID)) {
		t.Fatalf("carol via group: all=%v got=%v", all, got)
	}

	// Pagination on sparse results.
	all, page1, next1 := e.searchResources(req(dana, "project", "project:read", org, `{"limit": 2}`))
	wantAll := sorted(p1.ID, p2.ID, p3.ID)
	if all || !slices.Equal(page1, wantAll[:2]) || next1 != wantAll[1] {
		t.Fatalf("page1: all=%v got=%v next=%q", all, page1, next1)
	}
	all, page2, next2 := e.searchResources(req(dana, "project", "project:read", org, fmt.Sprintf(`{"limit": 2, "token": %q}`, next1)))
	if all || !slices.Equal(page2, wantAll[2:]) || next2 != "" {
		t.Fatalf("page2: all=%v got=%v next=%q", all, page2, next2)
	}

	// No-match cases return 200 { all: false, results: [] }.
	for name, body := range map[string]string{
		"suspended member":     req(frank, "project", "project:read", org, ""),
		"unknown member":       req(id.New(), "project", "project:read", org, ""),
		"cross-tree scope":     req(erin, "project", "project:read", org, ""),
		"other org scope":      req(bob, "project", "project:read", other, ""),
		"deleted scope":        req(bob, "project", "project:read", fDel, ""),
		"unknown kind":         req(alice, "nope", "project:read", org, ""),
		"upward kind":          req(alice, "organization", "organization:read", f1, ""),
		"ungranted permission": req(bob, "project", "project:delete", org, ""),
	} {
		t.Run(name, func(t *testing.T) {
			all, got, next := e.searchResources(body)
			if all || len(got) != 0 || next != "" {
				t.Fatalf("%s: all=%v got=%v next=%q", name, all, got, next)
			}
		})
	}

	// Invalid request returns 400.
	r := e.callInternal("POST", "/access/v1/search/resource", `{"subject": {"type": "user", "id": "`+bob+`"}}`)
	if r.status != 400 || r.code() != "INVALID_REQUEST" {
		t.Fatalf("invalid search: %d %v", r.status, r.body)
	}
}
