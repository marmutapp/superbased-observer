package sqlitedsn

import (
	"path/filepath"
	"strings"
)

// Escape returns path in the form expected inside a SQLite "file:"
// URI: separators normalized via filepath.ToSlash, with the three
// characters the URI parser treats specially percent-encoded — "?"
// (starts the query string), "#" (starts the ignored fragment), and
// "%" (introduces an escape the parser decodes). Ordinary paths pass
// through byte-identical, so existing DSNs are unchanged.
//
// The caller supplies the "file:" prefix and query string:
//
//	dsn := "file:" + sqlitedsn.Escape(path) + "?mode=ro"
func Escape(path string) string {
	p := filepath.ToSlash(path)
	if !strings.ContainsAny(p, "%?#") {
		return p
	}
	var b strings.Builder
	b.Grow(len(p) + 8)
	for i := 0; i < len(p); i++ {
		switch c := p[i]; c {
		case '%':
			b.WriteString("%25")
		case '?':
			b.WriteString("%3F")
		case '#':
			b.WriteString("%23")
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}
