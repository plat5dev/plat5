package apikey

import "regexp"

const MaxLabelLen = 64

var labelRe = regexp.MustCompile(`^[a-z0-9:._-]+$`)

// ValidLabel is the label hygiene shared by route required_labels and roles.
func ValidLabel(s string) bool {
	return s != "" && len(s) <= MaxLabelLen && labelRe.MatchString(s)
}
