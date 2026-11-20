package pgsql

import (
	"strings"
	"unicode"
)

// CommentSafe makes s safe inside a -- comment: a line break in an
// identifier would end the comment and run the rest of the name as SQL.
// PostgreSQL ends a -- comment at CR as well as LF; every control character
// becomes a space. Use BlockCommentSafe inside /* */.
func CommentSafe(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
}

// BlockCommentSafe makes s safe inside a /* */ comment: a "*/" in an
// identifier would close the comment early and run the rest as SQL, and a
// "/*" would open a nested one — PostgreSQL block comments nest, unlike the
// SQL standard's — that swallows the closing "*/" and everything after it.
// A space goes between the two runes of either pair, checked against what
// was already written so that "*/*" cannot leave a "/*" behind.
func BlockCommentSafe(s string) string {
	if !strings.Contains(s, "*/") && !strings.Contains(s, "/*") {
		return s
	}
	var sb strings.Builder
	var prev rune
	for _, r := range s {
		if (prev == '*' && r == '/') || (prev == '/' && r == '*') {
			sb.WriteByte(' ')
		}
		sb.WriteRune(r)
		prev = r
	}
	return sb.String()
}
