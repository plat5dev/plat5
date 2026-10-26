package e2e

import (
	"fmt"
	"os"
	"slices"
	"testing"
	"time"
)

type auditEvent struct {
	RequestID string  `json:"request_id"`
	Root      *string `json:"root_id"`
	Nodes     []struct {
		Kind string `json:"kind"`
		ID   string `json:"id"`
	} `json:"nodes"`
	NamedNodeIDs  []string `json:"named_node_ids"`
	NamedMemberID *string  `json:"named_member_id"`
	Actor         struct {
		MemberID  *string `json:"member_id"`
		UserID    *string `json:"user_id"`
		Principal string  `json:"principal"`
		AuthType  string  `json:"auth_type"`
		KeyID     *string `json:"key_id"`
	} `json:"actor"`
	Service   string            `json:"service"`
	Method    string            `json:"method"`
	Route     string            `json:"route"`
	Params    map[string]string `json:"params"`
	Outcome   string            `json:"outcome"`
	Status    *int              `json:"status"`
	Details   map[string]any    `json:"details"`
	CreatedID *string           `json:"created_id"`
}

// events reads a log through the gateway until it holds each wanted
// "METHOD route" and no event in it is pending: outcomes, and refusals, are
// written after the response.
func (s *stack) events(who caller, path string, wants ...string) []auditEvent {
	s.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var page struct {
			AuditEvents []auditEvent `json:"audit_events"`
		}
		s.call(who, "GET", path, nil, 200, &page)
		done := true
		for _, e := range page.AuditEvents {
			done = done && e.Outcome != "pending"
		}
		for _, w := range wants {
			done = done && slices.ContainsFunc(page.AuditEvents, func(e auditEvent) bool { return e.Method+" "+e.Route == w })
		}
		if done || time.Now().After(deadline) {
			return page.AuditEvents
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// find is the one event for method and route in a log, or fails.
func find(t *testing.T, log []auditEvent, method, route string) auditEvent {
	t.Helper()
	var found []auditEvent
	for _, e := range log {
		if e.Method == method && e.Route == route {
			found = append(found, e)
		}
	}
	if len(found) != 1 {
		t.Fatalf("%s %s: %d events in %+v", method, route, len(found), log)
	}
	return found[0]
}

func has(log []auditEvent, method, route string) bool {
	for _, e := range log {
		if e.Method == method && e.Route == route {
			return true
		}
	}
	return false
}

func chain(e auditEvent) string {
	out := ""
	for _, n := range e.Nodes {
		out += "/" + n.Kind + ":" + n.ID
	}
	return out
}

func is(p *string, v string) bool { return p != nil && *p == v }

// An event is in the log of each node in its chain, and of the node it
// created; a person's log holds their user-route events by user id; a user
// route that makes a tree is in that tree's log as its member; refusals are
// recorded and filterable; audit:read decides who reads a log.
func TestAuditLogs(t *testing.T) {
	if os.Getenv("AUDIT_ENABLED") == "false" {
		t.Skip("audit is off")
	}
	s := newStack(t)
	run := fmt.Sprintf("%d", time.Now().UnixNano())
	owner := s.person("e2e-audit-owner-" + run)
	outsider := s.person("e2e-audit-outsider-" + run)

	var user struct {
		ID string `json:"id"`
	}
	s.call(owner, "GET", "/user", nil, 200, &user)
	var org, project, env node
	s.call(owner, "POST", "/orgs", map[string]string{"display_name": "Audit " + run}, 201, &org)
	orgPath := "/orgs/" + org.Slug
	s.call(owner, "POST", orgPath+"/projects", map[string]string{"display_name": "Web"}, 201, &project)
	projectPath := orgPath + "/projects/" + project.Slug
	s.call(owner, "POST", projectPath+"/envs", map[string]string{"display_name": "Development"}, 201, &env)
	envPath := projectPath + "/envs/" + env.Slug
	var held membership
	s.call(owner, "GET", orgPath+"/membership", nil, 200, &held)
	ownerMember := held.MemberID

	var sa struct {
		ID string `json:"id"`
	}
	s.call(owner, "POST", envPath+"/service-accounts", map[string]string{"name": "ci", "role": "developer"}, 201, &sa)
	var saKey key
	s.call(owner, "POST", envPath+"/service-accounts/"+sa.ID+"/keys", map[string]string{}, 201, &saKey)
	bot := caller(saKey.Key)
	s.call(bot, "GET", envPath, nil, 200, nil)
	s.call(bot, "DELETE", projectPath, nil, 403, nil)

	orgChain := "/organization:" + org.ID
	projectChain := orgChain + "/project:" + project.ID
	envChain := projectChain + "/environment:" + env.ID
	const (
		projects = "/orgs/{organization}/projects"
		theProj  = "/orgs/{organization}/projects/{project}"
		theEnv   = "/orgs/{organization}/projects/{project}/envs/{environment}"
		accounts = "/orgs/{organization}/projects/{project}/envs/{environment}/service-accounts"
		keys     = "/orgs/{organization}/projects/{project}/envs/{environment}/service-accounts/{service_account_id}/keys"
	)

	// The org's log.
	orgLog := s.events(owner, orgPath+"/audit-events", "POST /orgs", "DELETE "+theProj)

	// The org's creation, a user route, is in it because identity named the
	// org: its actor is the creator's member there, never a user id.
	created := find(t, orgLog, "POST", "/orgs")
	if created.Root != nil || len(created.Nodes) != 0 || !slices.Equal(created.NamedNodeIDs, []string{org.ID}) ||
		!is(created.NamedMemberID, ownerMember) || !is(created.Actor.MemberID, ownerMember) || created.Actor.UserID != nil ||
		!is(created.CreatedID, org.ID) || created.Outcome != "responded" || *created.Status != 201 ||
		created.Actor.Principal != "person" || created.Actor.AuthType != "jwt" || created.Actor.KeyID != nil {
		t.Fatalf("org create in the org's log: %+v", created)
	}
	projectCreate := find(t, orgLog, "POST", projects)
	if chain(projectCreate) != orgChain || !is(projectCreate.Root, org.ID) || !is(projectCreate.CreatedID, project.ID) ||
		!is(projectCreate.Actor.MemberID, ownerMember) || projectCreate.Actor.UserID != nil {
		t.Fatalf("project create: %+v", projectCreate)
	}
	saCreate := find(t, orgLog, "POST", accounts)
	if chain(saCreate) != envChain || *saCreate.Status != 201 || len(saCreate.Params) != 0 {
		t.Fatalf("service account create: %+v", saCreate)
	}
	keyCreate := find(t, orgLog, "POST", keys)
	if keyCreate.Params["service_account_id"] != sa.ID {
		t.Fatalf("key create's params: %+v", keyCreate)
	}
	// Refused by permission, above its bindings: rejected, as the service
	// account and its key.
	refused := find(t, orgLog, "DELETE", theProj)
	if refused.Outcome != "rejected" || *refused.Status != 403 || chain(refused) != projectChain ||
		!is(refused.Actor.MemberID, sa.ID) || refused.Actor.Principal != "service_account" ||
		refused.Actor.AuthType != "apikey" || !is(refused.Actor.KeyID, saKey.ID) {
		t.Fatalf("permission 403: %+v", refused)
	}
	// Reads aren't audited.
	if has(orgLog, "GET", theEnv) {
		t.Fatalf("a GET was audited: %+v", orgLog)
	}

	// The project's log: each event whose chain holds the project, and the
	// project's own creation; not the org's.
	projectLog := s.events(owner, projectPath+"/audit-events", "DELETE "+theProj)
	for _, want := range []auditEvent{projectCreate, saCreate, keyCreate, refused} {
		if find(t, projectLog, want.Method, want.Route).RequestID != want.RequestID {
			t.Fatalf("%s %s isn't the org log's event", want.Method, want.Route)
		}
	}
	if has(projectLog, "POST", "/orgs") {
		t.Fatalf("the org's creation in the project's log: %+v", projectLog)
	}
	// By id, the same log.
	if byID := s.events(owner, "/orgs/"+org.ID+"/projects/"+project.ID+"/audit-events"); len(byID) != len(projectLog) {
		t.Fatalf("by id: %d events, by slug: %d", len(byID), len(projectLog))
	}

	// The owner's own log: the org's creation, by user id, and nothing from a
	// kind route.
	mine := s.events(owner, "/user/audit-events", "POST /orgs")
	own := find(t, mine, "POST", "/orgs")
	if own.RequestID != created.RequestID || !is(own.Actor.UserID, user.ID) || own.Actor.MemberID != nil ||
		own.Root != nil || len(own.Nodes) != 0 {
		t.Fatalf("org create in the person's log: %+v", own)
	}
	if has(mine, "POST", projects) {
		t.Fatalf("a kind route's event in the person's log: %+v", mine)
	}

	// Filters.
	var page struct {
		AuditEvents []auditEvent `json:"audit_events"`
	}
	s.call(owner, "GET", orgPath+"/audit-events?status=403", nil, 200, &page)
	if len(page.AuditEvents) != 1 || page.AuditEvents[0].RequestID != refused.RequestID {
		t.Fatalf("?status=403: %+v", page)
	}
	s.call(owner, "GET", orgPath+"/audit-events?outcome=rejected&actor_member_id="+sa.ID, nil, 200, &page)
	if len(page.AuditEvents) != 1 || page.AuditEvents[0].RequestID != refused.RequestID {
		t.Fatalf("filtered: %+v", page)
	}
	s.call(owner, "GET", orgPath+"/audit-events?actor_member_id="+ownerMember+"&method=POST", nil, 200, &page)
	if len(page.AuditEvents) != 5 {
		t.Fatalf("the owner's writes, the named create included: %+v", page)
	}
	s.call(owner, "GET", orgPath+"/audit-events?nope=1", nil, 422, nil)

	// Permissions: developer holds no audit:read, and someone outside the org
	// holds nothing there.
	s.call(bot, "GET", envPath+"/audit-events", nil, 403, nil)
	s.call(outsider, "GET", orgPath+"/audit-events", nil, 403, nil)
	if len(s.events(outsider, "/user/audit-events")) != 0 {
		t.Fatal("the outsider's own log isn't empty")
	}
	// A service account isn't admitted on a user route
	// (docs/reference/api/internal.md#admit).
	s.call(bot, "GET", "/user/audit-events", nil, 401, nil)
}
