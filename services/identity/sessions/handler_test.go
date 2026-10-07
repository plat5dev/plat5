package sessions

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestValidPayloadOmitsUserID(t *testing.T) {
	raw, err := json.Marshal(validPayload("mem_1", "org_1", nil))
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
	if _, ok := body["scopes"]; !ok || body["scopes"] != nil {
		t.Fatalf("scopes must be present and null: %s", raw)
	}
}

func TestValidPayloadReturnsScopes(t *testing.T) {
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
	scopes, ok := body["scopes"].([]any)
	if !ok || len(scopes) != 1 || scopes[0] != "profile:read" {
		t.Fatalf("scopes: %s", raw)
	}
	empty, err := json.Marshal(validPayload("mem_1", "org_1", []string{}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(empty), `"scopes":[]`) {
		t.Fatalf("empty scopes must be an array: %s", empty)
	}
}
