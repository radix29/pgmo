//go:build livedb

package pgmo

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/radix29/pgmo/pgsql"
)

// quoteCorpus is the names and values the live quoting tests send to the
// server besides its own keyword list: the shapes QuoteIdent and
// QuoteLiteral treat differently, and the characters that break naive
// quoting.
var quoteCorpus = []string{
	"a", "orders", "_", "_1", "a1", "a_b", "A", "Orders", "1a", "a b", "a.b", "a-b",
	"a$", "$a", "é", "Ä", "日本", "ſ", `a"b`, `"`, `""`, "it's", "'", `\`, `a\b`, `\'`,
	`a\\b''c`, "tab\there", "line\nbreak", "cr\rx", "/* x */", "-- x", "$$", "$1",
	"select", "Select", "user", "name", "json_table", "system_user", "merge_action",
}

// TestLiveQuoteIdentMatchesServer pins pgsql.QuoteIdent to the server's
// quote_ident on every keyword the server knows and on quoteCorpus, and
// proves each result reads back as the same name. Against pg18, whose
// keywords pgsql's table is, the two must agree exactly; against an older
// server pgsql may quote a word that is a keyword only in a newer release,
// never leave bare one the server quotes.
func TestLiveQuoteIdentMatchesServer(t *testing.T) {
	pool, ctx, done := liveDB(t)
	defer done()
	var num int
	if err := pool.QueryRow(ctx, "SELECT pg_catalog.current_setting('server_version_num')::int").Scan(&num); err != nil {
		t.Fatalf("server_version_num: %v", err)
	}
	rows, err := pool.Query(ctx, "SELECT word, catcode::text, pg_catalog.quote_ident(word) FROM pg_catalog.pg_get_keywords()")
	if err != nil {
		t.Fatalf("pg_get_keywords: %v", err)
	}
	type kw struct{ word, catcode, server string }
	var kws []kw
	for rows.Next() {
		var k kw
		if err := rows.Scan(&k.word, &k.catcode, &k.server); err != nil {
			t.Fatalf("pg_get_keywords: %v", err)
		}
		kws = append(kws, k)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("pg_get_keywords: %v", err)
	}
	if len(kws) < 400 {
		t.Fatalf("pg_get_keywords returned %d words", len(kws))
	}
	names := append([]string(nil), quoteCorpus...)
	for _, k := range kws {
		if (k.catcode != "U") != (k.server != k.word) {
			t.Errorf("server: quote_ident(%q) = %q for catcode %s", k.word, k.server, k.catcode)
		}
		names = append(names, k.word, strings.ToUpper(k.word))
	}
	stricter := 0
	for _, name := range names {
		var server string
		if err := pool.QueryRow(ctx, "SELECT pg_catalog.quote_ident($1)", name).Scan(&server); err != nil {
			t.Fatalf("quote_ident(%q): %v", name, err)
		}
		got := pgsql.QuoteIdent(name)
		switch {
		case got == server:
		case num < 180000 && server == name && got == `"`+name+`"`:
			stricter++ // a keyword only in a newer release
		default:
			t.Errorf("QuoteIdent(%q) = %s, server quote_ident = %s", name, got, server)
		}
		readsBack(t, ctx, pool, got, name)
	}
	t.Logf("server_version_num %d: %d names, %d quoted only as a newer release's keyword", num, len(names), stricter)
}

// readsBack checks that ident, written as a table alias, a column alias
// and a column reference, is read by the server as the name want.
func readsBack(t *testing.T, ctx context.Context, pool *pgxpool.Pool, ident, want string) {
	t.Helper()
	rows, err := pool.Query(ctx, "SELECT "+ident+"."+ident+" FROM (SELECT 1 AS "+ident+") AS "+ident)
	if err != nil {
		t.Errorf("%s does not read back: %v", ident, err)
		return
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Errorf("%s does not read back: %v", ident, err)
		return
	}
	if got := rows.FieldDescriptions()[0].Name; got != want {
		t.Errorf("%s reads back as %q, want %q", ident, got, want)
	}
}

// TestLiveQuoteLiteralMatchesServer pins pgsql.QuoteLiteral to the server's
// quote_literal on quoteCorpus, and proves each literal reads back as its
// value with standard_conforming_strings both on and off — the reason a
// literal holding a backslash is written E'…'.
func TestLiveQuoteLiteralMatchesServer(t *testing.T) {
	pool, ctx, done := liveDB(t)
	defer done()
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()
	for _, scs := range []string{"on", "off"} {
		if _, err := conn.Exec(ctx, "SET standard_conforming_strings = "+scs); err != nil {
			t.Fatalf("standard_conforming_strings = %s: %v", scs, err)
		}
		for _, v := range quoteCorpus {
			lit := pgsql.QuoteLiteral(v)
			var server, back string
			if err := conn.QueryRow(ctx, "SELECT pg_catalog.quote_literal($1::text), "+lit+"::text", v).Scan(&server, &back); err != nil {
				t.Errorf("standard_conforming_strings %s: %s: %v", scs, lit, err)
				continue
			}
			if lit != server {
				t.Errorf("QuoteLiteral(%q) = %s, server quote_literal = %s", v, lit, server)
			}
			if back != v {
				t.Errorf("standard_conforming_strings %s: %s reads back as %q, want %q", scs, lit, back, v)
			}
		}
	}
	// Leave the pooled session as it was found.
	if _, err := conn.Exec(ctx, "RESET standard_conforming_strings"); err != nil {
		t.Fatalf("reset: %v", err)
	}
}

// TestLiveSpanUnescapeMatchesServer pins Span.Unescape to the value the
// server reads from each literal and quoted-identifier form whose escapes
// pgsql decodes.
func TestLiveSpanUnescapeMatchesServer(t *testing.T) {
	pool, ctx, done := liveDB(t)
	defer done()
	for _, text := range []string{
		"'it''s'", `'a\nb'`, "N'x'", `E'a\'b''c\\d'`, `E'\n\t\b\f\r\q'`, `E'\101\7\0101'`,
		`E'\x41\x4g\x'`, `E'é\U0001F600'`, `E'😀'`, `U&'d\0061t\+000061'`,
		`U&'\\ \D83D\DE00 it''s'`, "$$a'b''c$$", "$t$ $$ $t$", "$_é$x$_é$",
	} {
		var sp pgsql.Span
		for sp = range pgsql.Spans(text) {
			break
		}
		var server string
		if err := pool.QueryRow(ctx, "SELECT "+text+"::text").Scan(&server); err != nil {
			t.Errorf("%s: %v", text, err)
			continue
		}
		if got := sp.Unescape(text); got != server {
			t.Errorf("Unescape(%s) = %q, server reads %q", text, got, server)
		}
	}
	for _, text := range []string{`"a""b"`, `U&"d\0061t"`, `U&"a""\+01F600"`, `"Orders"`} {
		var sp pgsql.Span
		for sp = range pgsql.Spans(text) {
			break
		}
		readsBack(t, ctx, pool, text, sp.Unescape(text))
	}
}
