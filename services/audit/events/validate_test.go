package events

import (
	stderrors "errors"
	"strings"
	"testing"
	"time"

	"github.com/plat5dev/plat5/audit/errors"
)

var testNow = time.Date(2026, 10, 8, 18, 30, 0, 0, time.UTC)

const validIntent = `{
	"occurred_at": "2026-10-08T18:30:00.123456Z",
	"organization_id": "01J9ZX4K7M2P5Q8R1S3T6V9W0Y",
	"actor": {"member_id": "01J9ZX5B8C1D4E7F0G3H6J9K2M", "auth_type": "member_session", "key_prefix": "plat5-ms-1-x9Qa"},
	"service": "identity",
	"method": "PATCH",
	"route": "/org/members/{member_id}",
	"params": {"member_id": "01J9ZX6N2P5Q8R1S4T7V0W3X6Y"},
	"ip": "203.0.113.7",
	"user_agent": "curl/8.7.1"
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
	in, err := ParseIntent("7d6f1c2e-4b0a-4e8e-9a51-3f2b8c1d9e04", []byte(validIntent), testNow)
	if err != nil {
		t.Fatal(err)
	}
	if !in.OccurredAt.Equal(time.Date(2026, 10, 8, 18, 30, 0, 123_000_000, time.UTC)) {
		t.Fatalf("occurred_at is kept to the millisecond: %v", in.OccurredAt)
	}
	if in.Actor.AuthType != AuthMemberSession || in.Params["member_id"] == "" || *in.UserAgent != "curl/8.7.1" {
		t.Fatalf("parsed: %+v", in)
	}
}

func TestParseIntentDefaults(t *testing.T) {
	body := strings.Replace(validIntent, `"user_agent": "curl/8.7.1"`, `"user_agent": null`, 1)
	body = strings.Replace(body, `"params": {"member_id": "01J9ZX6N2P5Q8R1S4T7V0W3X6Y"},`, ``, 1)
	in, err := ParseIntent("req-1", []byte(body), testNow)
	if err != nil {
		t.Fatal(err)
	}
	if in.UserAgent != nil || in.Params == nil || len(in.Params) != 0 {
		t.Fatalf("absent user agent is null and absent params are {}: %+v", in)
	}
}

func TestParseIntentRejects(t *testing.T) {
	cases := map[string]struct {
		requestID string
		from, to  string
		field     string
	}{
		"request id":         {"bad id!", "", "", "request_id"},
		"missing org":        {"r", `"organization_id": "01J9ZX4K7M2P5Q8R1S3T6V9W0Y",`, ``, "organization_id"},
		"user auth type":     {"r", `"member_session"`, `"user_apikey"`, "actor.auth_type"},
		"unaudited method":   {"r", `"PATCH"`, `"OPTIONS"`, "method"},
		"relative route":     {"r", `"/org/members/{member_id}"`, `"org/members"`, "route"},
		"param with slash":   {"r", `"01J9ZX6N2P5Q8R1S4T7V0W3X6Y"`, `"a/b"`, "params.member_id"},
		"clock far behind":   {"r", `2026-10-08T18:30:00.123456Z`, `2026-10-06T18:30:00Z`, "occurred_at"},
		"not a time":         {"r", `2026-10-08T18:30:00.123456Z`, `yesterday`, "occurred_at"},
		"user agent too big": {"r", `"curl/8.7.1"`, `"` + strings.Repeat("a", MaxUserAgentLen+1) + `"`, "user_agent"},
		"wrong type":         {"r", `"203.0.113.7"`, `7`, "ip"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			body := validIntent
			if tc.from != "" {
				body = strings.Replace(body, tc.from, tc.to, 1)
			}
			_, err := ParseIntent(tc.requestID, []byte(body), testNow)
			paths := fieldPaths(t, err)
			if len(paths) != 1 || paths[0] != tc.field {
				t.Fatalf("want %s, got %v", tc.field, paths)
			}
		})
	}
}

func TestParseOutcome(t *testing.T) {
	ok := map[string]string{
		"responded with details": `{"outcome":"responded","status":200,"details":{"role":{"from":"developer","to":"admin"}}}`,
		"responded null details": `{"outcome":"responded","status":204,"details":null}`,
		"rejected":               `{"outcome":"rejected","status":403}`,
		"no response, 502":       `{"outcome":"no_response","status":502}`,
		"no response, gone":      `{"outcome":"no_response","status":null}`,
		"rejected, gone":         `{"outcome":"rejected","status":null}`,
	}
	for name, body := range ok {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseOutcome("r", []byte(body)); err != nil {
				t.Fatal(err)
			}
		})
	}

	bad := map[string]struct{ body, field string }{
		"pending":              {`{"outcome":"pending","status":200}`, "outcome"},
		"no status":            {`{"outcome":"responded"}`, "status"},
		"status out of range":  {`{"outcome":"rejected","status":42}`, "status"},
		"details on rejected":  {`{"outcome":"rejected","status":403,"details":{"a":1}}`, "details"},
		"details not object":   {`{"outcome":"responded","status":200,"details":[1]}`, "details"},
		"details over the cap": {`{"outcome":"responded","status":200,"details":{"a":"` + strings.Repeat("x", MaxDetailsBytes) + `"}}`, "details"},
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

const userIntent = `{
	"occurred_at": "2026-10-08T18:30:00Z",
	"user_id": "01J9ZX4K7M2P5Q8R1S3T6V9W0Z",
	"actor": {"auth_type": "jwt"},
	"service": "identity",
	"method": "POST",
	"route": "/user/organizations",
	"ip": "203.0.113.7"
}`

func TestParseUserIntent(t *testing.T) {
	in, err := ParseIntent("r", []byte(userIntent), testNow)
	if err != nil {
		t.Fatal(err)
	}
	if in.UserID == nil || in.OrganizationID != nil || in.Actor.MemberID != nil || in.Actor.KeyPrefix != nil {
		t.Fatalf("a person's event: %+v", in)
	}
	key := strings.Replace(userIntent, `{"auth_type": "jwt"}`, `{"auth_type": "user_apikey", "key_prefix": "plat5-k-2-AbC9"}`, 1)
	in, err = ParseIntent("r", []byte(key), testNow)
	if err != nil || in.Actor.KeyPrefix == nil {
		t.Fatalf("user key: %+v %v", in, err)
	}

	bad := map[string]struct{ from, to, field string }{
		"both owners":       {`"user_id"`, `"organization_id": "o", "user_id"`, "organization_id"},
		"member credential": {`{"auth_type": "jwt"}`, `{"auth_type": "member_apikey", "member_id": "m", "key_prefix": "p"}`, "actor.auth_type"},
		"member on jwt":     {`{"auth_type": "jwt"}`, `{"auth_type": "jwt", "member_id": "m"}`, "actor.member_id"},
		"prefix on jwt":     {`{"auth_type": "jwt"}`, `{"auth_type": "jwt", "key_prefix": "p"}`, "actor.key_prefix"},
		"user key, no pfx":  {`{"auth_type": "jwt"}`, `{"auth_type": "user_apikey"}`, "actor.key_prefix"},
	}
	for name, tc := range bad {
		t.Run(name, func(t *testing.T) {
			_, err := ParseIntent("r", []byte(strings.Replace(userIntent, tc.from, tc.to, 1)), testNow)
			paths := fieldPaths(t, err)
			if len(paths) != 1 || paths[0] != tc.field {
				t.Fatalf("want %s, got %v", tc.field, paths)
			}
		})
	}
}

func TestParseOutcomeIDs(t *testing.T) {
	out, err := ParseOutcome("r", []byte(`{"outcome":"responded","status":201,"created_id":"o1","organization_id":"o1","member_id":"m1"}`))
	if err != nil || *out.CreatedID != "o1" || out.Named == nil || out.Named.MemberID != "m1" {
		t.Fatalf("ids: %+v %v", out, err)
	}
	bad := map[string]struct{ body, field string }{
		"org without member":  {`{"outcome":"responded","status":201,"organization_id":"o1"}`, "member_id"},
		"created on rejected": {`{"outcome":"rejected","status":403,"created_id":"x"}`, "outcome"},
		"created not ascii":   {`{"outcome":"responded","status":201,"created_id":"a b"}`, "created_id"},
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
