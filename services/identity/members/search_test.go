package members

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/plat5dev/plat5/identity/internal/id"
	"github.com/plat5dev/plat5/identity/nodes"
)

func TestKindAtOrBelow(t *testing.T) {
	snap := parseConfig(t, testConfig)
	cases := []struct {
		kind, scope string
		want        bool
	}{
		{"organization", "organization", true},
		{"folder", "organization", true},
		{"project", "organization", true},
		{"project", "folder", true},
		{"project", "project", true},
		{"organization", "folder", false},
		{"folder", "project", false},
		{"unknown", "organization", false},
	}
	for _, tc := range cases {
		if got := kindAtOrBelow(snap, tc.kind, tc.scope); got != tc.want {
			t.Errorf("kindAtOrBelow(%q, %q) = %v, want %v", tc.kind, tc.scope, got, tc.want)
		}
	}
}

func TestGrantingPairs(t *testing.T) {
	snap := parseConfig(t, testConfig)
	kinds, roles := grantingPairs(snap, "project:write")
	var pairs []string
	for i := range kinds {
		pairs = append(pairs, kinds[i]+":"+roles[i])
	}
	slices.Sort(pairs)
	want := []string{
		"folder:admin", "folder:developer",
		"organization:admin", "organization:developer",
		"project:admin", "project:developer",
	}
	if !slices.Equal(pairs, want) {
		t.Fatalf("grantingPairs(project:write) = %v, want %v", pairs, want)
	}

	// A permission granted only by "*".
	kinds, roles = grantingPairs(snap, "billing:export")
	if len(kinds) != 3 {
		t.Fatalf("wildcard-only pairs: %v %v", kinds, roles)
	}
	for _, r := range roles {
		if r != "admin" {
			t.Fatalf("expected only admin, got %v", roles)
		}
	}
}

func TestReadResourceSearchValidation(t *testing.T) {
	uid := id.New()
	good := func(body string) ResourceSearch {
		t.Helper()
		obj, err := readObject([]byte(body))
		if err != nil {
			t.Fatalf("readObject: %v", err)
		}
		in, err := readResourceSearch(obj)
		if err != nil {
			t.Fatalf("readResourceSearch(%s): %v", body, err)
		}
		return in
	}
	in := good(fmt.Sprintf(`{
		"subject": {"type": "member", "id": %q},
		"action": {"name": "project:read"},
		"resource": {"type": "project", "id": "ignored"},
		"scope": {"node_id": %q},
		"page": {"limit": 25, "token": %q},
		"context": {"extra": true}
	}`, uid, uid, uid))
	if in.MemberID != uid || in.Permission != "project:read" || in.Kind != "project" ||
		in.ScopeNodeID != uid || in.Limit != 25 || in.After != uid {
		t.Fatalf("parsed %+v", in)
	}

	badCases := []struct {
		name, body, wantSubstr string
	}{
		{"missing subject", `{"action":{"name":"a"},"resource":{"type":"project"}}`, "subject"},
		{"wrong subject type", fmt.Sprintf(`{"subject":{"type":"user","id":%q},"action":{"name":"a"},"resource":{"type":"project"}}`, uid), "subject.type"},
		{"bad subject id", `{"subject":{"type":"member","id":"bad"},"action":{"name":"a"},"resource":{"type":"project"}}`, "subject.id"},
		{"missing resource", fmt.Sprintf(`{"subject":{"type":"member","id":%q},"action":{"name":"a"}}`, uid), "resource"},
		{"empty resource type", fmt.Sprintf(`{"subject":{"type":"member","id":%q},"action":{"name":"a"},"resource":{"type":""}}`, uid), "resource.type"},
		{"missing action", fmt.Sprintf(`{"subject":{"type":"member","id":%q},"resource":{"type":"project"}}`, uid), "action"},
		{"empty action name", fmt.Sprintf(`{"subject":{"type":"member","id":%q},"action":{"name":""},"resource":{"type":"project"}}`, uid), "action.name"},
		{"bad scope id", fmt.Sprintf(`{"subject":{"type":"member","id":%q},"action":{"name":"a"},"resource":{"type":"project"},"scope":{"node_id":"nope"}}`, uid), "scope.node_id"},
		{"limit zero", fmt.Sprintf(`{"subject":{"type":"member","id":%q},"action":{"name":"a"},"resource":{"type":"project"},"page":{"limit":0}}`, uid), "page.limit"},
		{"limit too high", fmt.Sprintf(`{"subject":{"type":"member","id":%q},"action":{"name":"a"},"resource":{"type":"project"},"page":{"limit":1001}}`, uid), "page.limit"},
		{"bad page token", fmt.Sprintf(`{"subject":{"type":"member","id":%q},"action":{"name":"a"},"resource":{"type":"project"},"page":{"token":"not-ulid"}}`, uid), "page.token"},
	}
	for _, tc := range badCases {
		t.Run(tc.name, func(t *testing.T) {
			obj, err := readObject([]byte(tc.body))
			if err != nil {
				t.Fatal(err)
			}
			_, err = readResourceSearch(obj)
			if err == nil || !strings.Contains(err.Error(), tc.wantSubstr) {
				t.Fatalf("err = %v, want substring %q", err, tc.wantSubstr)
			}
		})
	}
}

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
	if _, _, err := e.store.SetStatus(e.ctx, e.userOf("frank"), StatusSuspended); err != nil {
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
		"suspended member":      req(frank, "project", "project:read", org, ""),
		"unknown member":        req(id.New(), "project", "project:read", org, ""),
		"cross-tree scope":      req(erin, "project", "project:read", org, ""),
		"other org scope":       req(bob, "project", "project:read", other, ""),
		"deleted scope":         req(bob, "project", "project:read", fDel, ""),
		"unknown kind":          req(alice, "nope", "project:read", org, ""),
		"upward kind":           req(alice, "organization", "organization:read", f1, ""),
		"ungranted permission":  req(bob, "project", "project:delete", org, ""),
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
