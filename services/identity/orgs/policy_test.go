package orgs

import (
	"testing"

	"github.com/plat5dev/plat5/identity/errors"
)

func strPtr(s string) *string { return &s }

func member(id, userID string, role Role, status Status) *Member {
	m := &Member{ID: id, Role: role, Status: status}
	if userID != "" {
		m.UserID = strPtr(userID)
	}
	return m
}

func saMember(id, saID string, role Role) *Member {
	return &Member{
		ID:               id,
		ServiceAccountID: strPtr(saID),
		Role:             role,
		Status:           StatusActive,
	}
}

func TestCanCreateMember(t *testing.T) {
	t.Parallel()
	owner := member("a", "u1", RoleOwner, StatusActive)
	admin := member("b", "u2", RoleAdmin, StatusActive)
	mem := member("c", "u3", RoleMember, StatusActive)

	if err := CanCreateMember(owner, RoleOwner, "org"); err != nil {
		t.Fatalf("owner create owner: %v", err)
	}
	if err := CanCreateMember(admin, RoleMember, "org"); err != nil {
		t.Fatalf("admin create member: %v", err)
	}
	if err := CanCreateMember(admin, RoleOwner, "org"); err == nil {
		t.Fatal("admin must not create owner")
	}
	if err := CanCreateMember(mem, RoleMember, "org"); err == nil {
		t.Fatal("member must not create")
	}
}

func TestApplyMemberUpdateSoleOwner(t *testing.T) {
	t.Parallel()
	owner := member("o", "u1", RoleOwner, StatusActive)
	target := member("o", "u1", RoleOwner, StatusActive)
	role := RoleAdmin
	err := ApplyMemberUpdate(owner, target, "u1", &role, nil, 1)
	if err == nil {
		t.Fatal("expected sole owner demote error")
	}
	api, ok := err.(*errors.ApiError)
	if !ok || api.Code != "VALIDATION_ERROR" {
		t.Fatalf("got %#v", err)
	}
}

func TestApplyMemberRemove(t *testing.T) {
	t.Parallel()
	owner := member("o", "u1", RoleOwner, StatusActive)
	target := member("m", "u2", RoleMember, StatusActive)
	if err := ApplyMemberRemove(owner, target, "u1", 1); err != nil {
		t.Fatal(err)
	}
	if target.Status != StatusRemoved {
		t.Fatalf("status=%s", target.Status)
	}

	sole := member("o", "u1", RoleOwner, StatusActive)
	if err := ApplyMemberRemove(sole, sole, "u1", 1); err == nil {
		t.Fatal("sole owner cannot leave")
	}
}

func TestCanManageMemberKeys(t *testing.T) {
	t.Parallel()
	owner := member("o", "u1", RoleOwner, StatusActive)
	memberUser := member("m", "u2", RoleMember, StatusActive)
	sa := saMember("s", "sa1", RoleMember)

	if err := CanManageMemberKeys(memberUser, memberUser); err != nil {
		t.Fatalf("self keys: %v", err)
	}
	if err := CanManageMemberKeys(memberUser, owner); err == nil {
		t.Fatal("member cannot manage owner keys")
	}
	if err := CanManageMemberKeys(owner, sa); err != nil {
		t.Fatalf("owner SA keys: %v", err)
	}
	if err := CanManageMemberKeys(memberUser, sa); err == nil {
		t.Fatal("member cannot manage SA keys")
	}
}

func TestParseRole(t *testing.T) {
	t.Parallel()
	r, err := ParseRole("", RoleMember)
	if err != nil || r != RoleMember {
		t.Fatalf("default: %v %v", r, err)
	}
	if _, err := ParseRole("nope", RoleMember); err == nil {
		t.Fatal("expected invalid")
	}
}
