package apikey

import "regexp"

const MaxLabelLen = 64

var labelRe = regexp.MustCompile(`^[a-z0-9:._-]+$`)

// ScopesRefused is the 422 for a key mint that sends `scopes`. A key carries its
// owner's permissions. Refusing the field keeps an old client from believing it
// got a narrower key than it did.
const ScopesRefused = "Keys carry their owner's permissions and can't be narrowed."

// ValidLabel is the label hygiene shared by route required_labels and roles.
func ValidLabel(s string) bool {
	return s != "" && len(s) <= MaxLabelLen && labelRe.MatchString(s)
}

// WireLabels is nil for every label, so JSON encodes null (not omitted).
func WireLabels(labels []string) *[]string {
	if labels == nil {
		return nil
	}
	cp := labels
	return &cp
}

// WireLabelsJSON is for fiber.Map so every label is JSON null, not omitted.
func WireLabelsJSON(labels []string) any {
	if labels == nil {
		return nil
	}
	return labels
}
