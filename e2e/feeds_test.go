package e2e

import (
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptrace"
	"slices"
	"testing"
	"time"
)

// event is one event from identity's lifecycle log, as JSON.
type event map[string]any

// longPoll outlasts identity's 25 second wait.
var longPoll = &http.Client{Timeout: 40 * time.Second}

// read is one GET of identity's lifecycle log on its internal port, as a
// service reads it. query is the query string, without "?". A 204 is no
// events.
func (s *stack) read(client *http.Client, query string) (int, []event, bool) {
	s.t.Helper()
	url := s.identity + "/internal/events?" + query
	resp, err := client.Get(url)
	if err != nil {
		s.t.Fatalf("identity's internal port at %s: %v (run the stack with e2e/compose.yml, see README.md)", s.identity, err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return s.page(url, resp.StatusCode, raw)
}

func (s *stack) page(url string, status int, raw []byte) (int, []event, bool) {
	s.t.Helper()
	switch status {
	case http.StatusNoContent:
		return status, nil, false
	case http.StatusOK:
	default:
		s.t.Fatalf("GET %s: %d %s", url, status, raw)
	}
	var page struct {
		Events  []event `json:"events"`
		HasMore *bool   `json:"has_more"`
	}
	if err := json.Unmarshal(raw, &page); err != nil || page.Events == nil || page.HasMore == nil {
		s.t.Fatalf("GET %s: not a page: %v: %s", url, err, raw)
	}
	return status, page.Events, *page.HasMore
}

// feed is every event after cursor ("" from the first), page by page, and the
// cursor to read on from. With nothing past cursor yet, the first read waits
// for an event.
func (s *stack) feed(cursor string) ([]event, string) {
	s.t.Helper()
	var all []event
	for {
		query := "limit=100"
		if cursor != "" {
			query += "&after=" + cursor
		}
		_, events, more := s.read(longPoll, query)
		all = append(all, events...)
		if len(events) > 0 {
			cursor = events[len(events)-1]["id"].(string)
		}
		if !more {
			return all, cursor
		}
	}
}

// head is the log's cursor now, "" when it's empty: where a reader starts to
// see only what follows. Read from the first event, which never waits.
func (s *stack) head() string {
	s.t.Helper()
	_, events, more := s.read(s.http, "limit=100")
	cursor := ""
	for {
		if len(events) > 0 {
			cursor = events[len(events)-1]["id"].(string)
		}
		if !more {
			return cursor
		}
		_, events, more = s.read(s.http, "limit=100&after="+cursor)
	}
}

// readWhile reads the log with query, and does write once the request is on
// the wire, so the read is waiting when write's events land. It returns the
// read's answer and how long it took.
func (s *stack) readWhile(query string, write func()) (int, []event, time.Duration) {
	s.t.Helper()
	url := s.identity + "/internal/events?" + query
	sent := make(chan struct{})
	req, _ := http.NewRequest("GET", url, nil)
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{
		WroteRequest: func(httptrace.WroteRequestInfo) { close(sent) },
	}))
	type answer struct {
		status int
		raw    []byte
		err    error
	}
	answered := make(chan answer, 1)
	start := time.Now()
	go func() {
		resp, err := longPoll.Do(req)
		if err != nil {
			answered <- answer{err: err}
			return
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		answered <- answer{status: resp.StatusCode, raw: raw}
	}()
	<-sent
	write()
	a := <-answered
	took := time.Since(start)
	if a.err != nil {
		s.t.Fatalf("GET %s: %v", url, a.err)
	}
	status, events, _ := s.page(url, a.status, a.raw)
	return status, events, took
}

// about is the events whose field is value, in log order.
func about(events []event, field, value string) []event {
	var out []event
	for _, e := range events {
		if e[field] == value {
			out = append(out, e)
		}
	}
	return out
}

// fields is an event's keys, sorted.
func (e event) fields() []string {
	return slices.Sorted(maps.Keys(e))
}

// The fields each event carries, and no others.
var (
	nodeEventFields   = []string{"id", "kind", "node_id", "occurred_at", "root_id", "type"}
	memberEventFields = []string{"id", "member_id", "occurred_at", "principal", "root_id", "type"}
	userEventFields   = []string{"id", "occurred_at", "type", "user_id"}
)

// A waiting read returns the next event as it lands, and types picks what's
// answered and what ends a wait.
func TestEventLogWaitsAndTypes(t *testing.T) {
	s := newStack(t)
	run := fmt.Sprintf("%d", time.Now().UnixNano())
	owner := s.person("e2e-log-owner-" + run)
	var me struct {
		ID string `json:"id"`
	}
	s.call(owner, "GET", "/user", nil, 200, &me)
	at := s.head()
	if at == "" {
		t.Fatal("the log is empty after a first sign-in")
	}

	// A read waiting past the head returns the org's node.created once it
	// lands, long before the wait ends.
	var org node
	status, woke, took := s.readWhile("after="+at+"&types=node.created", func() {
		s.call(owner, "POST", "/orgs", map[string]string{"display_name": "Log " + run}, 201, &org)
	})
	if status != 200 || len(woke) != 1 || woke[0]["node_id"] != org.ID || woke[0]["kind"] != "organization" {
		t.Fatalf("the waiting read: %d %v", status, woke)
	}
	if took > 10*time.Second {
		t.Fatalf("the waiting read took %v", took)
	}

	// types: the org's creation wrote node.created and member.created; with
	// member.created only, only that one.
	_, members, _ := s.read(s.http, "after="+at+"&types=member.created,user.deleted")
	if len(members) == 0 {
		t.Fatal("no member.created")
	}
	for _, e := range members {
		if e["type"] != "member.created" && e["type"] != "user.deleted" {
			t.Fatalf("types answered %v", e)
		}
	}
	if mine := about(members, "root_id", org.ID); len(mine) != 1 || !slices.Equal(mine[0].fields(), memberEventFields) {
		t.Fatalf("the org's member.created: %v", mine)
	}

	// An event of another type doesn't end a wait for user.deleted; the
	// account's deletion does.
	status, deleted, _ := s.readWhile("after="+s.head()+"&types=user.deleted", func() {
		var project node
		s.call(owner, "POST", "/orgs/"+org.Slug+"/projects", map[string]string{"display_name": "Log"}, 201, &project)
		s.call(owner, "DELETE", "/user", nil, 204, nil)
	})
	if status != 200 || len(deleted) != 1 || deleted[0]["type"] != "user.deleted" || deleted[0]["user_id"] != me.ID {
		t.Fatalf("waiting for user.deleted: %d %v", status, deleted)
	}

	// An after past the head answers at once, with no events.
	if status, events, more := s.read(s.http, "after=999999999999"); status != 200 || len(events) != 0 || more {
		t.Fatalf("past the head: %d %v %v", status, events, more)
	}
}

// A bad param is a 422 naming it, and the two feeds the log replaced are gone.
func TestEventLogRefuses(t *testing.T) {
	s := newStack(t)
	for _, c := range []struct{ query, param string }{
		{"types=node.moved", "types"},
		{"types=node.purged,", "types"},
		{"after=0", "after"},
		{"after=abc", "after"},
		{"limit=0", "limit"},
	} {
		resp, err := s.http.Get(s.identity + "/internal/events?" + c.query)
		if err != nil {
			t.Fatal(err)
		}
		var body struct {
			Error struct {
				Code    string `json:"code"`
				Details struct {
					Fields []struct {
						Path string `json:"path"`
					} `json:"fields"`
				} `json:"details"`
			} `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&body)
		resp.Body.Close()
		if resp.StatusCode != 422 || body.Error.Code != "VALIDATION_ERROR" || len(body.Error.Details.Fields) != 1 || body.Error.Details.Fields[0].Path != c.param {
			t.Fatalf("%s: %d %+v", c.query, resp.StatusCode, body)
		}
	}
	for _, path := range []string{"/internal/tree-events", "/internal/user-events"} {
		resp, err := s.http.Get(s.identity + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 404 {
			t.Fatalf("%s: %d", path, resp.StatusCode)
		}
	}
}
