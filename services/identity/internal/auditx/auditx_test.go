package auditx

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestChangedOnlyRecordsDifferences(t *testing.T) {
	d := Details{}
	d.Changed("status", "active", "active")
	d.Changed("name", "ci", "deploy")
	if len(d) != 1 {
		t.Fatalf("only the changed field: %v", d)
	}

	admin, dev, dev2 := "admin", "developer", "developer"
	d = Details{}
	d.ChangedPtr("role", &dev, &dev2)
	if len(d) != 0 {
		t.Fatalf("equal values are no change: %v", d)
	}
	d.ChangedPtr("role", &dev, &admin)
	got, _ := Encode(d)
	if got != `{"role":{"from":"developer","to":"admin"}}` {
		t.Fatalf("encoded: %s", got)
	}

	d = Details{}
	d.ChangedPtr("role", nil, nil)
	d.ChangedPtr("other", nil, &admin)
	got, _ = Encode(d)
	if got != `{"other":{"from":null,"to":"admin"}}` {
		t.Fatalf("null to a value is a change: %s", got)
	}
}

func TestEncodeIsASCIIAndTheSameJSON(t *testing.T) {
	d := Details{"name": Change{From: "Café", To: "Ünïcode 🚀"}}
	got, err := Encode(d)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < len(got); i++ {
		if got[i] < 0x20 || got[i] > 0x7e {
			t.Fatalf("byte %d is not visible ASCII: %q", i, got)
		}
	}
	var back map[string]map[string]string
	if err := json.Unmarshal([]byte(got), &back); err != nil {
		t.Fatal(err)
	}
	if back["name"]["from"] != "Café" || back["name"]["to"] != "Ünïcode 🚀" {
		t.Fatalf("round trip: %v", back)
	}
}

func TestEncodeRefusesOverTheCap(t *testing.T) {
	if _, err := Encode(Details{"name": strings.Repeat("x", MaxBytes)}); err == nil {
		t.Fatal("over the cap must not be sent")
	}
}
