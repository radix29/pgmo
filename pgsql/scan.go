package pgsql

import (
	"iter"
	"strings"
	"unicode/utf8"
)

// SpanKind says what one run of PostgreSQL text is to the server's lexer.
type SpanKind uint8

const (
	SpanCode           SpanKind = iota // anything not below
	SpanLineComment                    // -- to the end of the line
	SpanBlockComment                   // /* … */, nested
	SpanParam                          // $1, a positional parameter
	SpanLiteral                        // '…', N'…'
	SpanEscapeLiteral                  // E'…'
	SpanUnicodeLiteral                 // U&'…'
	SpanBitLiteral                     // B'…', X'…'
	SpanDollarLiteral                  // $$…$$, $tag$…$tag$
	SpanQuotedIdent                    // "…"
	SpanUnicodeIdent                   // U&"…"
)

// Span is one run of text from Spans: s[Start:End] is the whole of it,
// delimiters and any prefix (E, N, B, X, U&) included. Closed is false for a
// quoted span or block comment that ran to the end of the text unterminated.
type Span struct {
	Kind       SpanKind
	Start, End int
	Closed     bool
}

// Spans splits s into the runs the server lexes differently: code, the two
// comment forms, positional parameters, the string literal forms (plain,
// escape, Unicode, bit and hex, dollar-quoted) and the two quoted-identifier
// forms. The spans tile s in order with no gaps; code spans are never empty.
//
// It is the one lexical walk behind every rewrite of script text in pgmo —
// binding $N arguments, redacting secrets, reading a function's CREATE
// header — so a rule fixed here is fixed for all of them. It scans, it does
// not parse; the rules are scan.l's, with standard_conforming_strings on:
//
//   - In '…', U&'…', "…" and U&"…" only a doubled closer escapes itself; a
//     backslash is an ordinary character (a U& escape is decoded after
//     lexing). In E'…' a backslash escapes the byte after it, so E'a\'b' is
//     one literal, and ” still escapes a quote. B'…' and X'…' have no
//     escape at all: B'1”0' is B'1' and then '0'.
//   - A dollar quote opens at $$ or $tag$ — a tag is a letter, '_' or
//     non-ASCII byte, then those or digits — and ends at the first exact,
//     case-sensitive repeat of the same delimiter; nothing inside is special.
//   - Block comments nest, so they are counted, not matched: the first */ of
//     "/* a /* b */ c */" does not end it. -- and /* start a comment even in
//     the middle of an operator ("1+--x").
//   - A -- comment ends before its first CR or LF, or runs to the end of the
//     text when there is none; it is closed either way. A quoted span or
//     block comment left open runs to the end of the text with Closed false.
//   - A prefix (E, N, B, X, U& — either case), a dollar quote and a $N
//     parameter start a token only as a word of their own, not inside one:
//     "E'x'" is an escape literal, "ME'x'" is the word ME and then a plain
//     literal; "a$$b$$" is one identifier, since '$' continues a name. A word
//     is a run of isIdentByte inside one code span, so a span's closer ends
//     it: $$a$$E'b' is a dollar quote and then an escape literal.
//   - Two literals separated by white space holding a newline are one
//     constant to the server ('a'<LF>'b' is 'ab'); here they are two spans
//     with code between, which is what a rewrite needs.
func Spans(s string) iter.Seq[Span] {
	return func(yield func(Span) bool) {
		code := 0 // where the pending code span starts
		for i := 0; i < len(s); {
			c := s[i]
			word := i == code || !isIdentByte(s[i-1]) // a token can start here
			var next, next2 byte
			if i+1 < len(s) {
				next = s[i+1]
			}
			if i+2 < len(s) {
				next2 = s[i+2]
			}
			var sp Span
			switch {
			case c == '\'':
				sp = scanQuoted(s, i, i, SpanLiteral, '\'')
			case c == '"':
				sp = scanQuoted(s, i, i, SpanQuotedIdent, '"')
			case word && next == '\'' && (c == 'E' || c == 'e'):
				sp = scanEscape(s, i)
			case word && next == '\'' && (c == 'N' || c == 'n'):
				sp = scanQuoted(s, i, i+1, SpanLiteral, '\'')
			case word && next == '\'' && (c == 'B' || c == 'b' || c == 'X' || c == 'x'):
				sp = Span{Kind: SpanBitLiteral, Start: i, End: len(s)}
				if j := strings.IndexByte(s[i+2:], '\''); j >= 0 {
					sp.End, sp.Closed = i+2+j+1, true
				}
			case word && (c == 'U' || c == 'u') && next == '&' && next2 == '\'':
				sp = scanQuoted(s, i, i+2, SpanUnicodeLiteral, '\'')
			case word && (c == 'U' || c == 'u') && next == '&' && next2 == '"':
				sp = scanQuoted(s, i, i+2, SpanUnicodeIdent, '"')
			case word && c == '$' && isDigit(next):
				j := i + 2
				for j < len(s) && isDigit(s[j]) {
					j++
				}
				sp = Span{Kind: SpanParam, Start: i, End: j, Closed: true}
			case word && c == '$' && dollarTag(s[i:]) != "":
				tag := dollarTag(s[i:])
				sp = Span{Kind: SpanDollarLiteral, Start: i, End: len(s)}
				if j := strings.Index(s[i+len(tag):], tag); j >= 0 {
					sp.End, sp.Closed = i+len(tag)+j+len(tag), true
				}
			case c == '-' && next == '-':
				sp = Span{Kind: SpanLineComment, Start: i, End: len(s), Closed: true}
				if j := strings.IndexAny(s[i:], "\r\n"); j >= 0 {
					sp.End = i + j
				}
			case c == '/' && next == '*':
				depth, j := 1, i+2
				for j < len(s) && depth > 0 {
					switch {
					case strings.HasPrefix(s[j:], "/*"):
						depth, j = depth+1, j+2
					case strings.HasPrefix(s[j:], "*/"):
						depth, j = depth-1, j+2
					default:
						j++
					}
				}
				sp = Span{Kind: SpanBlockComment, Start: i, End: j, Closed: depth == 0}
			default:
				i++
				continue
			}
			if code < sp.Start && !yield(Span{Kind: SpanCode, Start: code, End: sp.Start, Closed: true}) {
				return
			}
			if !yield(sp) {
				return
			}
			code, i = sp.End, sp.End
		}
		if code < len(s) {
			yield(Span{Kind: SpanCode, Start: code, End: len(s), Closed: true})
		}
	}
}

// scanQuoted reads the quoted span that starts at s[start] (a prefix, or the
// delimiter itself) and opens at s[open], up to the closer that ends it. A
// doubled closer is an escaped one.
func scanQuoted(s string, start, open int, kind SpanKind, closer byte) Span {
	for j := open + 1; j < len(s); j++ {
		if s[j] != closer {
			continue
		}
		if j+1 < len(s) && s[j+1] == closer {
			j++
			continue
		}
		return Span{Kind: kind, Start: start, End: j + 1, Closed: true}
	}
	return Span{Kind: kind, Start: start, End: len(s)}
}

// scanEscape reads the E'…' literal whose E is s[start]: a backslash
// escapes the byte after it, and ” is a quote.
func scanEscape(s string, start int) Span {
	for j := start + 2; j < len(s); j++ {
		switch s[j] {
		case '\\':
			j++
		case '\'':
			if j+1 < len(s) && s[j+1] == '\'' {
				j++
				continue
			}
			return Span{Kind: SpanEscapeLiteral, Start: start, End: j + 1, Closed: true}
		}
	}
	return Span{Kind: SpanEscapeLiteral, Start: start, End: len(s)}
}

// dollarTag returns the dollar-quote delimiter s starts with — "$$" or
// "$tag$" — or "" when it starts with none.
func dollarTag(s string) string {
	if len(s) < 2 || s[0] != '$' {
		return ""
	}
	for j := 1; j < len(s); j++ {
		switch c := s[j]; {
		case c == '$':
			return s[:j+1]
		case isTagStart(c), j > 1 && isDigit(c):
		default:
			return ""
		}
	}
	return ""
}

// IsQuoted reports whether sp is a literal or a quoted identifier.
func (sp Span) IsQuoted() bool {
	return sp.Kind >= SpanLiteral
}

// IsLiteral reports whether sp is a string constant of any form, bit and
// hex strings and dollar quotes included.
func (sp Span) IsLiteral() bool {
	return sp.Kind >= SpanLiteral && sp.Kind <= SpanDollarLiteral
}

// IsIdent reports whether sp is a quoted identifier of either form.
func (sp Span) IsIdent() bool {
	return sp.Kind == SpanQuotedIdent || sp.Kind == SpanUnicodeIdent
}

// IsComment reports whether sp is a comment of either form.
func (sp Span) IsComment() bool {
	return sp.Kind == SpanLineComment || sp.Kind == SpanBlockComment
}

// Unescape returns the value of a quoted span of s — its text without the
// delimiters or prefix, with its escapes decoded — and any other span's text
// as it stands. An unterminated span's value runs to the end.
//
// The value is the one the server lexes: ” and "" are made single, an
// E'…' literal's backslash escapes (\n, \t, octal, \xHH, \uXXXX,
// \UXXXXXXXX) and a U&'…' or U&"…" span's \XXXX, \+XXXXXX and \\ are
// decoded, a UTF-16 surrogate pair making one character. A B'…' or X'…'
// literal's value is its digits; a dollar quote's is its text verbatim. A
// U& span is decoded with the default escape character: a UESCAPE clause
// after it is not read. An escape the server would reject (an invalid code
// point, a lone surrogate) decodes to U+FFFD; an incomplete U& escape is
// left as written.
func (sp Span) Unescape(s string) string {
	text := s[sp.Start:sp.End]
	var open int // the delimiter's offset in text
	switch sp.Kind {
	case SpanLiteral, SpanEscapeLiteral, SpanBitLiteral, SpanUnicodeLiteral:
		open = strings.IndexByte(text, '\'')
	case SpanQuotedIdent, SpanUnicodeIdent:
		open = strings.IndexByte(text, '"')
	case SpanDollarLiteral:
		tag := dollarTag(text)
		body := text[len(tag):]
		if sp.Closed {
			body = body[:len(body)-len(tag)]
		}
		return body
	default:
		return text
	}
	closer := text[open : open+1]
	body := text[open+1:]
	if sp.Closed {
		body = body[:len(body)-1]
	}
	switch sp.Kind {
	case SpanBitLiteral:
		return body
	case SpanEscapeLiteral:
		return unescapeE(body)
	}
	body = strings.ReplaceAll(body, closer+closer, closer)
	if sp.Kind == SpanUnicodeLiteral || sp.Kind == SpanUnicodeIdent {
		return unescapeU(body)
	}
	return body
}

// unescapeE decodes the body of an E'…' literal: ” and the backslash
// escapes the server's lexer reads (xeescape, xeoctesc, xehexesc,
// xeunicode).
func unescapeE(body string) string {
	var sb strings.Builder
	// unicode reads \uXXXX or \UXXXXXXXX at body[j:].
	unicode := func(j int) (rune, int, bool) {
		if j+1 >= len(body) || body[j] != '\\' {
			return 0, 0, false
		}
		switch body[j+1] {
		case 'u':
			cp, ok := hexRune(body[j+2:], 4)
			return cp, 6, ok
		case 'U':
			cp, ok := hexRune(body[j+2:], 8)
			return cp, 10, ok
		}
		return 0, 0, false
	}
	for i := 0; i < len(body); {
		c := body[i]
		if c == '\'' && i+1 < len(body) && body[i+1] == '\'' {
			sb.WriteByte('\'')
			i += 2
			continue
		}
		if c != '\\' || i+1 == len(body) {
			sb.WriteByte(c)
			i++
			continue
		}
		if cp, n, ok := unicode(i); ok {
			i += n + writeCodePoint(&sb, cp, body, i+n, unicode)
			continue
		}
		e := body[i+1]
		switch {
		case e >= '0' && e <= '7':
			v, j := 0, i+1
			for j < len(body) && j < i+4 && body[j] >= '0' && body[j] <= '7' {
				v, j = v*8+int(body[j]-'0'), j+1
			}
			sb.WriteByte(byte(v))
			i = j
		case e == 'x' && i+2 < len(body) && isHex(body[i+2]):
			v, j := 0, i+2
			for j < len(body) && j < i+4 && isHex(body[j]) {
				v, j = v*16+hexVal(body[j]), j+1
			}
			sb.WriteByte(byte(v))
			i = j
		default:
			if r := strings.IndexByte("btnfr", e); r >= 0 {
				e = "\b\t\n\f\r"[r]
			}
			sb.WriteByte(e)
			i += 2
		}
	}
	return sb.String()
}

// unescapeU decodes the body of a U&'…' literal or U&"…" identifier, its
// doubled closers already made single: \XXXX, \+XXXXXX and \\, with the
// default escape character.
func unescapeU(body string) string {
	if !strings.Contains(body, `\`) {
		return body
	}
	var sb strings.Builder
	// unicode reads \XXXX or \+XXXXXX at body[j:].
	unicode := func(j int) (rune, int, bool) {
		if j+1 >= len(body) || body[j] != '\\' {
			return 0, 0, false
		}
		if body[j+1] == '+' {
			cp, ok := hexRune(body[j+2:], 6)
			return cp, 8, ok
		}
		cp, ok := hexRune(body[j+1:], 4)
		return cp, 5, ok
	}
	for i := 0; i < len(body); {
		if body[i] != '\\' {
			sb.WriteByte(body[i])
			i++
			continue
		}
		if i+1 < len(body) && body[i+1] == '\\' {
			sb.WriteByte('\\')
			i += 2
			continue
		}
		if cp, n, ok := unicode(i); ok {
			i += n + writeCodePoint(&sb, cp, body, i+n, unicode)
			continue
		}
		sb.WriteByte('\\')
		i++
	}
	return sb.String()
}

// writeCodePoint writes the code point an escape decoded to, pairing a
// high surrogate with a low one escaped right after it at body[at:], read
// by unicode. It returns how many more bytes of body it used (the second
// escape of a pair, or 0). What the server would reject becomes U+FFFD.
func writeCodePoint(sb *strings.Builder, cp rune, body string, at int, unicode func(int) (rune, int, bool)) int {
	if cp >= 0xD800 && cp <= 0xDBFF {
		if lo, n, ok := unicode(at); ok && lo >= 0xDC00 && lo <= 0xDFFF {
			sb.WriteRune((cp-0xD800)<<10 + (lo - 0xDC00) + 0x10000)
			return n
		}
	}
	if cp == 0 || !utf8.ValidRune(cp) {
		cp = utf8.RuneError
	}
	sb.WriteRune(cp)
	return 0
}

// hexRune reads exactly n hex digits from the start of s.
func hexRune(s string, n int) (rune, bool) {
	if len(s) < n {
		return 0, false
	}
	var v rune
	for i := range n {
		if !isHex(s[i]) {
			return 0, false
		}
		v = v*16 + rune(hexVal(s[i]))
	}
	return v, true
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func isHex(c byte) bool { return isDigit(c) || c|0x20 >= 'a' && c|0x20 <= 'f' }

func hexVal(c byte) int {
	if isDigit(c) {
		return int(c - '0')
	}
	return int(c|0x20-'a') + 10
}

// isTagStart reports whether c can start a dollar-quote tag or an unquoted
// name: an ASCII letter, '_', or any byte of a non-ASCII character
// (scan.l's ident_start and dolq_start).
func isTagStart(c byte) bool {
	return c == '_' || c >= 0x80 || 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z'
}

// isIdentByte reports whether c can continue an unquoted PostgreSQL word —
// scan.l's ident_cont: a name start, a digit or '$'. Digits count so that a
// number's trailing letter is not read as a prefix ("1e'x'").
func isIdentByte(c byte) bool {
	return isTagStart(c) || isDigit(c) || c == '$'
}
