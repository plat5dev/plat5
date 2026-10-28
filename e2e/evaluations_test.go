package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"
)

// evaluate posts body to identity's internal port, as a service would, and
// decodes the 200 into into.
func (s *stack) evaluate(path string, body any, into any) {
	s.t.Helper()
	b, _ := json.Marshal(body)
	req, err := http.NewRequest("POST", s.identity+path, bytes.NewReader(b))
	if err != nil {
		s.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.http.Do(req)
	if err != nil {
		s.t.Fatalf("identity's internal port at %s: %v (run the stack with e2e/compose.yml, see README.md)", s.identity, err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		s.t.Fatalf("POST %s: %d %s", path, resp.StatusCode, out)
	}
	if err := json.Unmarshal(out, into); err != nil {
		s.t.Fatalf("POST %s: %v: %s", path, err, out)
	}
}

func subject(memberID string) map[string]string {
	return map[string]string{"type": "member", "id": memberID}
}

func resource(n node) map[string]string {
	return map[string]string{"type": n.Kind, "id": n.ID}
}

func action(permission string) map[string]string {
	return map[string]string{"name": permission}
}

// decide is one evaluation's decision.
func (s *stack) decide(memberID string, n node, permission string) bool {
	s.t.Helper()
	var out struct {
		Decision *bool `json:"decision"`
	}
	s.evaluate("/access/v1/evaluation", map[string]any{
		"subject": subject(memberID), "resource": resource(n), "action": action(permission),
	}, &out)
	if out.Decision == nil {
		s.t.Fatalf("no decision for %s %s on %s", memberID, permission, n.ID)
	}
	return *out.Decision
}

// A service checking access outside a request: a member bound at the org with
// a role lacking a project permission and at a project with one granting it;
// the project binding removed while the member stays in the tree; the project
// deleted; two members in one batch. Identity answers on its internal port,
// with no cache, so nothing here waits on the gateway's.
func TestEvaluations(t *testing.T) {
	s := newStack(t)
	run := fmt.Sprintf("%d", time.Now().UnixNano())
	owner := s.person("e2e-eval-owner-" + run)
	pat := s.person("e2e-eval-pat-" + run)

	var org, project node
	s.call(owner, "POST", "/orgs", map[string]string{"display_name": "Evaluations " + run}, 201, &org)
	orgPath := "/orgs/" + org.Slug
	s.call(owner, "POST", orgPath+"/projects", map[string]string{"display_name": "Tasks"}, 201, &project)
	projectPath := orgPath + "/projects/" + project.Slug
	var ownerM membership
	s.call(owner, "GET", orgPath+"/membership", nil, 200, &ownerM)

	// Pat joins the org as member (organization:read, no project:read), and
	// is bound viewer at the project (project:read).
	var invite struct {
		Token string `json:"token"`
	}
	s.call(owner, "POST", orgPath+"/invites", map[string]string{"role": "member"}, 201, &invite)
	var redeemed struct {
		MemberID string `json:"member_id"`
	}
	s.call(pat, "POST", "/user/invites/redeem", map[string]string{"token": invite.Token}, 200, &redeemed)
	patM := redeemed.MemberID
	var viewer binding
	s.call(owner, "POST", projectPath+"/bindings", map[string]string{"member_id": patM, "role": "viewer"}, 201, &viewer)

	// 1. The project binding grants project:read there; the org's role doesn't.
	if !s.decide(patM, project, "project:read") {
		t.Fatal("pat's viewer binding at the project grants project:read there")
	}
	if s.decide(patM, org, "project:read") {
		t.Fatal("pat's member role at the org grants no project:read")
	}
	if !s.decide(patM, org, "organization:read") {
		t.Fatal("pat's member role at the org grants organization:read")
	}

	// 2. Remove the project binding: false at once, and pat stays in the tree.
	s.call(owner, "DELETE", projectPath+"/bindings/"+viewer.ID, nil, 204, nil)
	if s.decide(patM, project, "project:read") {
		t.Fatal("project:read after the binding's removal")
	}
	var patMember struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	s.call(owner, "GET", orgPath+"/members/"+patM, nil, 200, &patMember)
	if patMember.ID != patM || patMember.Status != "active" || !s.decide(patM, org, "organization:read") {
		t.Fatalf("pat left the tree: %+v", patMember)
	}

	// 3. Delete the project: false, even for the org's admin.
	if !s.decide(ownerM.MemberID, project, "project:read") {
		t.Fatal("the org's admin reaches the project")
	}
	s.call(owner, "DELETE", projectPath, nil, 204, nil)
	if s.decide(ownerM.MemberID, project, "project:read") {
		t.Fatal("project:read on a deleted project")
	}

	// 4. Two members in one call: the action and resource as defaults.
	var batch struct {
		Evaluations []struct {
			Decision bool `json:"decision"`
		} `json:"evaluations"`
	}
	s.evaluate("/access/v1/evaluations", map[string]any{
		"resource": resource(org),
		"action":   action("project:write"),
		"evaluations": []map[string]any{
			{"subject": subject(ownerM.MemberID)},
			{"subject": subject(patM)},
			{"subject": subject(patM), "action": action("organization:read")},
		},
	}, &batch)
	got := []bool{}
	for _, e := range batch.Evaluations {
		got = append(got, e.Decision)
	}
	if !slices.Equal(got, []bool{true, false, true}) {
		t.Fatalf("batch: %v", got)
	}

	// A body identity can't read is a 400 in the error envelope.
	req, _ := http.NewRequest("POST", s.identity+"/access/v1/evaluation",
		strings.NewReader(`{"subject": {"type": "user", "id": "x"}, "resource": {"type": "organization", "id": "`+org.ID+`"}, "action": {"name": "organization:read"}}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.http.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var bad apiError
	_ = json.NewDecoder(resp.Body).Decode(&bad)
	if resp.StatusCode != 400 || bad.Error.Code != "INVALID_REQUEST" || bad.Error.Message != `subject.type must be "member".` {
		t.Fatalf("subject of type user: %d %+v", resp.StatusCode, bad)
	}
}
