package nodes

import (
	"regexp"
	"strconv"
	"strings"
)

// Slug rules are fixed, not config (docs/concepts/model.md#ids-and-slugs).
const (
	minSlugLen = 3
	maxSlugLen = 63
	// maxBaseLen leaves room for a -NN collision suffix.
	maxBaseLen = 59
)

var slugRe = regexp.MustCompile(`^[a-z][a-z0-9-]{1,61}[a-z0-9]$`)

// ValidSlug is the slug shape: lowercase letters, digits and hyphens, starting
// with a letter, not ending with a hyphen, 3 to 63 characters. Never an id,
// since ids contain _.
func ValidSlug(s string) bool {
	return slugRe.MatchString(s)
}

// baseSlug is the slug generated from a label: lowercased, each run of other
// characters as -, trimmed. Prefixed with the kind name when it would start
// with a digit or be too short, and filled to the minimum length.
func baseSlug(label, kind string) string {
	s := slugify(label)
	if s == "" || s[0] < 'a' || s[0] > 'z' || len(s) < minSlugLen {
		s = strings.Trim(slugify(kind)+"-"+s, "-")
	}
	for len(s) < minSlugLen {
		s += "0"
	}
	return trimTo(s, maxBaseLen)
}

// candidate is the nth slug to try for base: base, base-2, base-3, …
func candidate(base string, n int) string {
	if n <= 1 {
		return base
	}
	suffix := "-" + strconv.Itoa(n)
	return trimTo(base, maxSlugLen-len(suffix)) + suffix
}

func slugify(v string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(v) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			dash = false
			continue
		}
		if !dash && b.Len() > 0 {
			b.WriteByte('-')
			dash = true
		}
	}
	return strings.TrimRight(b.String(), "-")
}

func trimTo(s string, n int) string {
	if len(s) > n {
		s = s[:n]
	}
	return strings.TrimRight(s, "-")
}
