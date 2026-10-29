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

// routeChecksPath is route_checks.path at its default: the e2e config sets none.
const routeChecksPath = "/access/routes"

type check struct {
	Method string `json:"method"`
	Path   string `json:"path"`
}

// routeChecks sends checks to the gateway's route checks as who, requires a
// 200, and returns each `allowed`, in order.
func (s *stack) routeChecks(who caller, checks ...check) []bool {
	s.t.Helper()
	var out struct {
		Checks []struct {
			Allowed *bool `json:"allowed"`
		} `json:"checks"`
	}
	s.call(who, "POST", routeChecksPath, map[string]any{"checks": checks}, 200, &out)
	if len(out.Checks) != len(checks) {
		s.t.Fatalf("route checks: %d answers for %d checks", len(out.Checks), len(checks))
	}
	got := make([]bool, len(checks))
	for i, c := range out.Checks {
		if c.Allowed == nil {
			s.t.Fatalf("route checks: no allowed at %d", i)
		}
		got[i] = *c.Allowed
	}
	return got
}

func want(t *testing.T, who string, got []bool, expected ...bool) {
	t.Helper()
	if !slices.Equal(got, expected) {
		t.Fatalf("%s: got %v, want %v", who, got, expected)
	}
}

// Route checks (docs/reference/gateway.md#route-checks), for people, a guest,
// a service account and someone outside the tree, against identity's routes
// and a probe service's project and public routes. The probe service is the
// e2e config plus one service, applied here and taken out again at the end;
// route checks never call it.
func TestRouteChecks(t *testing.T) {
	s := newStack(t)
	run := fmt.Sprintf("%d", time.Now().UnixNano())

	config := s.config()
	config["services"].(map[string]any)["probe"] = map[string]any{
		"url": "http://probe:3000",
		"scopes": map[string]any{
			"project": map[string]any{
				"routes": []any{map[string]any{
					"path": "/tasks", "upstream": "/projects/{project.id}/tasks",
					"required_permissions": []string{"project:read"}, "methods": []string{"GET"},
				}},
			},
			"public": map[string]any{
				"routes": []any{map[string]any{"path": "/status", "methods": []string{"GET"}}},
			},
		},
	}
	s.apply(config)
	t.Cleanup(func() { s.apply(s.config()) })

	owner := s.person("e2e-checks-owner-" + run)
	pat := s.person("e2e-checks-pat-" + run)
	guest := s.person("e2e-checks-guest-" + run)
	outsider := s.person("e2e-checks-outsider-" + run)

	// The gateway follows the apply within a second or so: wait for the public route.
	status := check{"GET", "/status"}
	deadline := time.Now().Add(30 * time.Second)
	for !s.routeChecks(outsider, status)[0] {
		if time.Now().After(deadline) {
			t.Fatal("the probe service's public route never reached the gateway")
		}
		time.Sleep(500 * time.Millisecond)
	}

	var org, project, env node
	s.call(owner, "POST", "/orgs", map[string]string{"display_name": "Checks " + run}, 201, &org)
	orgPath := "/orgs/" + org.Slug
	s.call(owner, "POST", orgPath+"/projects", map[string]string{"display_name": "Web"}, 201, &project)
	projectPath := orgPath + "/projects/" + project.Slug
	s.call(owner, "POST", projectPath+"/envs", map[string]string{"display_name": "Prod"}, 201, &env)
	envPath := projectPath + "/envs/" + env.Slug

	// Pat: a project-only member. In the org as member (organization:read and
	// nothing at projects), viewer at the project.
	var invite struct {
		Token string `json:"token"`
	}
	var redeemed struct {
		MemberID string `json:"member_id"`
	}
	s.call(owner, "POST", orgPath+"/invites", map[string]string{"role": "member"}, 201, &invite)
	s.call(pat, "POST", "/user/invites/redeem", map[string]string{"token": invite.Token}, 200, &redeemed)
	s.call(owner, "POST", projectPath+"/bindings", map[string]string{"member_id": redeemed.MemberID, "role": "viewer"}, 201, nil)

	// A guest: invited at the project only.
	s.call(owner, "POST", projectPath+"/invites", map[string]string{"role": "viewer"}, 201, &invite)
	s.call(guest, "POST", "/user/invites/redeem", map[string]string{"token": invite.Token}, 200, nil)

	// A service account at the environment.
	var sa struct {
		ID string `json:"id"`
	}
	var saKey key
	s.call(owner, "POST", envPath+"/service-accounts", map[string]string{"name": "ci", "role": "developer"}, 201, &sa)
	s.call(owner, "POST", envPath+"/service-accounts/"+sa.ID+"/keys", map[string]string{}, 201, &saKey)
	bot := caller(saKey.Key)
	settle()

	tasks := check{"GET", projectPath + "/tasks"}
	orgBindings := check{"GET", orgPath + "/bindings"}
	orgNode := check{"GET", orgPath}
	projects := check{"GET", orgPath + "/projects"}
	deleteProject := check{"DELETE", projectPath}
	me := check{"GET", "/user"}
	unknown := check{"GET", "/nowhere/" + run}
	wrongMethod := check{"PUT", orgPath}

	want(t, "owner", s.routeChecks(owner, tasks, orgBindings, orgNode, deleteProject, status, me, unknown, wrongMethod),
		true, true, true, true, true, true, false, false)
	want(t, "project-only member", s.routeChecks(pat, tasks, orgBindings, orgNode, projects, deleteProject),
		true, false, true, true, false)
	want(t, "guest", s.routeChecks(guest, tasks, orgNode, orgBindings, projects),
		true, false, false, true)
	want(t, "outsider", s.routeChecks(outsider, tasks, orgNode, projects, status, me),
		false, false, false, true, true)
	want(t, "service account", s.routeChecks(bot, me, check{"GET", envPath}, check{"DELETE", envPath}, tasks, status),
		false, true, false, false, true)

	// Each answer is what the request gets.
	s.call(pat, "GET", projectPath+"/bindings", nil, 403, nil)
	s.call(pat, "GET", orgPath+"/bindings", nil, 403, nil)
	s.call(guest, "GET", orgPath, nil, 403, nil)
	s.call(bot, "GET", "/user", nil, 401, nil)

	// A revoked key is a 401 with only user checks too, where a valid service
	// account's key answers false.
	var revoked key
	s.call(owner, "POST", envPath+"/service-accounts/"+sa.ID+"/keys", map[string]string{}, 201, &revoked)
	s.call(owner, "DELETE", envPath+"/service-accounts/"+sa.ID+"/keys/"+revoked.ID, nil, 204, nil)
	settle()
	if code, body, _ := s.try(caller(revoked.Key), "POST", routeChecksPath, map[string]any{"checks": []check{me}}); code != 401 {
		t.Fatalf("a revoked key, user checks only: %d %s", code, body)
	}
	want(t, "service account, user checks only", s.routeChecks(bot, me), false)

	// A deleted project: refused, except its restore, as allow_deleted admits it.
	s.call(owner, "DELETE", projectPath, nil, 204, nil)
	settle()
	restore := check{"POST", projectPath + "/restore"}
	want(t, "owner, deleted project", s.routeChecks(owner, tasks, check{"GET", projectPath}, restore), false, false, true)
	want(t, "project-only member, deleted project", s.routeChecks(pat, tasks, restore), false, false)

	// The call's own errors.
	code, body, err := s.try(owner, "POST", routeChecksPath, map[string]any{"checks": []check{}})
	var bad apiError
	_ = json.Unmarshal(body, &bad)
	if err != nil || code != 422 || bad.Error.Code != "VALIDATION_ERROR" {
		t.Fatalf("no checks: %v %d %s", err, code, body)
	}
	many := make([]check, 101)
	for i := range many {
		many[i] = orgNode
	}
	if code, body, _ := s.try(owner, "POST", routeChecksPath, map[string]any{"checks": many}); code != 422 {
		t.Fatalf("101 checks: %d %s", code, body)
	}
	code, body, _ = s.try(owner, "POST", routeChecksPath, map[string]any{"checks": []check{{"get", orgPath}, {"GET", orgPath + "?x=1"}}})
	bad = apiError{}
	_ = json.Unmarshal(body, &bad)
	fields, _ := bad.Error.Details["fields"].([]any)
	if code != 422 || len(fields) != 2 || !strings.Contains(string(body), `"checks[1].path"`) {
		t.Fatalf("a bad method and a query string: %d %s", code, body)
	}
	if code, body, _ := s.try(owner, "GET", routeChecksPath, nil); code != 404 {
		t.Fatalf("GET on the route checks path: %d %s", code, body)
	}

	// No credential is a 401, and so is a key identity refuses.
	req, _ := http.NewRequest("POST", s.gateway+routeChecksPath,
		bytes.NewReader([]byte(`{"checks":[{"method":"GET","path":"/status"}]}`)))
	resp, err := s.http.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	out, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("no credential: %d %s", resp.StatusCode, out)
	}
	if code, body, _ := s.try("plat5-k-2-nosuchkey", "POST", routeChecksPath, map[string]any{"checks": []check{orgNode}}); code != 401 {
		t.Fatalf("an unknown key: %d %s", code, body)
	}

	// Rate-limit headers, as on a route.
	req, _ = http.NewRequest("POST", s.gateway+routeChecksPath,
		bytes.NewReader([]byte(`{"checks":[{"method":"GET","path":"/status"}]}`)))
	req.Header.Set("Authorization", "Bearer "+string(owner))
	resp, err = s.http.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("X-RateLimit-Limit") != "100" || resp.Header.Get("X-Request-ID") == "" {
		t.Fatalf("headers: %d %v", resp.StatusCode, resp.Header)
	}
}
