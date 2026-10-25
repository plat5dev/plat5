package events

import (
	stderrors "errors"
	"strings"
	"testing"
	"time"

	"github.com/plat5dev/plat5/audit/errors"
)

var testNow = time.Date(2026, 10, 8, 18, 30, 0, 0, time.UTC)

// A kind route's intent, as the gateway sends it.
const validIntent = `{
	"occurred_at": "2026-10-08T18:30:00.123456Z",
	"nodes": [{"kind": "organization", "id": "01j9zx4k7m2p5q8r1s3t6v9w0y"}, {"kind": "project", "id": "01j9zx5a"}],
	"actor": {"member_id": "01j9zx5b8c1d4e7f0g3h6j9k2m", "user_id": null, "principal": "service_account", "auth_type": "apikey", "key_id": "01j9zx5c"},
	"service": "identity",
	"method": "PATCH",
	"route": "/orgs/{organization}/projects/{project}/bindings/{binding_id}",
	"params": {"binding_id": "01j9zx6n2p5q8r1s4t7v0w3x6y"},
	"ip": "203.0.113.7",
	"user_agent": "curl/8.7.1"
}`

// A user route's intent: no nodes, the person's user id.
const userIntent = `{
	"occurred_at": "2026-10-08T18:30:00Z",
	"nodes": [],
	"actor": {"member_id": null, "user_id": "01j9zx7u", "principal": "person", "auth_type": "jwt", "key_id": null},
	"service": "identity",
	"method": "POST",
	"route": "/orgs",
	"params": {},
	"ip": "203.0.113.7",
	"user_agent": null
}`

func fieldPaths(t *testing.T, err error) []string {
	t.Helper()
	var apiErr *errors.ApiError
	if !stderrors.As(err, &apiErr) || apiErr.Status != 422 {
		t.Fatalf("want 422, got %v", err)
	}
	var paths []string
	for _, f := range apiErr.Details.(map[string]interface{})["fields"].([]errors.Field) {
		paths = append(paths, f.Path)
	}
	return paths
}

func TestParseIntent(t *testing.T) {
	in, final, err := ParseIntent("7d6f1c2e-4b0a-4e8e-9a51-3f2b8c1d9e04", []byte(validIntent))
	if err != nil || final != nil {
		t.Fatal(err, final)
	}
	if !in.OccurredAt.Equal(time.Date(2026, 10, 8, 18, 30, 0, 123_000_000, time.UTC)) {
		t.Fatalf("occurred_at is kept to the millisecond: %v", in.OccurredAt)
	}
	if *in.RootID != "01j9zx4k7m2p5q8r1s3t6v9w0y" || in.Nodes[1].Kind != "project" || *in.Actor.MemberID == "" ||
		in.Actor.UserID != nil || *in.Actor.KeyID != "01j9zx5c" || in.Params["binding_id"] == "" || *in.UserAgent != "curl/8.7.1" {
		t.Fatalf("parsed: %+v", in)
	}

	in, _, err = ParseIntent("r", []byte(userIntent))
	if err != nil || in.RootID != nil || len(in.Nodes) != 0 || in.Actor.MemberID != nil || *in.Actor.UserID != "01j9zx7u" ||
		in.Actor.KeyID != nil || in.UserAgent != nil {
		t.Fatalf("a user route's intent: %+v %v", in, err)
	}
}

// Audit stores what it's sent: it doesn't repeat the gateway's rules, and a
// newer gateway's fields are ignored.
func TestParseIntentAcceptsWhatItCanStore(t *testing.T) {
	for name, tc := range map[string]struct{ from, to string }{
		"unknown fields":           {`"ip": "203.0.113.7",`, `"ip": "203.0.113.7", "region": "eu", "actor_v2": {"x": 1},`},
		"far from now":             {`2026-10-08T18:30:00.123456Z`, `2019-01-01T00:00:00Z`},
		"service account jwt":      {`"auth_type": "apikey"`, `"auth_type": "jwt"`},
		"a new principal":          {`"service_account"`, `"robot"`},
		"member and user":          {`"user_id": null`, `"user_id": "01u"`},
		"any method":               {`"PATCH"`, `"PROPFIND"`},
		"param with slash":         {`"01j9zx6n2p5q8r1s4t7v0w3x6y"`, `"a/b"`},
		"long user agent":          {`"curl/8.7.1"`, `"` + strings.Repeat("a", 2000) + `"`},
		"no actor":                 {`"actor": {"member_id": "01j9zx5b8c1d4e7f0g3h6j9k2m", "user_id": null, "principal": "service_account", "auth_type": "apikey", "key_id": "01j9zx5c"},`, ``},
		"no params, no user agent": {`"params": {"binding_id": "01j9zx6n2p5q8r1s4t7v0w3x6y"},`, ``},
	} {
		t.Run(name, func(t *testing.T) {
			in, _, err := ParseIntent("r", []byte(strings.Replace(validIntent, tc.from, tc.to, 1)))
			if err != nil {
				t.Fatal(err)
			}
			if in.Params == nil || in.Nodes == nil {
				t.Fatalf("params and nodes are never null: %+v", in)
			}
		})
	}
}

func TestParseIntentRefusesWhatItCantStore(t *testing.T) {
	cases := map[string]struct {
		requestID string
		from, to  string
		field     string
	}{
		"no request id":  {"", "", "", "request_id"},
		"no occurred_at": {"r", `"occurred_at": "2026-10-08T18:30:00.123456Z",`, ``, "occurred_at"},
		"not a time":     {"r", `2026-10-08T18:30:00.123456Z`, `yesterday`, "occurred_at"},
		"no service":     {"r", `"service": "identity",`, ``, "service"},
		"wrong type":     {"r", `"203.0.113.7"`, `7`, "ip"},
		"nodes a string": {"r", `"nodes": [`, `"nodes": "x", "nodes2": [`, "nodes"},
		"not json":       {"r", `{`, `[`, "body"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			body := validIntent
			if tc.from != "" {
				body = strings.Replace(body, tc.from, tc.to, 1)
			}
			_, _, err := ParseIntent(tc.requestID, []byte(body))
			paths := fieldPaths(t, err)
			if len(paths) != 1 || paths[0] != tc.field {
				t.Fatalf("want %s, got %v", tc.field, paths)
			}
		})
	}
}

// A gateway refusal is the intent plus its outcome, written final.
func TestParseRejectedIntent(t *testing.T) {
	body := strings.Replace(validIntent, `"ip": "203.0.113.7",`, `"ip": "203.0.113.7", "outcome": "rejected", "status": 403,`, 1)
	in, final, err := ParseIntent("r", []byte(body))
	if err != nil || in == nil || final == nil || final.Outcome != OutcomeRejected || *final.Status != 403 {
		t.Fatalf("rejected: %+v %+v %v", in, final, err)
	}
	pending := strings.Replace(validIntent, `"ip": "203.0.113.7",`, `"ip": "203.0.113.7", "outcome": "pending",`, 1)
	if _, final, err := ParseIntent("r", []byte(pending)); err != nil || final != nil {
		t.Fatalf("a pending intent is an intent: %+v %v", final, err)
	}
}

func TestParseOutcome(t *testing.T) {
	ok := map[string]string{
		"responded with details": `{"outcome":"responded","status":200,"details":{"role":{"from":"developer","to":"admin"}}}`,
		"responded null details": `{"outcome":"responded","status":204,"details":null}`,
		"rejected":               `{"outcome":"rejected","status":503,"details":null}`,
		"no response, 502":       `{"outcome":"no_response","status":502,"details":null}`,
		"no response, gone":      `{"outcome":"no_response","status":null,"details":null}`,
		"details on rejected":    `{"outcome":"rejected","status":403,"details":{"a":1}}`,
		"created on rejected":    `{"outcome":"rejected","status":403,"created_id":"x"}`,
		"no status on responded": `{"outcome":"responded"}`,
		"a new outcome":          `{"outcome":"timed_out","status":504}`,
		"unknown fields":         `{"outcome":"responded","status":200,"latency_ms":12}`,
		"big details":            `{"outcome":"responded","status":200,"details":{"a":"` + strings.Repeat("x", 9000) + `"}}`,
	}
	for name, body := range ok {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseOutcome("r", []byte(body)); err != nil {
				t.Fatal(err)
			}
		})
	}

	bad := map[string]struct{ body, field string }{
		"pending":            {`{"outcome":"pending","status":200}`, "outcome"},
		"no outcome":         {`{"status":200}`, "outcome"},
		"status a string":    {`{"outcome":"responded","status":"200"}`, "status"},
		"details not object": {`{"outcome":"responded","status":200,"details":[1]}`, "details"},
		"named not a list":   {`{"outcome":"responded","status":201,"named_nodes":"01o"}`, "named_nodes"},
	}
	for name, tc := range bad {
		t.Run(name, func(t *testing.T) {
			_, err := ParseOutcome("r", []byte(tc.body))
			paths := fieldPaths(t, err)
			if len(paths) != 1 || paths[0] != tc.field {
				t.Fatalf("want %s, got %v", tc.field, paths)
			}
		})
	}
}

func TestParseOutcomeIDs(t *testing.T) {
	out, err := ParseOutcome("r", []byte(`{"outcome":"responded","status":201,"details":null,"created_id":"01o",
		"named_nodes":["01o","01p"],"named_member_id":"01m"}`))
	if err != nil || *out.CreatedID != "01o" || len(out.NamedNodes) != 2 || out.NamedNodes[1] != "01p" || *out.NamedMemberID != "01m" {
		t.Fatalf("ids: %+v %v", out, err)
	}
	out, err = ParseOutcome("r", []byte(`{"outcome":"responded","status":200,"details":null}`))
	if err != nil || out.NamedNodes == nil || len(out.NamedNodes) != 0 || out.NamedMemberID != nil || out.CreatedID != nil {
		t.Fatalf("none named: %+v %v", out, err)
	}
}
