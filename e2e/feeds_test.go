package e2e

import (
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"slices"
	"strings"
)

// event is one event from identity's feeds, as JSON.
type event map[string]any

// feed reads one of identity's event feeds, tree-events or user-events, on its
// internal port as a service would: every event after cursor ("" from the
// first), page by page, and the cursor to read on from.
func (s *stack) feed(name, cursor string) ([]event, string) {
	s.t.Helper()
	key := strings.ReplaceAll(name, "-", "_")
	var all []event
	for {
		url := s.identity + "/internal/" + name + "?limit=100"
		if cursor != "" {
			url += "&starting_after=" + cursor
		}
		resp, err := s.http.Get(url)
		if err != nil {
			s.t.Fatalf("identity's internal port at %s: %v (run the stack with e2e/compose.yml, see README.md)", s.identity, err)
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			s.t.Fatalf("GET %s: %d %s", url, resp.StatusCode, raw)
		}
		var page map[string]json.RawMessage
		var events []event
		var more bool
		if err := json.Unmarshal(raw, &page); err != nil {
			s.t.Fatalf("GET %s: %v: %s", url, err, raw)
		}
		if err := json.Unmarshal(page[key], &events); err != nil || events == nil {
			s.t.Fatalf("GET %s: no %s: %s", url, key, raw)
		}
		_ = json.Unmarshal(page["has_more"], &more)
		all = append(all, events...)
		if len(events) > 0 {
			cursor = events[len(events)-1]["id"].(string)
		}
		if !more {
			return all, cursor
		}
	}
}

// head is a feed's cursor now: where a reader starts to see only what follows.
func (s *stack) head(name string) string {
	s.t.Helper()
	_, cursor := s.feed(name, "")
	return cursor
}

// about is the events whose field is value, in feed order.
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
