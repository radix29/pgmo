//go:build livedb

package pgmo

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/radix29/pgmo/pgsql"
)

// The live tests of the P4.11 database families — collations, text search,
// languages, casts, operators — over one throwaway database each, built by
// liveCatalogObjects. Each listing is matched to its catalog read
// independently, the fixture's objects to what their statements made, and
// every read again as a plain role (identical: none of these catalogs hides
// a row) and under a search_path holding the fixture's schema (identical:
// type names stay qualified).

// liveCatalogObjectsFixture is the fixture's DDL. "S s" holds citext, an
// enum, an ICU nondeterministic collation and a libc one, a dictionary and
// a configuration copied from english with two mappings changed, a cast by
// I/O conversion, a binary cast, and an operator that is its own
// commutator; "P l" is a trusted language over plpgsql's handlers, USAGE
// revoked from PUBLIC and granted to the plain role.
var liveCatalogObjectsFixture = []string{
	`CREATE SCHEMA "S s"`,
	`CREATE EXTENSION citext SCHEMA "S s"`,
	`CREATE TYPE "S s"."Mood" AS ENUM ('a', 'b')`,
	`CREATE COLLATION "S s"."Ci" (provider = icu, locale = 'und-u-ks-level2', deterministic = false)`,
	`CREATE COLLATION "S s".c8 FROM "C"`,
	`COMMENT ON COLLATION "S s".c8 IS 'live'`,
	`CREATE TEXT SEARCH DICTIONARY "S s"."D d" (TEMPLATE = pg_catalog.snowball, language = english, stopwords = english)`,
	`CREATE TEXT SEARCH CONFIGURATION "S s"."C c" (COPY = pg_catalog.english)`,
	`ALTER TEXT SEARCH CONFIGURATION "S s"."C c" ALTER MAPPING FOR asciiword WITH "S s"."D d", pg_catalog.simple`,
	`ALTER TEXT SEARCH CONFIGURATION "S s"."C c" DROP MAPPING FOR url`,
	`CREATE TRUSTED LANGUAGE "P l" HANDLER plpgsql_call_handler INLINE plpgsql_inline_handler VALIDATOR plpgsql_validator`,
	`REVOKE USAGE ON LANGUAGE "P l" FROM PUBLIC`,
	`COMMENT ON LANGUAGE "P l" IS 'live'`,
	`CREATE CAST ("S s"."Mood" AS text) WITH INOUT AS IMPLICIT`,
	`CREATE CAST ("S s".citext AS bytea) WITHOUT FUNCTION`,
	`CREATE FUNCTION "S s".eq(integer, integer) RETURNS boolean LANGUAGE sql IMMUTABLE AS 'SELECT $1 = $2'`,
	`CREATE OPERATOR "S s".=== (LEFTARG = integer, RIGHTARG = integer, FUNCTION = "S s".eq, COMMUTATOR = OPERATOR("S s".===), MERGES)`,
	`COMMENT ON OPERATOR "S s".=== (integer, integer) IS 'live'`,
}

// liveCatalogObjects is one fixture's database: a Server connected to it,
// the Database, an admin connection to it, the plain role's name, and runs
// fn as that role or under a search_path holding "S s".
type liveCatalogObjects struct {
	ctx   context.Context
	s     *Server
	d     *Database
	conn  *pgx.Conn
	owner string
	plain string
}

func newLiveCatalogObjects(t *testing.T, label string) *liveCatalogObjects {
	t.Helper()
	admin, ctx, done := liveDBTimeout(t, 2*time.Minute)
	t.Cleanup(done)
	stamp := time.Now().UnixNano()
	db, plain := fmt.Sprintf("pgmo_live_%s_%d", label, stamp), fmt.Sprintf("pgmo_live_%s_plain_%d", label, stamp)
	q := pgsql.QuoteIdent
	t.Cleanup(func() {
		if _, err := admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+q(db)+" WITH (FORCE)"); err != nil {
			t.Errorf("drop database: %v", err)
		}
		if _, err := admin.Exec(context.Background(), "DROP ROLE IF EXISTS "+q(plain)); err != nil {
			t.Errorf("drop role: %v", err)
		}
	})
	for _, stmt := range []string{"CREATE DATABASE " + q(db), "CREATE ROLE " + q(plain)} {
		if _, err := admin.Exec(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	conn := liveScratchConn(t, ctx, db)
	for _, stmt := range append(slices.Clone(liveCatalogObjectsFixture), `GRANT USAGE ON LANGUAGE "P l" TO `+q(plain)) {
		if _, err := conn.Exec(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	s, err := Connect(ctx, ConnectionOptions{ConnString: *liveConnString, Database: db, ApplicationName: "pgmo - live " + label})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	d, err := s.DatabaseByName(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	f := &liveCatalogObjects{ctx: ctx, s: s, d: d, conn: conn, plain: plain}
	if err := conn.QueryRow(ctx, "SELECT current_user::text").Scan(&f.owner); err != nil {
		t.Fatal(err)
	}
	return f
}

// as runs fn in a transaction as the plain role (role true) or under a
// search_path of "S s" then pg_catalog.
func (f *liveCatalogObjects) as(t *testing.T, role bool, fn func(ctx context.Context) error) {
	t.Helper()
	set := `SET LOCAL search_path = "S s", pg_catalog`
	if role {
		set = "SET LOCAL ROLE " + pgsql.QuoteIdent(f.plain)
	}
	if err := f.s.InTransaction(f.ctx, func(ctx context.Context) error {
		if _, err := f.s.on(ctx).Exec(ctx, set); err != nil {
			return err
		}
		return fn(ctx)
	}); err != nil {
		t.Fatal(err)
	}
}

// strings1 reads a one-column text query.
func (f *liveCatalogObjects) strings1(t *testing.T, sql string, args ...any) []string {
	t.Helper()
	rows, err := f.conn.Query(f.ctx, sql, args...)
	if err != nil {
		t.Fatal(err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// dumpAll prints xs, their back-pointers cleared by clear, one per line.
func dumpAll[T any](xs []*T, clear func(*T)) string {
	var sb strings.Builder
	for _, x := range xs {
		c := *x
		clear(&c)
		fmt.Fprintf(&sb, "%+v\n", c)
	}
	return sb.String()
}

func TestLiveCollations(t *testing.T) {
	f := newLiveCatalogObjects(t, "coll")
	ctx, d := f.ctx, f.d
	clear := func(c *Collation) { c.db = nil }

	all, err := d.Collations(ctx, ObjectFilter{})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, c := range all {
		got = append(got, fmt.Sprintf("%s.%s/%d", c.Schema, c.Name, c.OID))
	}
	want := f.strings1(t, `SELECT n.nspname || '.' || c.collname || '/' || c.oid FROM pg_catalog.pg_collation c
		JOIN pg_catalog.pg_namespace n ON n.oid = c.collnamespace ORDER BY n.nspname, c.collname, c.collencoding`)
	if !slices.Equal(got, want) {
		t.Fatalf("Collations: %d rows, pg_collation %d — they differ", len(got), len(want))
	}

	var ciOID, c8OID OID
	var ciVersion string
	if err := f.conn.QueryRow(ctx, `SELECT (SELECT oid FROM pg_catalog.pg_collation WHERE collname = 'Ci'),
		(SELECT oid FROM pg_catalog.pg_collation WHERE collname = 'c8'),
		(SELECT COALESCE(collversion, '') FROM pg_catalog.pg_collation WHERE collname = 'Ci')`).Scan(&ciOID, &c8OID, &ciVersion); err != nil {
		t.Fatal(err)
	}
	wantMine := []Collation{
		{OID: ciOID, Schema: "S s", Name: "Ci", Owner: f.owner, Provider: LocaleProviderICU, Locale: "und-u-ks-level2", Version: ciVersion},
		{OID: c8OID, Schema: "S s", Name: "c8", Owner: f.owner, Provider: LocaleProviderLibc, Deterministic: true,
			Collate: "C", CType: "C", Comment: "live"},
	}
	mine, err := d.Collations(ctx, ObjectFilter{Schema: []TextCriterion{{TextEquals, `"S s"`}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(mine) != 2 {
		t.Fatalf("Collations(\"S s\") = %s", dumpAll(mine, clear))
	}
	for i, c := range mine {
		g := *c
		g.db = nil
		if c.Database() != d || !reflect.DeepEqual(g, wantMine[i]) {
			t.Errorf("%s =\n%+v\nwant\n%+v", c.Name, g, wantMine[i])
		}
	}
	if ciVersion == "" {
		t.Errorf("the ICU collation records no version: the fixture means it to")
	}

	yes, no := true, false
	for _, c := range []struct {
		f    ObjectFilter
		want int // rows, -1 for "the system ones"
	}{
		{ObjectFilter{Name: []TextCriterion{{TextEquals, "CI"}}}, 0},
		{ObjectFilter{Name: []TextCriterion{{TextEquals, `"Ci"`}}}, 1},
		{ObjectFilter{System: &no, ExtensionMember: &no}, 2},
		{ObjectFilter{ExtensionMember: &yes}, 0},
	} {
		cs, err := d.Collations(ctx, c.f)
		if err != nil || len(cs) != c.want {
			t.Errorf("Collations(%+v) = %d, %v; want %d", c.f, len(cs), err, c.want)
		}
	}
	sys, err := d.Collations(ctx, ObjectFilter{System: &yes})
	if err != nil || len(sys) != len(all)-2 || !slices.ContainsFunc(sys, func(c *Collation) bool {
		return c.Name == "default" && c.Provider == LocaleProviderDefault
	}) {
		t.Errorf("Collations(System) = %d, %v; want every collation but the fixture's, default among them", len(sys), err)
	}

	byName, err := d.CollationByName(ctx, "S s", "Ci")
	if err != nil || dumpAll([]*Collation{byName}, clear) != dumpAll(mine[:1], clear) {
		t.Errorf("CollationByName = %+v, %v; want the listing's", byName, err)
	}
	ucs, err := d.CollationByName(ctx, "pg_catalog", "ucs_basic")
	if err != nil || ucs.Encoding != "UTF8" || !ucs.IsSystem {
		t.Errorf("CollationByName(ucs_basic) = %+v, %v; want the UTF8 one, a system collation", ucs, err)
	}
	for _, n := range []string{"ci", "Gone"} {
		if _, err := d.CollationByName(ctx, "S s", n); !errors.Is(err, ErrNotFound) {
			t.Errorf("CollationByName(%s): err = %v, want ErrNotFound", n, err)
		}
	}

	for _, role := range []bool{true, false} {
		f.as(t, role, func(ctx context.Context) error {
			again, err := d.Collations(ctx, ObjectFilter{})
			if err == nil && dumpAll(again, clear) != dumpAll(all, clear) {
				t.Errorf("Collations differ as role %v / under a search_path", role)
			}
			return err
		})
	}
}

func TestLiveTextSearch(t *testing.T) {
	f := newLiveCatalogObjects(t, "ts")
	ctx, d := f.ctx, f.d

	// Every configuration's mappings, as the catalog holds them, against
	// what pgmo groups.
	cfgs, err := d.TextSearchConfigurations(ctx, ObjectFilter{})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, c := range cfgs {
		for _, m := range c.Mappings {
			var dicts []string
			for _, dc := range m.Dictionaries {
				dicts = append(dicts, dc.Schema+"."+dc.Name)
			}
			got = append(got, fmt.Sprintf("%s.%s %s %s", c.Schema, c.Name, m.TokenType, strings.Join(dicts, ",")))
		}
	}
	want := f.strings1(t, `SELECT n.nspname || '.' || c.cfgname || ' ' || t.alias || ' ' ||
			pg_catalog.string_agg(dn.nspname || '.' || dc.dictname, ',' ORDER BY m.mapseqno)
		FROM pg_catalog.pg_ts_config c
		JOIN pg_catalog.pg_namespace n ON n.oid = c.cfgnamespace
		CROSS JOIN LATERAL pg_catalog.ts_token_type(c.cfgparser) t
		JOIN pg_catalog.pg_ts_config_map m ON m.mapcfg = c.oid AND m.maptokentype = t.tokid
		JOIN pg_catalog.pg_ts_dict dc ON dc.oid = m.mapdict
		JOIN pg_catalog.pg_namespace dn ON dn.oid = dc.dictnamespace
		GROUP BY n.nspname, c.cfgname, t.tokid, t.alias ORDER BY n.nspname, c.cfgname, t.tokid`)
	if !slices.Equal(got, want) {
		t.Errorf("mappings:\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	cc, err := d.TextSearchConfigurationByName(ctx, "S s", "C c")
	if err != nil {
		t.Fatal(err)
	}
	en, err := d.TextSearchConfigurationByName(ctx, "pg_catalog", "english")
	if err != nil {
		t.Fatal(err)
	}
	if cc.Owner != f.owner || cc.Parser != (ObjectName{"pg_catalog", "default"}) || cc.IsSystem || !en.IsSystem ||
		len(cc.Mappings) != len(en.Mappings)-1 ||
		!reflect.DeepEqual(cc.Mappings[0], TextSearchMapping{"asciiword", []ObjectName{{"S s", "D d"}, {"pg_catalog", "simple"}}}) ||
		slices.ContainsFunc(cc.Mappings, func(m TextSearchMapping) bool { return m.TokenType == "url" }) {
		t.Errorf("\"S s\".\"C c\" = %+v; want english's mappings, asciiword's changed and url's dropped", cc)
	}
	dd, err := d.TextSearchDictionaryByName(ctx, "S s", "D d")
	if err != nil || dd.Template != (ObjectName{"pg_catalog", "snowball"}) || dd.Options != "language = 'english', stopwords = 'english'" ||
		dd.Owner != f.owner {
		t.Errorf("\"S s\".\"D d\" = %+v, %v; want snowball with its two options", dd, err)
	}

	// The listings against the catalogs, by name; the system flag.
	for _, c := range []struct {
		what, sql string
		list      func(ctx context.Context) ([]string, error)
	}{
		{"parsers", `SELECT n.nspname || '.' || x.prsname FROM pg_catalog.pg_ts_parser x JOIN pg_catalog.pg_namespace n ON n.oid = x.prsnamespace ORDER BY 1`,
			func(ctx context.Context) ([]string, error) {
				xs, err := d.TextSearchParsers(ctx, ObjectFilter{})
				return namesOf(xs, func(x *TextSearchParser) string { return x.Schema + "." + x.Name }), err
			}},
		{"templates", `SELECT n.nspname || '.' || x.tmplname FROM pg_catalog.pg_ts_template x JOIN pg_catalog.pg_namespace n ON n.oid = x.tmplnamespace ORDER BY 1`,
			func(ctx context.Context) ([]string, error) {
				xs, err := d.TextSearchTemplates(ctx, ObjectFilter{})
				return namesOf(xs, func(x *TextSearchTemplate) string { return x.Schema + "." + x.Name }), err
			}},
		{"dictionaries", `SELECT n.nspname || '.' || x.dictname FROM pg_catalog.pg_ts_dict x JOIN pg_catalog.pg_namespace n ON n.oid = x.dictnamespace ORDER BY 1`,
			func(ctx context.Context) ([]string, error) {
				xs, err := d.TextSearchDictionaries(ctx, ObjectFilter{})
				return namesOf(xs, func(x *TextSearchDictionary) string { return x.Schema + "." + x.Name }), err
			}},
		{"configurations", `SELECT n.nspname || '.' || x.cfgname FROM pg_catalog.pg_ts_config x JOIN pg_catalog.pg_namespace n ON n.oid = x.cfgnamespace ORDER BY 1`,
			func(ctx context.Context) ([]string, error) {
				xs, err := d.TextSearchConfigurations(ctx, ObjectFilter{})
				return namesOf(xs, func(x *TextSearchConfiguration) string { return x.Schema + "." + x.Name }), err
			}},
	} {
		got, err := c.list(ctx)
		// ORDER BY 1 sorts by collation; pgmo by schema then name. Compare
		// as sets.
		slices.Sort(got)
		want := f.strings1(t, c.sql)
		slices.Sort(want)
		if err != nil || !slices.Equal(got, want) {
			t.Errorf("%s = %q, %v; want %q", c.what, got, err, want)
		}
	}
	no := false
	user, err := d.TextSearchDictionaries(ctx, ObjectFilter{System: &no})
	if err != nil || len(user) != 1 || user[0].Name != "D d" {
		t.Errorf("TextSearchDictionaries(user) = %d, %v; want \"D d\" alone", len(user), err)
	}
	if _, err := d.TextSearchParserByName(ctx, "pg_catalog", "Default"); !errors.Is(err, ErrNotFound) {
		t.Errorf("TextSearchParserByName(Default): err = %v, want ErrNotFound (exact names)", err)
	}
	if p, err := d.TextSearchParserByName(ctx, "pg_catalog", "default"); err != nil || p.Headline != (ObjectName{"pg_catalog", "prsd_headline"}) {
		t.Errorf("TextSearchParserByName(default) = %+v, %v; want its headline function", p, err)
	}
	if tm, err := d.TextSearchTemplateByName(ctx, "pg_catalog", "simple"); err != nil || tm.Init != (ObjectName{"pg_catalog", "dsimple_init"}) {
		t.Errorf("TextSearchTemplateByName(simple) = %+v, %v; want its init function", tm, err)
	}

	clear := func(c *TextSearchConfiguration) { c.db = nil }
	for _, role := range []bool{true, false} {
		f.as(t, role, func(ctx context.Context) error {
			again, err := d.TextSearchConfigurations(ctx, ObjectFilter{})
			if err == nil && dumpAll(again, clear) != dumpAll(cfgs, clear) {
				t.Errorf("TextSearchConfigurations differ as role %v / under a search_path", role)
			}
			return err
		})
	}
}

// namesOf maps xs through name.
func namesOf[T any](xs []T, name func(T) string) []string {
	out := make([]string, 0, len(xs))
	for _, x := range xs {
		out = append(out, name(x))
	}
	return out
}

func TestLiveLanguages(t *testing.T) {
	f := newLiveCatalogObjects(t, "lang")
	ctx, d := f.ctx, f.d
	clear := func(l *Language) { l.db = nil }

	all, err := d.Languages(ctx, ObjectFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := namesOf(all, func(l *Language) string { return l.Name }),
		f.strings1(t, "SELECT lanname::text FROM pg_catalog.pg_language ORDER BY lanname"); !slices.Equal(got, want) {
		t.Fatalf("Languages = %q, want %q", got, want)
	}
	var oid OID
	if err := f.conn.QueryRow(ctx, `SELECT oid FROM pg_catalog.pg_language WHERE lanname = 'P l'`).Scan(&oid); err != nil {
		t.Fatal(err)
	}
	want := Language{OID: oid, Name: "P l", Owner: f.owner, Procedural: true, Trusted: true,
		Handler:   ObjectName{"pg_catalog", "plpgsql_call_handler"},
		Inline:    ObjectName{"pg_catalog", "plpgsql_inline_handler"},
		Validator: ObjectName{"pg_catalog", "plpgsql_validator"},
		Privileges: []PrivilegeGrant{{Grantee: f.owner, Grantor: f.owner, Privilege: PrivilegeUsage},
			{Grantee: f.plain, Grantor: f.owner, Privilege: PrivilegeUsage}},
		Comment: "live"}
	byName, err := d.LanguageByName(ctx, "P l")
	if err != nil {
		t.Fatal(err)
	}
	if g := *byName; func() bool { g.db = nil; return !reflect.DeepEqual(g, want) }() {
		t.Errorf("\"P l\" = %s, want %+v", dumpAll([]*Language{byName}, clear), want)
	}
	pl, err := d.LanguageByName(ctx, "plpgsql")
	if err != nil || !pl.IsSystem || pl.Extension != "plpgsql" || !pl.PrivilegesDefault {
		t.Errorf("plpgsql = %+v, %v; want initdb's, the extension's member, its ACL never set", pl, err)
	}
	yes, no := true, false
	for _, c := range []struct {
		f    ObjectFilter
		want []string
	}{
		{ObjectFilter{System: &yes}, []string{"c", "internal", "plpgsql", "sql"}},
		{ObjectFilter{System: &no}, []string{"P l"}},
		{ObjectFilter{ExtensionMember: &yes}, []string{"plpgsql"}},
		{ObjectFilter{Name: []TextCriterion{{TextEquals, "P L"}}}, nil},
		{ObjectFilter{Name: []TextCriterion{{TextEquals, `"P l"`}}, Schema: []TextCriterion{{TextEquals, "x"}}}, []string{"P l"}},
	} {
		ls, err := d.Languages(ctx, c.f)
		if got := namesOf(ls, func(l *Language) string { return l.Name }); err != nil || !slices.Equal(got, c.want) {
			t.Errorf("Languages(%+v) = %q, %v; want %q", c.f, got, err, c.want)
		}
	}
	if _, err := d.LanguageByName(ctx, "p l"); !errors.Is(err, ErrNotFound) {
		t.Errorf("LanguageByName(p l): err = %v, want ErrNotFound", err)
	}
	f.as(t, true, func(ctx context.Context) error {
		again, err := d.Languages(ctx, ObjectFilter{})
		if err == nil && dumpAll(again, clear) != dumpAll(all, clear) {
			t.Errorf("Languages differ as a plain role")
		}
		return err
	})
}

func TestLiveCasts(t *testing.T) {
	f := newLiveCatalogObjects(t, "cast")
	ctx, d := f.ctx, f.d
	clear := func(c *Cast) { c.db = nil }

	all, err := d.Casts(ctx, ObjectFilter{})
	if err != nil {
		t.Fatal(err)
	}
	got := namesOf(all, func(c *Cast) string { return fmt.Sprint(c.OID) })
	want := f.strings1(t, "SELECT oid::text FROM pg_catalog.pg_cast")
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("Casts: %d, pg_cast %d — they differ", len(got), len(want))
	}

	var mood, citext OID
	if err := f.conn.QueryRow(ctx, `SELECT '"S s"."Mood"'::pg_catalog.regtype::pg_catalog.oid, '"S s".citext'::pg_catalog.regtype::pg_catalog.oid`).
		Scan(&mood, &citext); err != nil {
		t.Fatal(err)
	}
	no, yes := false, true
	user, err := d.Casts(ctx, ObjectFilter{System: &no, ExtensionMember: &no})
	if err != nil {
		t.Fatal(err)
	}
	sigs := namesOf(user, (*Cast).Signature)
	if !slices.Equal(sigs, []string{`("S s"."Mood" AS text)`, `("S s".citext AS bytea)`}) {
		t.Fatalf("user casts = %q", sigs)
	}
	if c := user[0]; c.SourceType != mood || c.TargetType != 25 || c.Function != (ObjectName{}) ||
		c.Context != CastImplicit || c.Method != CastByInOut {
		t.Errorf("%+v: want an implicit I/O conversion", c)
	}
	if c := user[1]; c.SourceType != citext || c.TargetType != 17 || c.Function != (ObjectName{}) ||
		c.Context != CastExplicit || c.Method != CastBinary {
		t.Errorf("%+v: want an explicit binary coercion", c)
	}
	members, err := d.Casts(ctx, ObjectFilter{ExtensionMember: &yes})
	if err != nil || len(members) == 0 || slices.ContainsFunc(members, func(c *Cast) bool { return c.Extension != "citext" }) {
		t.Errorf("Casts(member) = %d, %v; want citext's own", len(members), err)
	}
	byTypes, err := d.CastByTypes(ctx, mood, 25)
	if err != nil || dumpAll([]*Cast{byTypes}, clear) != dumpAll(user[:1], clear) {
		t.Errorf("CastByTypes = %+v, %v; want the listing's", byTypes, err)
	}
	if _, err := d.CastByTypes(ctx, 25, mood); !errors.Is(err, ErrNotFound) {
		t.Errorf("CastByTypes(text, Mood): err = %v, want ErrNotFound", err)
	}
	for _, role := range []bool{true, false} {
		f.as(t, role, func(ctx context.Context) error {
			again, err := d.Casts(ctx, ObjectFilter{})
			if err == nil && dumpAll(again, clear) != dumpAll(all, clear) {
				t.Errorf("Casts differ as role %v / under a search_path", role)
			}
			return err
		})
	}
}

func TestLiveOperators(t *testing.T) {
	f := newLiveCatalogObjects(t, "op")
	ctx, d := f.ctx, f.d
	clear := func(o *Operator) { o.db = nil }

	all, err := d.Operators(ctx, ObjectFilter{})
	if err != nil {
		t.Fatal(err)
	}
	got := namesOf(all, func(o *Operator) string { return fmt.Sprint(o.OID) })
	want := f.strings1(t, `SELECT o.oid::text FROM pg_catalog.pg_operator o JOIN pg_catalog.pg_namespace n ON n.oid = o.oprnamespace
		ORDER BY n.nspname, o.oprname, o.oprleft, o.oprright`)
	if !slices.Equal(got, want) {
		t.Fatalf("Operators: %d, pg_operator %d — they differ", len(got), len(want))
	}
	// Every operator's identity, as regoperator prints it under an empty
	// search_path (every name qualified), against Signature.
	if _, err := f.conn.Exec(ctx, "SET search_path = ''"); err != nil {
		t.Fatal(err)
	}
	idents := f.strings1(t, `SELECT o.oid::pg_catalog.regoperator::text FROM pg_catalog.pg_operator o
		JOIN pg_catalog.pg_namespace n ON n.oid = o.oprnamespace ORDER BY n.nspname, o.oprname, o.oprleft, o.oprright`)
	if _, err := f.conn.Exec(ctx, "RESET search_path"); err != nil {
		t.Fatal(err)
	}
	for i, o := range all {
		// regoperator writes "schema.op(left,right)", NONE for none, no
		// spaces; pg_catalog's types it writes as Signature does, and
		// pg_catalog itself not at all (always searched, even under an
		// empty search_path).
		sig := strings.ReplaceAll(strings.Replace(o.Signature(), " (", "(", 1), ", ", ",")
		sig = strings.TrimPrefix(sig, "pg_catalog.")
		if sig != idents[i] {
			t.Errorf("Signature %q, regoperator %q", o.Signature(), idents[i])
			break
		}
	}

	var oid, eq OID
	if err := f.conn.QueryRow(ctx, `SELECT '"S s".===(integer,integer)'::pg_catalog.regoperator::pg_catalog.oid,
		'pg_catalog.=(integer,integer)'::pg_catalog.regoperator::pg_catalog.oid`).Scan(&oid, &eq); err != nil {
		t.Fatal(err)
	}
	wantMine := Operator{OID: oid, Schema: "S s", Name: "===", Owner: f.owner, Kind: OperatorInfix,
		LeftType: 23, RightType: 23, ResultType: 16, Left: "integer", Right: "integer", Result: "boolean",
		Function: ObjectName{"S s", "eq"}, Commutator: ObjectName{"S s", "==="}, Merges: true, Comment: "live"}
	mine, err := d.OperatorByName(ctx, "S s", "===", 23, 23)
	if err != nil {
		t.Fatal(err)
	}
	if g := *mine; func() bool { g.db = nil; return !reflect.DeepEqual(g, wantMine) }() {
		t.Errorf("\"S s\".=== = %s, want %+v", dumpAll([]*Operator{mine}, clear), wantMine)
	}
	if mine.Signature() != `"S s".=== (integer, integer)` {
		t.Errorf("Signature() = %s", mine.Signature())
	}
	if o, err := d.OperatorByName(ctx, "pg_catalog", "=", 23, 23); err != nil || o.OID != eq || !o.IsSystem {
		t.Errorf("OperatorByName(=) = %+v, %v; want OID %d, a system one", o, err, eq)
	}
	if o, err := d.OperatorByName(ctx, "pg_catalog", "-", 0, 23); err != nil || o.Kind != OperatorPrefix || o.Left != "" {
		t.Errorf("OperatorByName(prefix -) = %+v, %v", o, err)
	}
	if _, err := d.OperatorByName(ctx, "S s", "===", 23, 20); !errors.Is(err, ErrNotFound) {
		t.Errorf("OperatorByName(=== integer, bigint): err = %v, want ErrNotFound", err)
	}
	yes, no := true, false
	user, err := d.Operators(ctx, ObjectFilter{Schema: []TextCriterion{{TextEquals, `"S s"`}}, ExtensionMember: &no})
	if err != nil || len(user) != 1 || user[0].OID != oid {
		t.Errorf("Operators(\"S s\", not members) = %d, %v; want === alone", len(user), err)
	}
	cit, err := d.Operators(ctx, ObjectFilter{Name: []TextCriterion{{TextEquals, "="}}, ExtensionMember: &yes})
	if err != nil || len(cit) == 0 || slices.ContainsFunc(cit, func(o *Operator) bool { return o.Extension != "citext" || o.Schema != "S s" }) {
		t.Errorf("Operators(=, members) = %d, %v; want citext's", len(cit), err)
	}
	for _, role := range []bool{true, false} {
		f.as(t, role, func(ctx context.Context) error {
			again, err := d.Operators(ctx, ObjectFilter{})
			if err == nil && dumpAll(again, clear) != dumpAll(all, clear) {
				t.Errorf("Operators differ as role %v / under a search_path", role)
			}
			return err
		})
	}
}
