package pgsql

import "strings"

// QuoteIdent returns name as PostgreSQL would read it as one identifier: bare
// when it needs no quotes, otherwise wrapped in double quotes with every
// embedded double quote doubled. It is the server's quote_ident() and follows
// its rule: a name stays bare only if it starts with a lowercase ASCII letter
// or '_', continues with lowercase ASCII letters, digits or '_', and is not a
// keyword other than an unreserved one.
//
// Quoting only when needed is what pg_dump and PostgreSQL users write
// (PLAN.md D14), and it is always right, not just tidier:
//
//   - An uppercase letter is quoted, because PostgreSQL folds an unquoted name
//     to lowercase: Orders bare is the table orders.
//   - '$' and non-ASCII letters are legal in a bare name but quoted here, as
//     quote_ident quotes them: its rule is ASCII-only, and pgmo's text is
//     meant to read the same as the server's.
//   - The keyword list is the union over every supported server version, so
//     a name that became a keyword in a newer release is quoted even in text
//     generated against an older one (keywords.go).
//
// It quotes the whole string as one identifier, so pass each part of a
// qualified name separately (QualifiedName). The empty name comes back as
// "", which the server rejects as a zero-length identifier: an empty name is
// a caller's bug, and an error from the server says so better than a silently
// missing name would.
func QuoteIdent(name string) string {
	if isBareIdent(name) {
		return name
	}
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// isBareIdent reports whether QuoteIdent leaves name bare: see QuoteIdent.
func isBareIdent(name string) bool {
	if name == "" {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z', c == '_':
		case i > 0 && c >= '0' && c <= '9':
		default:
			return false
		}
	}
	_, kw := keywords[name]
	return !kw
}

// QualifiedName returns schema.name, each part quoted with QuoteIdent, or
// just the quoted name when schema is empty. The empty case is the point:
// the naive QuoteIdent(schema)+"."+QuoteIdent(name) emits `"".name`, which
// the server rejects as a zero-length identifier. An unqualified name
// resolves through the session's search_path, so pass the schema whenever
// the object's schema is known.
func QualifiedName(schema, name string) string {
	if schema == "" {
		return QuoteIdent(name)
	}
	return QuoteIdent(schema) + "." + QuoteIdent(name)
}

// UnquoteIdent undoes QuoteIdent for one name part: `"a""b"` is `a"b`.
// Anything else — a bare name, or one whose quotes don't match — comes back
// unchanged. It does not split a qualified name: `"s"."t"` is not one part,
// so it is returned as is.
//
// A bare name is not case-folded. Use it where PostgreSQL hands back an
// identifier already quoted (regclass and regproc text, EXPLAIN output,
// pg_get_*def): the server prints a name bare only when it needs no quotes,
// so a bare name there already is the stored name. A name typed by a person
// is different — Orders bare means orders — and is not what this is for.
// The U&"…" form is not undone; the server never prints it.
func UnquoteIdent(name string) string {
	if len(name) < 2 || name[0] != '"' || name[len(name)-1] != '"' {
		return name
	}
	inner := name[1 : len(name)-1]
	// Every quote inside must be doubled, or the outer pair is not one
	// quoted identifier (`"a"b"`, `"a"."b"`).
	if strings.Count(inner, `""`)*2 != strings.Count(inner, `"`) {
		return name
	}
	return strings.ReplaceAll(inner, `""`, `"`)
}

// FoldIdent returns the stored name that one identifier, as a person types
// it, means to the server: "Orders" (quoted) is Orders, Orders bare is
// orders. It is the counterpart of UnquoteIdent for typed input, and what a
// caller looking an object up by a typed name does first — pgmo matches
// names exactly (two names differing only by case are two objects), so the
// folding has to happen before the lookup, the server's way, not as a
// case-insensitive match after it.
//
// The rule is the server's parse_ident() on one part: leading and trailing
// white space is dropped; a whole quoted identifier is unquoted as
// UnquoteIdent does; anything else has ASCII A–Z lowered and every other
// byte kept — the server's downcase_identifier touches only ASCII in a
// multibyte server encoding (UTF8), so ÄbC means Äbc. As parse_ident, it
// does not truncate a name over 63 bytes (the lexer would). It neither
// splits at '.' nor validates: pass each part of a qualified name
// separately; text the server would reject comes back folded as is. The
// U&"…" form is not undone.
func FoldIdent(s string) string {
	s = strings.Trim(s, " \t\n\r\v\f")
	if u := UnquoteIdent(s); u != s {
		return u
	}
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + ('a' - 'A')
		}
	}
	return string(b)
}

// QuoteLiteral renders s as a PostgreSQL string literal, safe to embed in
// SQL text where a parameter placeholder is not accepted (DDL, a DO block,
// a function body). Prefer a bound parameter ($1) for ordinary values. It is
// the server's quote_literal(): embedded single quotes are doubled, and a
// string holding a backslash becomes an escape string, E'…', with every
// backslash doubled too — so the literal means s whatever the session's
// standard_conforming_strings says. Without a backslash it is a plain '…'.
//
// It quotes a value, not an identifier. A name that ends up inside a string
// literal — the argument of a regclass cast, to_regclass, has_table_privilege
// and similar — needs QuoteIdent (QualifiedName) on each part first: without
// the quotes the server folds it to lowercase and splits it at a '.'.
//
// PostgreSQL text cannot hold a NUL byte, so neither can a literal; the
// server rejects a statement whose text contains one.
func QuoteLiteral(s string) string {
	if !strings.Contains(s, `\`) {
		return "'" + strings.ReplaceAll(s, "'", "''") + "'"
	}
	var sb strings.Builder
	sb.Grow(len(s) + 4)
	sb.WriteString("E'")
	for i := 0; i < len(s); i++ {
		if c := s[i]; c == '\'' || c == '\\' {
			sb.WriteByte(c)
		}
		sb.WriteByte(s[i])
	}
	sb.WriteByte('\'')
	return sb.String()
}

// QuoteDollar quotes s as a dollar-quoted string constant, the form pg_dump
// writes a function body in: $$…$$, or $_$…$_$, $__$…$__$ — the first tag
// whose opening part ("$", "$_", …) does not occur in s, as pg_dump's
// appendStringLiteralDQ picks it. Nothing inside is escaped, so a body reads
// exactly as written, quotes and backslashes included, whatever
// standard_conforming_strings says.
//
// No occurrence of the closing tag can start inside s: one would need s to
// end in the tag's opening part, which s does not contain.
func QuoteDollar(s string) string {
	tag := "$"
	for strings.Contains(s, tag) {
		tag += "_"
	}
	tag += "$"
	return tag + s + tag
}
