package pgsql

import (
	"strings"
	"testing"
)

// spanString renders Spans(s) as kind:text pairs, a ! after an
// unterminated one, for table comparison.
func spanString(s string) string {
	names := [...]string{"code", "line", "block", "param", "lit", "elit", "ulit", "blit", "dollar", "quoted", "uquoted"}
	var parts []string
	for sp := range Spans(s) {
		p := names[sp.Kind] + ":" + s[sp.Start:sp.End]
		if !sp.Closed {
			p += "!"
		}
		parts = append(parts, p)
	}
	return strings.Join(parts, " | ")
}

func TestSpans(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"", ""},
		{"SELECT 1", "code:SELECT 1"},
		{"a 'b' c", "code:a  | lit:'b' | code: c"},
		{"'it''s'", "lit:'it''s'"},
		{`'a\'`, `lit:'a\'`}, // standard_conforming_strings: a backslash is a character
		{"N'x' n'y'", "lit:N'x' | code:  | lit:n'y'"},
		{"IN'x'", "code:IN | lit:'x'"},
		{`E'a\'b' x`, `elit:E'a\'b' | code: x`},
		{`e'a''b\\'`, `elit:e'a''b\\'`},
		{`E'\`, `elit:E'\!`},
		{"ME'x'", "code:ME | lit:'x'"},
		{"1e'x'", "code:1e | lit:'x'"},
		{"éE'x'", "code:éE | lit:'x'"},
		{"B'10' x'1f' X'' b'", "blit:B'10' | code:  | blit:x'1f' | code:  | blit:X'' | code:  | blit:b'!"},
		{"B'1''0'", "blit:B'1' | lit:'0'"}, // no escape in a bit string (live: syntax error at '0')
		{`U&'d\0061t' u&'x'`, `ulit:U&'d\0061t' | code:  | ulit:u&'x'`},
		{`U&"a""b" x`, `uquoted:U&"a""b" | code: x`},
		{"fU&'x'", "code:fU& | lit:'x'"},
		{"U & 'x'", "code:U &  | lit:'x'"},
		{`"a""b" x`, `quoted:"a""b" | code: x`},
		{`"open`, `quoted:"open!`},
		{"$$a'b$$ x", "dollar:$$a'b$$ | code: x"},
		{"$fn$ $$ $FN$ $fn$x", "dollar:$fn$ $$ $FN$ $fn$ | code:x"},
		{"$a$ $b$ $a$", "dollar:$a$ $b$ $a$"},
		{"$_1é$x$_1é$", "dollar:$_1é$x$_1é$"},
		{"$1$ x", "param:$1 | code:$ x"},
		{"$a$ open", "dollar:$a$ open!"},
		{"a$$b$$", "code:a$$b$$"}, // '$' continues a name
		{"$$a$$E'b'", "dollar:$$a$$ | elit:E'b'"},
		{"$1$$x$$", "param:$1 | dollar:$$x$$"}, // live: syntax error at "$$x$$"
		{"$a b", "code:$a b"},
		{"$", "code:$"},
		{"SELECT $1, $23 FROM t WHERE a$1 = $", "code:SELECT  | param:$1 | code:,  | param:$23 | code: FROM t WHERE a$1 = $"},
		{"a -- c\nb", "code:a  | line:-- c | code:\nb"},
		{"a -- c", "code:a  | line:-- c"},
		{"-- a\rSELECT 1", "line:-- a | code:\rSELECT 1"},
		{"1+--x\n", "code:1+ | line:--x | code:\n"},
		{"/* a /* b */ c */x", "block:/* a /* b */ c */ | code:x"},
		{"/* a /* b */", "block:/* a /* b */!"},
		{"/*/ */", "block:/*/ */"},
		{"2*/*x*/3", "code:2* | block:/*x*/ | code:3"},
		{"'open", "lit:'open!"},
		{"-- '\n'--'", "line:-- ' | code:\n | lit:'--'"},
		{"'/*' */", "lit:'/*' | code: */"},
		{"$$--$$ '$$'", "dollar:$$--$$ | code:  | lit:'$$'"},
		{"'a'\n'b'", "lit:'a' | code:\n | lit:'b'"},
	} {
		if got := spanString(tc.in); got != tc.want {
			t.Errorf("Spans(%q)\n got %s\nwant %s", tc.in, got, tc.want)
		}
	}
}

func TestSpanUnescape(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"'it''s'", "it's"},
		{`'a\nb'`, `a\nb`},
		{"N'it''s'", "it's"},
		{"'open''", "open'"},
		{"'", ""},
		{`E'a\'b''c\\d'`, `a'b'c\d`},
		{`E'\n\t\b\f\r\q'`, "\n\t\b\f\r" + "q"},
		{`E'\101\7\0101'`, "A\x07\x081"},
		{`E'\x41\x4g\x'`, "A\x04gx"},
		{`E'é\U0001F600'`, "é😀"},
		{`E'😀'`, "😀"},
		{`E'\uD83Dx'`, "�x"},
		{`E'\u0000'`, "�"},
		{`E'\U00110000'`, "�"},
		{`E'\u12'`, "u12"},
		{`E'open\`, `open\`},
		{`U&'d\0061t\+000061'`, "data"},
		{`U&'\\ \D83D\DE00 it''s'`, `\ 😀 it's`},
		{`U&'\12'`, `\12`},
		{`U&"d\0061t"`, "dat"},
		{`U&"a""b"`, `a"b`},
		{"B'1010'", "1010"},
		{"x'1F'", "1F"},
		{"b'open", "open"},
		{`"a""b"`, `a"b`},
		{`"`, ""},
		{"$$a'b''c$$", "a'b''c"},
		{"$t$ $$ $t$", " $$ "},
		{"$t$open", "open"},
		{"$1", "$1"},
		{"-- x", "-- x"},
	} {
		sp := Span{}
		for sp = range Spans(tc.in) {
			break
		}
		if got := sp.Unescape(tc.in); got != tc.want {
			t.Errorf("Unescape(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestSpanPredicates(t *testing.T) {
	for kind := SpanCode; kind <= SpanUnicodeIdent; kind++ {
		sp := Span{Kind: kind}
		comment := kind == SpanLineComment || kind == SpanBlockComment
		literal := kind >= SpanLiteral && kind <= SpanDollarLiteral
		ident := kind == SpanQuotedIdent || kind == SpanUnicodeIdent
		if sp.IsComment() != comment || sp.IsLiteral() != literal || sp.IsIdent() != ident || sp.IsQuoted() != (literal || ident) {
			t.Errorf("kind %d: IsComment %v IsLiteral %v IsIdent %v IsQuoted %v", kind, sp.IsComment(), sp.IsLiteral(), sp.IsIdent(), sp.IsQuoted())
		}
	}
}

// FuzzSpans checks the scanner's invariants on arbitrary text: the spans
// tile it in order, code spans are non-empty and closed, a closed span ends
// in its own closer, Unescape never panics — and every span, scanned on its
// own, comes back as exactly itself. The last holds because a token's start
// depends only on bytes inside the code run before it, so a span never
// depends on what precedes it; a scanner that let a closer join the next
// word, or let a quoted span end at a different closer, fails it.
//
//	go test -run XXX -fuzz FuzzSpans -fuzztime 60s -fuzzminimizetime 0
func FuzzSpans(f *testing.F) {
	for _, s := range []string{
		"", "'a''b'", "N'x'", "IN'x'", "éN'x'", `E'a\'b'`, `E'\`, "B'1''0'", `U&'\0061'`, `U&"a""b"`,
		`"a""b"`, "-- x", "-- x\ny", "-- x\ry", "/* /* */ */", "/* /*", "'open", `"open`,
		"$$a$$", "$t$ $$ $t$", "$t$open", "a$$b$$", "$$a$$E'b'", "$1$$x$$", "$1, $23",
		"SELECT $1, '$2' -- $2\n/* $1 */ \"$1\" $f$ $1 $f$",
		"CREATE FUNCTION f() RETURNS int LANGUAGE sql AS $body$ SELECT 1 $body$",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		at := 0
		for sp := range Spans(s) {
			if sp.Start != at || sp.End <= sp.Start || sp.End > len(s) {
				t.Fatalf("%q: span %+v does not follow %d", s, sp, at)
			}
			if sp.Kind == SpanCode && !sp.Closed {
				t.Fatalf("%q: open code span %+v", s, sp)
			}
			text := s[sp.Start:sp.End]
			if sp.Closed && sp.IsQuoted() {
				last := text[len(text)-1]
				want := map[bool]byte{true: '"', false: '\''}[sp.IsIdent()]
				if sp.Kind == SpanDollarLiteral {
					want = '$'
				}
				if last != want {
					t.Fatalf("%q: closed span %+v ends in %q", s, sp, last)
				}
			}
			sp.Unescape(s)
			var alone []Span
			for a := range Spans(text) {
				alone = append(alone, a)
			}
			if len(alone) != 1 || alone[0] != (Span{Kind: sp.Kind, End: len(text), Closed: sp.Closed}) {
				t.Fatalf("%q: span %+v (%q) scans alone as %+v", s, sp, text, alone)
			}
			at = sp.End
		}
		if at != len(s) {
			t.Fatalf("%q: spans end at %d", s, at)
		}
	})
}
