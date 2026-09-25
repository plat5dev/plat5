package orgs

import (
	"strings"

	"github.com/plat5dev/plat5/identity/errors"
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
