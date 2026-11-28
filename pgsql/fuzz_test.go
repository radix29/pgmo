package pgsql

import (
	"strings"
	"testing"
)

// FuzzNames drives the quoting and comment helpers with arbitrary text, as
// every catalog name and every value written into DDL reaches them, and
// checks each against the scanner — the server's own reading of the text:
// a quoted name or literal is exactly one closed span whose value is the
// input, and a comment-safe name cannot end its comment. The seeds run in
// plain go test; fuzzing is on demand:
//
//	go test -run XXX -fuzz FuzzNames -fuzztime 60s -fuzzminimizetime 0
func FuzzNames(f *testing.F) {
	for _, s := range []string{
		"", "public", "Orders", `a"b`, `"a""b"`, `"s"."t"`, `"a"b"`, "select", "name", "json_table",
		"_x$1", "1x", "é", "ſ", "\xff", "a\x00b", `it's`, `a\b`, `\'`, `\`, "''", "$$", "$1",
		"*/", "/*", "*/*", "a\nb", "a\rb", "-- x", "/* x */", "U&\"x\"", "E'x'",
		"$", "x$", "$_", "a$_", "$_$", "$$_$$", "$tag$",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		q := QuoteIdent(s)
		if got := UnquoteIdent(q); got != s {
			t.Fatalf("UnquoteIdent(QuoteIdent(%q)) = %q", s, got)
		}
		UnquoteIdent(s)
		if got := FoldIdent(q); got != s {
			t.Fatalf("FoldIdent(QuoteIdent(%q)) = %q", s, got)
		}
		FoldIdent(s)
		if q == s {
			only(t, q, SpanCode, s) // bare: nothing the scanner treats specially
		} else {
			only(t, q, SpanQuotedIdent, s)
		}
		if q != s && s != "" { // QualifiedName("", x) is x alone
			qn := QualifiedName(s, s)
			var parts []Span
			for sp := range Spans(qn) {
				parts = append(parts, sp)
			}
			if len(parts) != 3 || parts[1].Kind != SpanCode || qn[parts[1].Start:parts[1].End] != "." {
				t.Fatalf("QualifiedName(%q, %q) = %q scans as %+v", s, s, qn, parts)
			}
		}

		lit := QuoteLiteral(s)
		kind := SpanLiteral
		if strings.Contains(s, `\`) {
			kind = SpanEscapeLiteral
		}
		only(t, lit, kind, s)
		only(t, QuoteDollar(s), SpanDollarLiteral, s)

		if got := CommentSafe(s); strings.ContainsAny(got, "\r\n") {
			t.Fatalf("CommentSafe(%q) = %q keeps a line break", s, got)
		}
		line := "-- " + CommentSafe(s) + "\nSELECT 1"
		if first(line) != "\nSELECT 1" {
			t.Fatalf("%q: the -- comment swallowed the statement", line)
		}
		block := "/* " + BlockCommentSafe(s) + " */SELECT 1"
		if first(block) != "SELECT 1" {
			t.Fatalf("%q: the block comment does not end at its */", block)
		}
	})
}

// only fails t unless text scans as exactly one closed span of kind whose
// value is want.
func only(t *testing.T, text string, kind SpanKind, want string) {
	t.Helper()
	var spans []Span
	for sp := range Spans(text) {
		spans = append(spans, sp)
	}
	if len(spans) != 1 || spans[0].Kind != kind || !spans[0].Closed {
		t.Fatalf("%q scans as %+v, want one closed span of kind %d", text, spans, kind)
	}
	if got := spans[0].Unescape(text); got != want {
		t.Fatalf("%q unescapes to %q, want %q", text, got, want)
	}
}

// first returns the text after the first span of s.
func first(s string) string {
	for sp := range Spans(s) {
		return s[sp.End:]
	}
	return s
}
