package sessions

import (
	"encoding/json"
	"testing"
)

func TestValidPayloadOmitsUserID(t *testing.T) {
	raw, err := json.Marshal(validPayload("mem_1", "org_1"))
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
