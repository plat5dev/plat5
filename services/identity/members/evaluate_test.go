package members

import (
	"fmt"
	"io"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/plat5dev/plat5/identity/metrics"
	"github.com/plat5dev/plat5/identity/nodes"
)

// POST /access/v1/evaluation and /access/v1/evaluations
// (docs/reference/api/internal.md#access-evaluation): AuthZEN 1.0's shapes,
// decided by admit's resolution.

// evalTree is Gruow with alice its admin, a folder apps with projects web,
// old and gone, and members holding each way of reaching web.
type evalTree struct {
	gruow, apps, web, old, gone, other *nodes.Node
	alice, bob, carol, dana, frank     string // member ids in Gruow
	sa, oldSA, erin, removed           string
}

func evalSetup(t *testing.T) (*env, evalTree) {
	e := setup(t)
	var tr evalTree
	tr.gruow, tr.alice = e.org(e.person("alice"), "Gruow")
	tr.apps = e.child(tr.gruow, "folder", "apps")
	tr.web = e.child(tr.apps, "project", "web")
	tr.old = e.child(tr.apps, "project", "old")
	tr.gone = e.child(tr.apps, "project", "gone")
	tr.other, tr.erin = e.org(e.person("erin"), "Other")

	// bob: developer at the org, which includes member.
	tr.bob = e.join(tr.gruow, "bob", "developer")
	e.bind(tr.old, tr.bob, "admin")
	e.bind(tr.gone, tr.bob, "admin")
	// carol: a guest, viewer on web only.
	carol := e.person("carol")
	if _, err := e.store.Redeem(e.ctx, carol, e.invite(tr.web, "viewer").Token); err != nil {
		t.Fatal(err)
	}
	tr.carol = e.memberOf(tr.gruow, "carol")
	// dana: nobody at the org, viewer on web.
	tr.dana = e.join(tr.gruow, "dana", "nobody")
	e.bind(tr.web, tr.dana, "viewer")
	// frank: developer at the org, then suspended.
	tr.frank = e.join(tr.gruow, "frank", "developer")
	if _, _, err := e.store.SetStatus(e.ctx, e.userOf("frank"), StatusSuspended); err != nil {
		t.Fatal(err)
	}
	// A removed member, once developer at the org.
	tr.removed = e.join(tr.gruow, "gil", "developer")
	if _, err := e.store.RemoveMember(e.ctx, tr.gruow.ID, tr.removed); err != nil {
		t.Fatal(err)
	}
	// A service account that belongs to web with developer there, and one
	// that belongs to old, bound at the org, whose node is then deleted.
	tr.sa, _ = e.serviceAccount(tr.web, "developer")
	tr.oldSA, _ = e.serviceAccount(tr.old, "developer")
	e.bind(tr.gruow, tr.oldSA, "developer")

	if _, err := e.nodes.Delete(e.ctx, e.snap(), tr.old.ID); err != nil {
		t.Fatal(err)
	}
	e.exec(`UPDATE nodes SET status = 'purged' WHERE id = $1`, tr.gone.ID)
	return e, tr
}

// memberOf is the active member of the person behind subject in root.
func (e *env) memberOf(root *nodes.Node, subject string) string {
	e.t.Helper()
	var id string
	if err := e.pool.QueryRow(e.ctx, `
		SELECT m.id FROM members m JOIN idp_links l ON l.principal_id = m.principal_id
		WHERE l.subject = $1 AND m.root_id = $2 AND m.removed_at IS NULL
	`, subject, root.ID).Scan(&id); err != nil {
		e.t.Fatalf("member of %s: %v", subject, err)
	}
	return id
}

func evalBody(member, kind, node, permission string) string {
	return fmt.Sprintf(`{"subject": {"type": "member", "id": %q}, "resource": {"type": %q, "id": %q}, "action": {"name": %q}}`,
		member, kind, node, permission)
}

// decide is POST /access/v1/evaluation's decision; it fails on anything but
// a 200 with a boolean decision.
func (e *env) decide(body string) bool {
	e.t.Helper()
	r := e.callInternal("POST", "/access/v1/evaluation", body)
	d, ok := r.body["decision"].(bool)
	if r.status != 200 || !ok || len(r.body) != 1 {
		e.t.Fatalf("evaluation %s: %d %v", body, r.status, r.body)
	}
	return d
}

// decisions is POST /access/v1/evaluations' evaluations, as booleans.
func (e *env) decisions(body string) []bool {
	e.t.Helper()
	r := e.callInternal("POST", "/access/v1/evaluations", body)
	if r.status != 200 {
		e.t.Fatalf("evaluations %s: %d %v", body, r.status, r.body)
	}
	if _, ok := r.body["decision"]; ok {
		e.t.Fatalf("a batch answer has no top-level decision: %v", r.body)
	}
	out := []bool{}
	for _, it := range r.list("evaluations") {
		d, ok := it["decision"].(bool)
		if !ok || len(it) != 1 {
			e.t.Fatalf("evaluation %v", it)
		}
		out = append(out, d)
	}
	return out
}

func TestEvaluation(t *testing.T) {
	e, tr := evalSetup(t)
	unknown := "01jbx9q3t5vk8m2n4p6r7s8t9w"

	cases := []struct {
		name                           string
		member, kind, node, permission string
		want                           bool
	}{
		// true
		{"* grants every permission", tr.alice, "project", tr.web.ID, "anything:at-all", true},
		{"inherited from the org", tr.bob, "project", tr.web.ID, "project:write", true},
		{"includes resolved", tr.bob, "project", tr.web.ID, "organization:read", true},
		{"on the node itself", tr.dana, "project", tr.web.ID, "project:read", true},
		{"a guest on its node", tr.carol, "project", tr.web.ID, "project:read", true},
		{"a service account in its tree", tr.sa, "project", tr.web.ID, "project:write", true},
		{"at the root", tr.bob, "organization", tr.gruow.ID, "organization:read", true},

		// false
		{"no binding grants it", tr.bob, "project", tr.web.ID, "project:delete", false},
		{"a binding below doesn't reach up", tr.dana, "folder", tr.apps.ID, "project:read", false},
		{"a guest at the root", tr.carol, "organization", tr.gruow.ID, "organization:read", false},
		{"a service account's binding doesn't reach up", tr.sa, "organization", tr.gruow.ID, "project:read", false},
		{"deleted node", tr.bob, "project", tr.old.ID, "project:read", false},
		{"purged node", tr.bob, "project", tr.gone.ID, "project:read", false},
		{"unknown node", tr.alice, "project", unknown, "project:read", false},
		{"resource type isn't the node's kind", tr.bob, "folder", tr.web.ID, "project:read", false},
		{"resource type no kind has", tr.bob, "team", tr.web.ID, "project:read", false},
		{"removed member", tr.removed, "project", tr.web.ID, "project:read", false},
		{"unknown member", unknown, "project", tr.web.ID, "project:read", false},
		{"member of another tree", tr.erin, "project", tr.web.ID, "project:read", false},
		{"admin of another tree", tr.erin, "organization", tr.gruow.ID, "organization:read", false},
		{"suspended person", tr.frank, "project", tr.web.ID, "project:read", false},
		{"service account whose node is deleted", tr.oldSA, "organization", tr.gruow.ID, "organization:read", false},
		{"a member id as a node id", tr.bob, "project", tr.bob, "project:read", false},
		{"a node id as a member id", tr.web.ID, "project", tr.web.ID, "project:read", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := e.decide(evalBody(c.member, c.kind, c.node, c.permission)); got != c.want {
				t.Fatalf("decision %v, want %v", got, c.want)
			}
		})
	}

	// The same answers through the batch endpoint, in one call, in order.
	items := []string{}
	want := []bool{}
	for _, c := range cases {
		items = append(items, evalBody(c.member, c.kind, c.node, c.permission))
		want = append(want, c.want)
	}
	if got := e.decisions(`{"evaluations": [` + strings.Join(items, ",") + `]}`); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("batch %v, want %v", got, want)
	}
}

// An evaluation answers what the gateway would: a deleted ancestor ends the
// chain, even under a node still marked live.
func TestEvaluationDeletedAncestor(t *testing.T) {
	e, tr := evalSetup(t)
	body := evalBody(tr.bob, "project", tr.web.ID, "project:read")
	if !e.decide(body) {
		t.Fatal("bob reaches web")
	}
	e.exec(`UPDATE nodes SET status = 'deleted' WHERE id = $1`, tr.apps.ID)
	if e.decide(body) {
		t.Fatal("a deleted folder above web still grants")
	}
}

// A removed binding is false at once, and the member stays in the tree; a
// role gone from the document, or no longer bindable there, grants nothing.
func TestEvaluationChanges(t *testing.T) {
	e, tr := evalSetup(t)
	body := evalBody(tr.dana, "project", tr.web.ID, "project:read")
	if !e.decide(body) {
		t.Fatal("dana reaches web")
	}
	if _, err := e.store.DeleteBinding(e.ctx, tr.web.ID, e.bindingOf(tr.dana, tr.web.ID)); err != nil {
		t.Fatal(err)
	}
	if e.decide(body) || e.removed(tr.dana) {
		t.Fatalf("after the binding's removal: decision true, or dana removed (%v)", e.removed(tr.dana))
	}

	carol := evalBody(tr.carol, "project", tr.web.ID, "project:read")
	cfg := strings.Replace(testConfig, `"viewer": { "permissions": ["project:read"], "bindable_at": ["project"] }`,
		`"viewer": { "permissions": ["project:read"], "bindable_at": ["organization"] }`, 1)
	if cfg == testConfig {
		t.Fatal("config unchanged")
	}
	e.src.Store(parseConfig(t, cfg))
	if e.decide(carol) {
		t.Fatal("a role no longer bindable at project still grants there")
	}
	e.src.Store(parseConfig(t, testConfig))
	if !e.decide(carol) {
		t.Fatal("the role grants again once it's bindable again")
	}
}

// Top-level subject, action, resource and context are defaults; a key in an
// item replaces the default whole. No items is one evaluation.
func TestEvaluationsDefaults(t *testing.T) {
	e, tr := evalSetup(t)
	subject := func(m string) string { return fmt.Sprintf(`{"type": "member", "id": %q}`, m) }
	resource := func(kind, id string) string { return fmt.Sprintf(`{"type": %q, "id": %q}`, kind, id) }
	action := func(name string) string { return fmt.Sprintf(`{"name": %q}`, name) }

	body := fmt.Sprintf(`{
		"subject": %s, "action": %s, "resource": %s, "context": {"time": "2026-10-27T12:00:00Z"},
		"evaluations": [
			{},
			{"action": %s},
			{"subject": %s},
			{"resource": %s},
			{"subject": %s, "action": %s, "resource": %s, "context": "ignored"},
			{"subject": %s}
		]
	}`,
		subject(tr.bob), action("project:write"), resource("project", tr.web.ID),
		action("project:delete"),
		subject(tr.carol),
		resource("organization", tr.gruow.ID),
		subject(tr.alice), action("anything"), resource("folder", tr.apps.ID),
		subject(tr.dana))
	want := []bool{
		true,  // bob, project:write on web: all defaults
		false, // the action overridden: project:delete
		false, // carol, a viewer, can't write
		true,  // the resource overridden: bob's developer at the org grants project:write there too
		true,  // alice's own subject, action and resource
		false, // dana is a viewer: no project:write
	}
	if got := e.decisions(body); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("got %v, want %v", got, want)
	}

	// Items may carry everything, with no defaults at all.
	if got := e.decisions(`{"evaluations": [` + evalBody(tr.bob, "project", tr.web.ID, "project:read") + `]}`); fmt.Sprint(got) != "[true]" {
		t.Fatalf("no defaults: %v", got)
	}

	// No items, or an empty list: one evaluation of the top level, answered
	// as one decision.
	for _, items := range []string{``, `, "evaluations": []`, `, "evaluations": null`} {
		single := strings.TrimSuffix(evalBody(tr.bob, "project", tr.web.ID, "project:write"), "}") + items + "}"
		r := e.callInternal("POST", "/access/v1/evaluations", single)
		if r.status != 200 || r.body["decision"] != true || r.body["evaluations"] != nil {
			t.Fatalf("%q: %d %v", items, r.status, r.body)
		}
	}

	// The single endpoint ignores evaluations: it reads the top level.
	r := e.callInternal("POST", "/access/v1/evaluation",
		strings.TrimSuffix(evalBody(tr.bob, "project", tr.web.ID, "project:write"), "}")+`, "evaluations": [{"action": {"name": "x"}}]}`)
	if r.status != 200 || r.body["decision"] != true {
		t.Fatalf("single with evaluations: %d %v", r.status, r.body)
	}
}

// options.evaluations_semantic, as AuthZEN 1.0 defines it: every item, items
// up to the first false, or items up to the first true.
func TestEvaluationsSemantics(t *testing.T) {
	e, tr := evalSetup(t)
	// bob on web: true, false, true, false.
	items := []string{
		evalBody(tr.bob, "project", tr.web.ID, "project:read"),
		evalBody(tr.bob, "project", tr.web.ID, "project:delete"),
		evalBody(tr.bob, "project", tr.web.ID, "project:write"),
		evalBody(tr.bob, "project", tr.old.ID, "project:read"),
	}
	batch := func(options string, items ...string) string {
		return `{` + options + `"evaluations": [` + strings.Join(items, ",") + `]}`
	}
	cases := []struct {
		options string
		items   []string
		want    string
	}{
		{``, items, "[true false true false]"},
		{`"options": {},`, items, "[true false true false]"},
		{`"options": {"evaluations_semantic": null},`, items, "[true false true false]"},
		{`"options": {"evaluations_semantic": "execute_all", "another_option": 1},`, items, "[true false true false]"},
		{`"options": {"evaluations_semantic": "deny_on_first_deny"},`, items, "[true false]"},
		{`"options": {"evaluations_semantic": "deny_on_first_deny"},`, []string{items[0], items[2]}, "[true true]"},
		{`"options": {"evaluations_semantic": "permit_on_first_permit"},`, items, "[true]"},
		{`"options": {"evaluations_semantic": "permit_on_first_permit"},`, []string{items[1], items[3], items[2], items[0]}, "[false false true]"},
		{`"options": {"evaluations_semantic": "permit_on_first_permit"},`, []string{items[1], items[3]}, "[false false]"},
	}
	for _, c := range cases {
		if got := e.decisions(batch(c.options, c.items...)); fmt.Sprint(got) != c.want {
			t.Fatalf("%s: got %v, want %s", c.options, got, c.want)
		}
	}
}

// At most 100 items, and the same node or member many times over is still
// answered item by item.
func TestEvaluationsSize(t *testing.T) {
	e, tr := evalSetup(t)
	items := make([]string, MaxEvaluations)
	want := make([]bool, MaxEvaluations)
	for i := range items {
		switch i % 3 {
		case 0:
			items[i], want[i] = evalBody(tr.bob, "project", tr.web.ID, "project:read"), true
		case 1:
			items[i], want[i] = evalBody(tr.carol, "project", tr.web.ID, "project:write"), false
		default:
			items[i], want[i] = evalBody(tr.dana, "project", tr.web.ID, "project:read"), true
		}
	}
	if got := e.decisions(`{"evaluations": [` + strings.Join(items, ",") + `]}`); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("100 items: %v", got)
	}
	over := append(items, items[0])
	r := e.callInternal("POST", "/access/v1/evaluations", `{"evaluations": [`+strings.Join(over, ",")+`]}`)
	if r.status != 400 || r.code() != "INVALID_REQUEST" || r.message() != "evaluations holds at most 100 items." {
		t.Fatalf("101 items: %d %v", r.status, r.body)
	}
}

// Only a body identity can't read is an error: a 400 in the error envelope,
// its message naming the field.
func TestEvaluationMalformed(t *testing.T) {
	e, tr := evalSetup(t)
	ok := evalBody(tr.bob, "project", tr.web.ID, "project:read")
	member := fmt.Sprintf(`{"type": "member", "id": %q}`, tr.bob)
	resource := fmt.Sprintf(`{"type": "project", "id": %q}`, tr.web.ID)
	action := `{"name": "project:read"}`
	with := func(subject, resource, action string) string {
		parts := []string{}
		if subject != "" {
			parts = append(parts, `"subject": `+subject)
		}
		if resource != "" {
			parts = append(parts, `"resource": `+resource)
		}
		if action != "" {
			parts = append(parts, `"action": `+action)
		}
		return `{` + strings.Join(parts, ", ") + `}`
	}

	single := []struct {
		name, body, message string
	}{
		{"not JSON", `{`, "The body must be a JSON object."},
		{"empty body", ``, "The body must be a JSON object."},
		{"an array", `[` + ok + `]`, "The body must be a JSON object."},
		{"null", `null`, "The body must be a JSON object."},
		{"no subject", with("", resource, action), "subject is required."},
		{"null subject", with("null", resource, action), "subject is required."},
		{"no resource", with(member, "", action), "resource is required."},
		{"no action", with(member, resource, ""), "action is required."},
		{"subject a string", with(`"bob"`, resource, action), "subject must be an object."},
		{"subject type user", with(fmt.Sprintf(`{"type": "user", "id": %q}`, tr.bob), resource, action), `subject.type must be "member".`},
		{"subject type missing", with(fmt.Sprintf(`{"id": %q}`, tr.bob), resource, action), `subject.type must be "member".`},
		{"subject type a number", with(`{"type": 1, "id": "x"}`, resource, action), "subject.type must be a string."},
		{"subject id missing", with(`{"type": "member"}`, resource, action), "subject.id must be a member id."},
		{"subject id malformed", with(`{"type": "member", "id": "bob"}`, resource, action), "subject.id must be a member id."},
		{"subject id uppercase", with(fmt.Sprintf(`{"type": "member", "id": %q}`, strings.ToUpper(tr.bob)), resource, action), "subject.id must be a member id."},
		{"subject id a number", with(`{"type": "member", "id": 7}`, resource, action), "subject.id must be a string."},
		{"resource type empty", with(member, fmt.Sprintf(`{"type": "", "id": %q}`, tr.web.ID), action), "resource.type must be a kind name."},
		{"resource id malformed", with(member, `{"type": "project", "id": "web"}`, action), "resource.id must be a node id."},
		{"resource an array", with(member, `[]`, action), "resource must be an object."},
		{"action name empty", with(member, resource, `{"name": ""}`), "action.name must be a permission."},
		{"action name missing", with(member, resource, `{}`), "action.name must be a permission."},
		{"action name a list", with(member, resource, `{"name": ["project:read"]}`), "action.name must be a string."},
	}
	for _, c := range single {
		t.Run(c.name, func(t *testing.T) {
			for _, path := range []string{"/access/v1/evaluation", "/access/v1/evaluations"} {
				r := e.callInternal("POST", path, c.body)
				if r.status != 400 || r.code() != "INVALID_REQUEST" || r.message() != c.message {
					t.Fatalf("%s: %d %v, want 400 %q", path, r.status, r.body, c.message)
				}
			}
		})
	}

	batch := []struct {
		name, body, message string
	}{
		{"evaluations an object", `{"evaluations": {}}`, "evaluations must be an array."},
		{"an item not an object", `{"evaluations": [` + ok + `, 3]}`, "evaluations[1] must be an object."},
		{"an item lacking a subject, no default", `{"action": ` + action + `, "evaluations": [{"resource": ` + resource + `}]}`,
			"evaluations[0] needs a subject, in the item or at the top level."},
		{"an item lacking a resource, no default", `{"subject": ` + member + `, "evaluations": [` + ok + `, {"action": ` + action + `}]}`,
			"evaluations[1] needs a resource, in the item or at the top level."},
		{"an item's subject malformed", `{"evaluations": [` + ok + `, ` + ok + `, ` + with(`{"type": "member", "id": "nope"}`, resource, action) + `]}`,
			"evaluations[2].subject.id must be a member id."},
		{"an item's subject of another type", `{"evaluations": [` + with(`{"type": "user", "id": "u"}`, resource, action) + `]}`,
			`evaluations[0].subject.type must be "member".`},
		{"a malformed default, even if overridden", `{"subject": {"type": "user", "id": "u"}, "evaluations": [` + ok + `]}`,
			`subject.type must be "member".`},
		{"unknown semantic", `{"options": {"evaluations_semantic": "first"}, "evaluations": [` + ok + `]}`,
			"options.evaluations_semantic must be execute_all, deny_on_first_deny or permit_on_first_permit."},
		{"options not an object", `{"options": "execute_all", "evaluations": [` + ok + `]}`, "options must be an object."},
		{"semantic not a string", `{"options": {"evaluations_semantic": true}, "evaluations": [` + ok + `]}`, "options.evaluations_semantic must be a string."},
	}
	for _, c := range batch {
		t.Run(c.name, func(t *testing.T) {
			r := e.callInternal("POST", "/access/v1/evaluations", c.body)
			if r.status != 400 || r.code() != "INVALID_REQUEST" || r.message() != c.message {
				t.Fatalf("%d %v, want 400 %q", r.status, r.body, c.message)
			}
		})
	}

	// What AuthZEN lets a PEP send and Plat5 doesn't read is ignored:
	// properties, context of any shape, unknown keys.
	extra := fmt.Sprintf(`{
		"subject": {"type": "member", "id": %q, "properties": {"department": "Sales"}},
		"resource": {"type": "project", "id": %q, "properties": {"library_record": {"title": "x"}}},
		"action": {"name": "project:read", "properties": {"method": "GET"}},
		"context": "anything", "unknown": [1, 2]
	}`, tr.bob, tr.web.ID)
	if !e.decide(extra) {
		t.Fatal("extra fields changed the answer")
	}
}

// AuthZEN asks that a request id sent comes back, errors included.
func TestEvaluationRequestID(t *testing.T) {
	e, tr := evalSetup(t)
	for _, c := range []struct {
		path, body string
		status     int
	}{
		{"/access/v1/evaluation", evalBody(tr.bob, "project", tr.web.ID, "project:read"), 200},
		{"/access/v1/evaluations", `{"evaluations": [` + evalBody(tr.bob, "project", tr.web.ID, "project:read") + `]}`, 200},
		{"/access/v1/evaluation", `{}`, 400},
	} {
		req := httptest.NewRequest("POST", c.path, strings.NewReader(c.body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Request-ID", "bfe9eb29-ab87-4ca3-be83-a1d5d8305716")
		resp, err := e.internal.Test(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != c.status || resp.Header.Get("X-Request-ID") != "bfe9eb29-ab87-4ca3-be83-a1d5d8305716" {
			t.Fatalf("%s: %d, X-Request-ID %q", c.path, resp.StatusCode, resp.Header.Get("X-Request-ID"))
		}
	}
}

// access_evaluations_total counts the decisions answered, by decision; a
// batch that stops early counts only what it answered.
func TestEvaluationMetrics(t *testing.T) {
	e, tr := evalSetup(t)
	before := scrapeEvaluations(t)
	e.decide(evalBody(tr.bob, "project", tr.web.ID, "project:read"))
	e.decisions(`{"options": {"evaluations_semantic": "deny_on_first_deny"}, "evaluations": [` +
		evalBody(tr.bob, "project", tr.web.ID, "project:delete") + `,` +
		evalBody(tr.bob, "project", tr.web.ID, "project:read") + `]}`)
	after := scrapeEvaluations(t)
	if after["true"]-before["true"] != 1 || after["false"]-before["false"] != 1 {
		t.Fatalf("before %v, after %v", before, after)
	}
}

var evaluationsSeries = regexp.MustCompile(`(?m)^access_evaluations_total\{decision="(true|false)"\} (\S+)$`)

// scrapeEvaluations reads access_evaluations_total from /metrics, as a
// scraper would.
func scrapeEvaluations(t *testing.T) map[string]float64 {
	t.Helper()
	rec := httptest.NewRecorder()
	metrics.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	raw, _ := io.ReadAll(rec.Body)
	out := map[string]float64{}
	for _, m := range evaluationsSeries.FindAllStringSubmatch(string(raw), -1) {
		v, err := strconv.ParseFloat(m[2], 64)
		if err != nil {
			t.Fatal(err)
		}
		out[m[1]] = v
	}
	return out
}
