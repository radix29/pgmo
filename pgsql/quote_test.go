package pgsql

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestQuote(t *testing.T) {
	for _, tc := range []struct{ got, want string }{
		{QuoteIdent("orders"), "orders"},
		{QuoteIdent("_x1"), "_x1"},
		{QuoteIdent("Orders"), `"Orders"`},
		{QuoteIdent("1x"), `"1x"`},
		{QuoteIdent("a$"), `"a$"`},
		{QuoteIdent("é"), `"é"`},
		{QuoteIdent("a b"), `"a b"`},
		{QuoteIdent(`a"b`), `"a""b"`},
		{QuoteIdent(""), `""`},
		{QuoteIdent("select"), `"select"`},         // reserved
		{QuoteIdent("int"), `"int"`},               // column name
		{QuoteIdent("left"), `"left"`},             // type or function name
		{QuoteIdent("json_table"), `"json_table"`}, // a keyword since 17 only
		{QuoteIdent("name"), "name"},               // unreserved
		{QuoteIdent("abort"), "abort"},             // unreserved
		{QualifiedName("public", "Orders"), `public."Orders"`},
		{QualifiedName("", "orders"), "orders"},
		{QualifiedName("my.schema", "a.b"), `"my.schema"."a.b"`},
		{QualifiedName(`s"`, "user"), `"s"""."user"`},
		{QuoteLiteral("it's"), "'it''s'"},
		{QuoteLiteral(""), "''"},
		{QuoteLiteral(`a\b'c`), `E'a\\b''c'`}, // quote_literal('a\b''c'), live on pg15
		{QuoteLiteral("é\n"), "'é\n'"},
	} {
		if tc.got != tc.want {
			t.Errorf("got %s, want %s", tc.got, tc.want)
		}
	}
}

func TestUnquoteIdent(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{`"a""b"`, `a"b`},
		{`"a""""b"`, `a""b`},
		{`"Orders"`, "Orders"},
		{`""`, ""},
		{"plain", "plain"},
		{"Plain", "Plain"}, // not folded
		{`"open`, `"open`},
		{`"a"b"`, `"a"b"`},     // an undoubled quote inside: not one quoted part
		{`"s"."t"`, `"s"."t"`}, // two parts
		{`U&"a"`, `U&"a"`},     // not undone
		{`"`, `"`},
	} {
		if got := UnquoteIdent(tc.in); got != tc.want {
			t.Errorf("UnquoteIdent(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	for _, name := range []string{`a"b`, `""`, "x", "", "X", "select", `"y"`} {
		if got := UnquoteIdent(QuoteIdent(name)); got != name {
			t.Errorf("UnquoteIdent(QuoteIdent(%q)) = %q", name, got)
		}
	}
}

// TestFoldIdent: a quoted identifier is unquoted, a bare one has only ASCII
// lowered, white space around either is dropped. Pinned to the server's
// parse_ident by TestLiveFoldIdentMatchesServer.
func TestFoldIdent(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"Orders", "orders"},
		{`"Orders"`, "Orders"},
		{"orders", "orders"},
		{"@AZ[`az{", "@az[`az{"}, // the ASCII range's edges
		{"ÄbC", "Äbc"},           // only ASCII folds
		{"ǅX", "ǅx"},
		{`"a""b"`, `a"b`},
		{`""`, ""},
		{"  Ab\t\n", "ab"},
		{` " A " `, " A "}, // white space inside the quotes is the name's
		{"a.B", "a.b"},     // not split
		{`"a"b"`, `"a"b"`}, // not one quoted identifier: folded as is
		{`U&"X"`, `u&"x"`}, // not undone
		{"A\xffB", "a\xffb"},
		{"", ""},
	} {
		if got := FoldIdent(tc.in); got != tc.want {
			t.Errorf("FoldIdent(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestKeywordsLowercase guards the table's shape: QuoteIdent looks up only
// names that are already lowercase ASCII, so an entry that is not would
// never match.
func TestKeywordsLowercase(t *testing.T) {
	for kw := range keywords {
		if kw == "" || strings.ToLower(kw) != kw || strings.Trim(kw, "abcdefghijklmnopqrstuvwxyz_0123456789") != "" {
			t.Errorf("keyword %q is not a lowercase ASCII word", kw)
		}
	}
}

// TestImportsOnlyStdlib keeps this package a leaf: the reason it exists is
// that importing it does not link pgx. A standard-library import path has
// no dot in its first element.
func TestImportsOnlyStdlib(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		ast, err := parser.ParseFile(token.NewFileSet(), f, src, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range ast.Imports {
			path, _ := strconv.Unquote(imp.Path.Value)
			if first, _, _ := strings.Cut(path, "/"); strings.Contains(first, ".") {
				t.Errorf("%s imports %s: package pgsql may import only the standard library", f, path)
			}
		}
	}
}
