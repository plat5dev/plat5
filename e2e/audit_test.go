package e2e

import (
	"fmt"
	"os"
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
	NamedRootID   *string `json:"named_root_id"`
	NamedMemberID *string `json:"named_member_id"`
	Actor         struct {
		MemberID  string  `json:"member_id"`
		Principal string  `json:"principal"`
		AuthType  string  `json:"auth_type"`
		KeyPrefix *string `json:"key_prefix"`
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

// events reads a log through the gateway until no event in it is pending:
// outcomes are written after the response.
func (s *stack) events(who caller, path string) []auditEvent {
	s.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var page struct {
			AuditEvents []auditEvent `json:"audit_events"`
		}
		s.call(who, "GET", path, nil, 200, &page)
		pending := false
		for _, e := range page.AuditEvents {
			pending = pending || e.Outcome == "pending"
		}
		if !pending || time.Now().After(deadline) {
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

// Each org's log holds its tree's events with their chain; each read by node
// returns that node and below; a person's log holds what they did on sign-in
// routes; the labels on the generated audit routes decide who reads them.
func TestAuditLogsPerRoot(t *testing.T) {
	if os.Getenv("AUDIT_ENABLED") == "false" {
		t.Skip("audit is off")
	}
	s := newStack(t)
	run := fmt.Sprintf("%d", time.Now().UnixNano())
	owner := s.person("e2e-audit-owner-" + run)
	outsider := s.person("e2e-audit-outsider-" + run)

	var org, project node
	s.call(owner, "POST", "/orgs", map[string]string{"label": "Audit " + run}, 201, &org)
	s.call(owner, "POST", "/orgs/"+org.Slug+"/projects", map[string]string{"label": "Web"}, 201, &project)
	orgPath := "/orgs/" + org.Slug
	projectPath := orgPath + "/projects/" + project.Slug

	var sa struct {
		ID string `json:"id"`
	}
	s.call(owner, "POST", projectPath+"/service-accounts", map[string]string{"name": "ci", "role": "developer"}, 201, &sa)
	var key struct {
		Key       string `json:"key"`
		KeyPrefix string `json:"key_prefix"`
	}
	s.call(owner, "POST", projectPath+"/service-accounts/"+sa.ID+"/keys", map[string]string{}, 201, &key)
	bot := apiKey(key.Key)
	s.call(bot, "GET", projectPath, nil, 200, nil)
	s.call(bot, "DELETE", projectPath, nil, 403, nil)

	orgChain := "/organization:" + org.ID
	projectChain := orgChain + "/project:" + project.ID

	// The org's log.
	orgLog := s.events(owner, orgPath+"/audit-events")
	created := find(t, orgLog, "POST", "/orgs")
	if created.Root != nil || len(created.Nodes) != 0 || created.NamedRootID == nil || *created.NamedRootID != org.ID ||
		created.CreatedID == nil || *created.CreatedID != org.ID || created.Outcome != "responded" {
		t.Fatalf("org create in the org's log: %+v", created)
	}
	// There the actor is the owner's member in the org, which the service named.
	orgMember := *created.NamedMemberID
	if created.Actor.MemberID != orgMember || created.Details["member_id"] != orgMember || created.Actor.Principal != "person" {
		t.Fatalf("org create's actor: %+v", created)
	}
	projectCreate := find(t, orgLog, "POST", "/orgs/{organization}/projects")
	if chain(projectCreate) != orgChain || *projectCreate.Root != org.ID || *projectCreate.CreatedID != project.ID ||
		projectCreate.Actor.MemberID != orgMember || projectCreate.Actor.AuthType != "jwt" || projectCreate.Actor.KeyPrefix != nil {
		t.Fatalf("project create: %+v", projectCreate)
	}
	saCreate := find(t, orgLog, "POST", "/orgs/{organization}/projects/{project}/service-accounts")
	if chain(saCreate) != projectChain || *saCreate.Status != 201 || len(saCreate.Params) != 0 {
		t.Fatalf("service account create: %+v", saCreate)
	}
	keyCreate := find(t, orgLog, "POST", "/orgs/{organization}/projects/{project}/service-accounts/{service_account}/keys")
	if keyCreate.Params["service_account"] != sa.ID {
		t.Fatalf("key create's params: %+v", keyCreate)
	}
	// Refused by label: rejected, as the service account and its key.
	refused := find(t, orgLog, "DELETE", "/orgs/{organization}/projects/{project}")
	if refused.Outcome != "rejected" || *refused.Status != 403 || chain(refused) != projectChain ||
		refused.Actor.MemberID != sa.ID || refused.Actor.Principal != "service_account" || refused.Actor.AuthType != "apikey" ||
		refused.Actor.KeyPrefix == nil || *refused.Actor.KeyPrefix != key.KeyPrefix {
		t.Fatalf("label 403: %+v", refused)
	}
	// Reads aren't audited.
	if has(orgLog, "GET", "/orgs/{organization}/projects/{project}") {
		t.Fatalf("a GET was audited: %+v", orgLog)
	}

	// The project's log: the project and below, not the org's own events.
	projectLog := s.events(owner, projectPath+"/audit-events")
	for _, want := range []auditEvent{saCreate, keyCreate, refused} {
		if find(t, projectLog, want.Method, want.Route).RequestID != want.RequestID {
			t.Fatalf("%s %s isn't the org log's event", want.Method, want.Route)
		}
	}
	if has(projectLog, "POST", "/orgs") || has(projectLog, "POST", "/orgs/{organization}/projects") {
		t.Fatalf("org events in the project's log: %+v", projectLog)
	}
	// By id, the same log.
	if byID := s.events(owner, "/orgs/"+org.ID+"/projects/"+project.ID+"/audit-events"); len(byID) != len(projectLog) {
		t.Fatalf("by id: %d events, by slug: %d", len(byID), len(projectLog))
	}

	// The owner's own log: the org create, with their directory member, and
	// nothing from a kind route.
	mine := s.events(owner, "/me/audit-events")
	own := find(t, mine, "POST", "/orgs")
	if own.RequestID != created.RequestID || own.Actor.MemberID == orgMember || own.Actor.MemberID == "" ||
		*own.NamedRootID != org.ID || *own.NamedMemberID != orgMember {
		t.Fatalf("org create in the person's log: %+v", own)
	}
	if has(mine, "POST", "/orgs/{organization}/projects") {
		t.Fatalf("a kind route's event in the person's log: %+v", mine)
	}

	// Filters.
	var page struct {
		AuditEvents []auditEvent `json:"audit_events"`
	}
	s.call(owner, "GET", orgPath+"/audit-events?outcome=rejected&actor_member_id="+sa.ID, nil, 200, &page)
	if len(page.AuditEvents) != 1 || page.AuditEvents[0].RequestID != refused.RequestID {
		t.Fatalf("filtered: %+v", page)
	}
	s.call(owner, "GET", orgPath+"/audit-events?actor_member_id="+own.Actor.MemberID, nil, 200, &page)
	if len(page.AuditEvents) != 0 {
		t.Fatalf("the directory member in the org's log: %+v", page)
	}
	s.call(owner, "GET", orgPath+"/audit-events?nope=1", nil, 422, nil)

	// Labels: developer holds no project:audit:read, and someone outside the
	// org holds nothing there.
	s.call(bot, "GET", projectPath+"/audit-events", nil, 403, nil)
	s.call(outsider, "GET", orgPath+"/audit-events", nil, 403, nil)
	if len(s.events(outsider, "/me/audit-events")) != 0 {
		t.Fatal("the outsider's own log isn't empty")
	}
	// A service account has no directory member, so its key isn't valid on
	// a sign-in route (reference/api/internal.md#admit).
	s.call(bot, "GET", "/me/audit-events", nil, 401, nil)
}
