package orgs

import (
	"testing"

	"github.com/plat5dev/plat5/identity/errors"
)

func strPtr(s string) *string { return &s }

func TestParsePatchStatus(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{"active", "suspended", " active "} {
		if _, err := ParsePatchStatus(raw); err != nil {
			t.Fatalf("%q: %v", raw, err)
		}
	}
	for _, raw := range []string{"", "removed", "owner"} {
		err := mustErr(t, raw)
		api, ok := err.(*errors.ApiError)
		if !ok || api.Code != "VALIDATION_ERROR" {
			t.Fatalf("%q: %#v", raw, err)
		}
	}
}

func mustErr(t *testing.T, raw string) error {
	t.Helper()
	_, err := ParsePatchStatus(raw)
	if err == nil {
		t.Fatalf("%q: expected error", raw)
	}
	return err
}

func TestRejectLastMember(t *testing.T) {
	t.Parallel()
	if err := rejectLastMember(2, "member_id"); err != nil {
		t.Fatal(err)
	}
	err := rejectLastMember(1, "member_id")
	api, ok := err.(*errors.ApiError)
	if !ok || api.Code != "VALIDATION_ERROR" || api.Message != lastMemberMessage {
		t.Fatalf("got %#v", err)
	}
	if err := rejectLastMember(0, "service_account_id"); err == nil {
		t.Fatal("expected last-member error")
	}
}
