package orgs

import (
	"strings"

	"github.com/plat5dev/plat5/identity/errors"
)

func (r Role) IsAdminOrOwner() bool {
	return r == RoleAdmin || r == RoleOwner
}

// RequireAdminOrOwner returns Forbidden when actor is not admin/owner.
func RequireAdminOrOwner(actor *Member, permission, resource string, resourceID interface{}) error {
	if actor.Role.IsAdminOrOwner() {
		return nil
	}
	return errors.ForbiddenError(permission, resource, resourceID)
}

// RequireOwner returns Forbidden when actor is not owner.
func RequireOwner(actor *Member, permission, resource string, resourceID interface{}) error {
	if actor.Role == RoleOwner {
		return nil
	}
	return errors.ForbiddenError(permission, resource, resourceID)
}

// ParseRole validates and returns a role. emptyDefault is used when raw is blank
// (pass "" to require an explicit role).
func ParseRole(raw string, emptyDefault Role) (Role, error) {
	role := Role(strings.TrimSpace(raw))
	if role == "" {
		if emptyDefault == "" {
			return "", errors.FieldError("role", "must be member, admin, or owner")
		}
		return emptyDefault, nil
	}
	if !role.Valid() {
		return "", errors.FieldError("role", "must be member, admin, or owner")
	}
	return role, nil
}

// ParseStatus validates and returns a status.
func ParseStatus(raw string) (Status, error) {
	status := Status(strings.TrimSpace(raw))
	if !status.Valid() {
		return "", errors.FieldError("status", "must be active, suspended, or removed")
	}
	return status, nil
}

// CanCreateMember checks whether actor may add a user member with role.
func CanCreateMember(actor *Member, role Role, orgID string) error {
	if err := RequireAdminOrOwner(actor, "member.create", "organization", orgID); err != nil {
		return err
	}
	if role == RoleOwner && actor.Role != RoleOwner {
		return errors.ForbiddenError("member.create_owner", "organization", orgID)
	}
	return nil
}

// ApplyMemberUpdate mutates target for a PATCH. actorUserID is the caller's user id.
func ApplyMemberUpdate(actor, target *Member, actorUserID string, newRole *Role, newStatus *Status, activeOwners int) error {
	memberID := target.ID
	isSelf := target.IsUser(actorUserID)

	if newRole != nil {
		if err := RequireAdminOrOwner(actor, "member.update_role", "member", memberID); err != nil {
			return err
		}
		if target.Role == RoleOwner && actor.Role != RoleOwner {
			return errors.ForbiddenError("member.manage_owner", "member", memberID)
		}
		if target.ServiceAccountID != nil && *newRole == RoleOwner {
			return errors.FieldError("role", "service accounts cannot be owners")
		}
		if *newRole == RoleOwner && actor.Role != RoleOwner {
			return errors.ForbiddenError("member.promote_owner", "member", memberID)
		}
		if target.Role == RoleOwner && *newRole != RoleOwner && activeOwners <= 1 {
			return errors.ValidationFields("Cannot demote the sole owner",
				errors.Field{Path: "role", Message: "sole owner cannot be demoted"})
		}
		target.Role = *newRole
	}

	if newStatus != nil {
		if *newStatus == StatusRemoved && isSelf {
			if target.Role == RoleOwner && activeOwners <= 1 {
				return errors.ValidationFields("Cannot leave as sole owner",
					errors.Field{Path: "status", Message: "transfer ownership first"})
			}
		} else {
			if err := RequireAdminOrOwner(actor, "member.update_status", "member", memberID); err != nil {
				return err
			}
			if target.Role == RoleOwner && actor.Role != RoleOwner {
				return errors.ForbiddenError("member.manage_owner", "member", memberID)
			}
		}
		if target.Role == RoleOwner && *newStatus != StatusActive && target.Status == StatusActive && activeOwners <= 1 {
			return errors.ValidationFields("Cannot change status of sole owner",
				errors.Field{Path: "status", Message: "sole owner must remain active"})
		}
		target.Status = *newStatus
	}
	return nil
}

// ApplyMemberRemove soft-removes target. actorUserID is the caller's user id.
func ApplyMemberRemove(actor, target *Member, actorUserID string, activeOwners int) error {
	memberID := target.ID
	isSelf := target.IsUser(actorUserID)
	if !isSelf {
		if err := RequireAdminOrOwner(actor, "member.remove", "member", memberID); err != nil {
			return err
		}
	}
	if target.Role == RoleOwner && !isSelf && actor.Role != RoleOwner {
		return errors.ForbiddenError("member.manage_owner", "member", memberID)
	}
	if target.Role == RoleOwner && activeOwners <= 1 {
		return errors.ValidationFields("Cannot remove the sole owner",
			errors.Field{Path: "member_id", Message: "transfer ownership first"})
	}
	target.Status = StatusRemoved
	return nil
}

// CanManageMemberKeys: human self or admin/owner for user members; admin/owner only for SA members.
func CanManageMemberKeys(actor, target *Member) error {
	memberID := target.ID
	isAdmin := actor.Role.IsAdminOrOwner()

	if target.Principal() == PrincipalServiceAccount {
		if !isAdmin {
			return errors.ForbiddenError("member_api_key.manage", "member", memberID)
		}
		return nil
	}

	if target.UserID != nil && actor.UserID != nil && *target.UserID == *actor.UserID {
		return nil
	}
	if isAdmin {
		return nil
	}
	return errors.ForbiddenError("member_api_key.manage", "member", memberID)
}
