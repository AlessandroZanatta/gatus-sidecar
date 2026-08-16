package discovery

import (
	"strings"
	"unicode"
)

// DisplayName turns a Kubernetes object or namespace name into something
// readable on a status page: "machine-learning" becomes "Machine learning".
//
// Sentence case rather than Title Case is deliberate, because it matches how the
// hand-written config being replaced reads ("Helper backend", not "Helper
// Backend"). Names that already contain uppercase are left alone, so an
// explicitly cased annotation survives and acronyms are not mangled.
func DisplayName(name string) string {
	if name == "" {
		return ""
	}
	if strings.ContainsFunc(name, unicode.IsUpper) {
		return name
	}

	words := strings.FieldsFunc(name, func(r rune) bool {
		return r == '-' || r == '_' || r == '.'
	})
	if len(words) == 0 {
		return name
	}

	out := strings.Join(words, " ")
	runes := []rune(out)
	runes[0] = unicode.ToUpper(runes[0])
	return string(runes)
}
