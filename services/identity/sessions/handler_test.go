package sessions

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestValidPayloadOmitsUserID(t *testing.T) {
	raw, err := json.Marshal(validPayload("mem_1", "org_1", []string{"*"}))
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	if _, ok := body["user_id"]; ok {
		t.Fatalf("validate must not return user_id: %s", raw)
	}
	if body["valid"] != true {
		t.Fatalf("valid: %#v", body["valid"])
	}
	if body["member_id"] != "mem_1" || body["organization_id"] != "org_1" {
		t.Fatalf("ids: %s", raw)
	}
	if labels, ok := body["labels"].([]any); !ok || len(labels) != 1 || labels[0] != "*" {
		t.Fatalf(`labels must be ["*"]: %s`, raw)
	}
}

func TestValidPayloadReturnsLabels(t *testing.T) {
	raw, err := json.Marshal(validPayload("mem_1", "org_1", []string{"profile:read"}))
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	if _, ok := body["user_id"]; ok {
		t.Fatalf("validate must not return user_id: %s", raw)
	}
	labels, ok := body["labels"].([]any)
	if !ok || len(labels) != 1 || labels[0] != "profile:read" {
		t.Fatalf("labels: %s", raw)
	}
	empty, err := json.Marshal(validPayload("mem_1", "org_1", []string{}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(empty), `"labels":[]`) {
		t.Fatalf("empty labels must be an array: %s", empty)
	}
}
