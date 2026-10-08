package orgs

import (
	"strings"

	"github.com/plat5dev/plat5/identity/errors"
	"github.com/plat5dev/plat5/identity/roles"
)

const lastMemberMessage = "Delete the organization instead of its last member."

// ParsePatchStatus accepts active or suspended. removed is DELETE, not a patch.
func ParsePatchStatus(raw string) (Status, error) {
	status := Status(strings.TrimSpace(raw))
	switch status {
	case StatusActive, StatusSuspended:
		return status, nil
	default:
		return "", errors.FieldError("status", "Status must be active or suspended.")
	}
}

// LastMemberError is 422. A remove must leave one non-removed member.
func LastMemberError(path string) error {
	return errors.ValidationFields(lastMemberMessage,
		errors.Field{Path: path, Message: lastMemberMessage})
}

func rejectLastMember(nonRemoved int, path string) error {
	if nonRemoved <= 1 {
		return LastMemberError(path)
	}
	return nil
}

// LastCreatorError is 422. The org keeps at least one member with creator_role.
func LastCreatorError(creator, path string) error {
	msg := "Keep at least one member with the " + creator + " role."
	return errors.ValidationFields(msg, errors.Field{Path: path, Message: msg})
}

// rejectLastCreator refuses a write that takes the org's non-removed members
// holding creator_role from one to zero. The target in members is already
// changed; prior is the target as it was. Roles off: nothing to keep.
func rejectLastCreator(set *roles.Set, members []*Member, prior Member, path string) error {
	creator := set.Creator()
	if creator == nil {
		return nil
	}
	if holdsRole(&prior, *creator) && countHolders(members, *creator) == 0 {
		return LastCreatorError(*creator, path)
	}
	return nil
}

func holdsRole(m *Member, role string) bool {
	return m.Status != StatusRemoved && m.Role != nil && *m.Role == role
}

func countHolders(members []*Member, role string) int {
	n := 0
	for _, m := range members {
		if holdsRole(m, role) {
			n++
		}
	}
	return n
}
