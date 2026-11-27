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
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/radix29/pgmo/pgsql"
)

// liveTypeFixture creates a throwaway database holding every kind of type
// and domain TestLiveTypes and TestLiveDomains read, two roles, and returns
// the database's name, a connection to it, the roles' names (b, plain) and
// the connected role's.
func liveTypeFixture(t *testing.T, ctx context.Context, admin *pgxpool.Pool, stmts []string) (db string, conn *pgx.Conn, b, plain, connected string) {
	t.Helper()
	stamp := time.Now().UnixNano()
	name := func(s string) string { return fmt.Sprintf("pgmo_live_type_%d_%s", stamp, s) }
	b, plain, db = name("B b"), name("plain"), name("db")
	q := pgsql.QuoteIdent
	if err := admin.QueryRow(ctx, "SELECT current_user::text").Scan(&connected); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+q(db)+" WITH (FORCE)"); err != nil {
			t.Errorf("drop database: %v", err)
		}
		for _, r := range []string{b, plain} {
			if _, err := admin.Exec(context.Background(), "DROP ROLE IF EXISTS "+q(r)); err != nil {
				t.Errorf("drop role %s: %v", r, err)
			}
		}
	})
	for _, stmt := range []string{"CREATE DATABASE " + q(db), "CREATE ROLE " + q(b), "CREATE ROLE " + q(plain)} {
		if _, err := admin.Exec(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	cfg, err := pgx.ParseConfig(*liveConnString)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Database = db
	conn, err = pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close(context.Background()) })
	for _, stmt := range stmts {
		stmt = strings.NewReplacer("{b}", q(b), "{plain}", q(plain)).Replace(stmt)
		if _, err := conn.Exec(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	return db, conn, b, plain, connected
}

// asPlainRole runs dump as the role plain, in a transaction of s, and
// returns what it answered.
func asPlainRole(t *testing.T, ctx context.Context, s *Server, plain string, dump func(context.Context) (string, error)) string {
	t.Helper()
	var out string
	err := s.InTransaction(ctx, func(ctx context.Context) error {
		if _, err := s.on(ctx).Exec(ctx, "SET LOCAL ROLE "+pgsql.QuoteIdent(plain)); err != nil {
			return err
		}
		var err error
		out, err = dump(ctx)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// TestLiveTypes reads the types of a throwaway database the listing and the
// by-name read must describe in full:
//
//   - app.mood: an enum whose third label was added BEFORE the second, so
//     sort order is not creation order, a label holding a space and a
//     quote, a comment, USAGE granted WITH GRANT OPTION; app."Mood", by
//     case alone; app.none, an enum with no labels;
//   - app.pair: a composite with a dropped attribute, one with its own
//     collation, one of an array of app.mood and one of a domain;
//     app.empty, a composite with none;
//   - app.fr: a float8 range with a SUBTYPE_DIFF and a named multirange
//     app.frm, PUBLIC's USAGE revoked; app.tr: a text range with a
//     collation and the server's multirange name;
//   - app.citext, the citext extension's base type; app.shell, a shell
//     type (CREATE TYPE name;).
//
// What is no Type stays out of the listing and is ErrNotFound by name: the
// implicit arrays, a table's and a view's row types, a domain. pg_catalog's
// int4range, point and box are read for the fields user types cannot
// reach; the listing plus the domains, the implicit arrays and the
// relations' row types account for every pg_type row. The listing is read
// again under a search_path holding app, and as a plain role: identical.
func TestLiveTypes(t *testing.T) {
	admin, ctx, done := liveDBTimeout(t, 2*time.Minute)
	t.Cleanup(done)
	db, conn, b, plain, connected := liveTypeFixture(t, ctx, admin, []string{
		"CREATE SCHEMA app",
		"CREATE EXTENSION citext SCHEMA app",
		"CREATE TYPE app.mood AS ENUM ('sad', 'ok', 'not ''so'' ok')",
		"ALTER TYPE app.mood ADD VALUE 'meh' BEFORE 'ok'",
		"COMMENT ON TYPE app.mood IS 'moody'",
		"GRANT USAGE ON TYPE app.mood TO {b} WITH GRANT OPTION",
		`CREATE TYPE app."Mood" AS ENUM ('x')`,
		"CREATE TYPE app.none AS ENUM ()",
		"CREATE DOMAIN app.code AS varchar(8)",
		`CREATE TYPE app.pair AS (gone integer, b varchar(5) COLLATE "C", c app.mood[], d app.code, e numeric(10,2))`,
		"ALTER TYPE app.pair DROP ATTRIBUTE gone",
		"CREATE TYPE app.empty AS ()",
		"CREATE FUNCTION app.fdiff(float8, float8) RETURNS float8 LANGUAGE sql IMMUTABLE AS 'SELECT $1 - $2'",
		"CREATE TYPE app.fr AS RANGE (subtype = float8, subtype_diff = app.fdiff, multirange_type_name = app.frm)",
		"REVOKE USAGE ON TYPE app.fr FROM PUBLIC",
		`CREATE TYPE app.tr AS RANGE (subtype = text, collation = "C")`,
		"CREATE TABLE app.tab (x integer)",
		"CREATE VIEW app.vw AS SELECT 1 AS one",
		"CREATE TYPE app.shell",
	})

	s, err := Connect(ctx, ConnectionOptions{ConnString: *liveConnString, Database: db, ApplicationName: "pgmo - live types"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	d, err := s.DatabaseByName(ctx, db)
	if err != nil {
		t.Fatal(err)
	}

	app := ObjectFilter{Schema: []TextCriterion{{TextEquals, "app"}}}
	listed, err := d.Types(ctx, app)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, ty := range listed {
		names = append(names, ty.Name)
	}
	if want := []string{"Mood", "citext", "empty", "fr", "frm", "mood", "none", "pair", "shell", "tr", "tr_multirange"}; !slices.Equal(names, want) {
		t.Fatalf("Types(app) = %q, want %q", names, want)
	}
	no, yes := false, true
	if ext, err := d.Types(ctx, ObjectFilter{ExtensionMember: &yes}); err != nil || len(ext) != 1 || ext[0].Name != "citext" ||
		ext[0].Origin != (Origin{Extension: "citext"}) {
		t.Errorf("Types(extension) = %+v, %v; want app.citext of citext", ext, err)
	}
	if user, err := d.Types(ctx, ObjectFilter{System: &no}); err != nil || len(user) != len(listed) {
		t.Errorf("Types(user) = %d, %v; want app's %d", len(user), err, len(listed))
	}
	ty := func(schema, n string) *Type {
		t.Helper()
		got, err := d.TypeByName(ctx, schema, n)
		if err != nil {
			t.Fatal(err)
		}
		if i := slices.IndexFunc(listed, func(l *Type) bool { return l.Schema == schema && l.Name == n }); i >= 0 && !reflect.DeepEqual(listed[i], got) {
			t.Errorf("listing's %s =\n%+v\nby name\n%+v", n, listed[i], got)
		}
		return got
	}
	pc := func(n string) ObjectName { return ObjectName{"pg_catalog", n} }

	mood := ty("app", "mood")
	if mood.OID == 0 || mood.Owner != connected || mood.Kind != TypeEnum || mood.Category != TypeCategoryEnum || mood.Preferred ||
		!slices.Equal(mood.Labels, []string{"sad", "meh", "ok", "not 'so' ok"}) || mood.Comment != "moody" ||
		mood.Input != pc("enum_in") || mood.Output != pc("enum_out") || mood.Receive != pc("enum_recv") || mood.Send != pc("enum_send") ||
		mood.TypmodInput != (ObjectName{}) || mood.Analyze != (ObjectName{}) || mood.Subscript != (ObjectName{}) ||
		mood.InternalLength != 4 || !mood.PassedByValue || mood.Alignment != AlignInt4 || mood.Storage != StoragePlain ||
		mood.Delimiter != "," || mood.Element != "" || mood.HasDefault || mood.Collatable || mood.Attributes != nil ||
		mood.Subtype != "" || mood.Range != (ObjectName{}) || mood.PrivilegesDefault || mood.Origin != (Origin{}) {
		t.Errorf("app.mood = %+v", mood)
	}
	for _, g := range []PrivilegeGrant{
		{Grantee: b, Grantor: connected, Privilege: PrivilegeUsage, Grantable: true},
		{Grantee: "", Grantor: connected, Privilege: PrivilegeUsage},
	} {
		if !slices.Contains(mood.Privileges, g) {
			t.Errorf("app.mood privileges %+v: want %+v", mood.Privileges, g)
		}
	}
	if upper := ty("app", "Mood"); upper.OID == mood.OID || !slices.Equal(upper.Labels, []string{"x"}) {
		t.Errorf(`app."Mood" = %+v: want its own enum`, upper)
	}
	if none := ty("app", "none"); none.Kind != TypeEnum || none.Labels != nil {
		t.Errorf("app.none = %+v: want an enum with no labels", none)
	}

	pair := ty("app", "pair")
	var codeOID, moodArray OID
	if err := conn.QueryRow(ctx, "SELECT 'app.code'::regtype::oid, 'app.mood[]'::regtype::oid").Scan(&codeOID, &moodArray); err != nil {
		t.Fatal(err)
	}
	if want := []TypeAttribute{
		{Name: "b", TypeOID: 1043, Type: "character varying(5)", Collation: pc("C")},
		{Name: "c", TypeOID: moodArray, Type: "app.mood[]"},
		{Name: "d", TypeOID: codeOID, Type: "app.code"},
		{Name: "e", TypeOID: 1700, Type: "numeric(10,2)"},
	}; pair.Kind != TypeComposite || pair.Category != TypeCategoryComposite || !reflect.DeepEqual(pair.Attributes, want) ||
		pair.Input != pc("record_in") || pair.Labels != nil || !pair.PrivilegesDefault || pair.Shell ||
		!reflect.DeepEqual(pair.Privileges, []PrivilegeGrant{{Grantee: "", Grantor: connected, Privilege: PrivilegeUsage},
			{Grantee: connected, Grantor: connected, Privilege: PrivilegeUsage}}) {
		t.Errorf("app.pair = %+v\nattributes %+v, want %+v", pair, pair.Attributes, want)
	}
	if sh := ty("app", "shell"); !sh.Shell || sh.Kind != TypePseudo || mood.Shell {
		t.Errorf("app.shell = %+v: want a shell type", sh)
	}
	if empty := ty("app", "empty"); empty.Kind != TypeComposite || empty.Attributes != nil {
		t.Errorf("app.empty = %+v: want a composite with no attributes", empty)
	}

	fr := ty("app", "fr")
	if fr.Kind != TypeRange || fr.Category != TypeCategoryRange || fr.Subtype != "double precision" ||
		fr.SubtypeOperatorClass != pc("float8_ops") || fr.Collation != (ObjectName{}) || fr.Canonical != (ObjectName{}) ||
		fr.SubtypeDiff != (ObjectName{"app", "fdiff"}) || fr.Multirange != (ObjectName{"app", "frm"}) || fr.Range != (ObjectName{}) ||
		fr.Input != pc("range_in") || fr.Analyze != pc("range_typanalyze") || fr.Collatable ||
		!reflect.DeepEqual(fr.Privileges, []PrivilegeGrant{{Grantee: connected, Grantor: connected, Privilege: PrivilegeUsage}}) {
		t.Errorf("app.fr = %+v", fr)
	}
	if frm := ty("app", "frm"); frm.Kind != TypeMultirange || frm.Range != (ObjectName{"app", "fr"}) || frm.Subtype != "" ||
		frm.Multirange != (ObjectName{}) || frm.Input != pc("multirange_in") {
		t.Errorf("app.frm = %+v", frm)
	}
	if tr := ty("app", "tr"); tr.Subtype != "text" || tr.Collation != pc("C") || tr.SubtypeOperatorClass != pc("text_ops") ||
		tr.Multirange != (ObjectName{"app", "tr_multirange"}) || tr.SubtypeDiff != (ObjectName{}) {
		t.Errorf("app.tr = %+v", tr)
	}
	if ct := ty("app", "citext"); ct.Kind != TypeBase || ct.Extension != "citext" || ct.Input != (ObjectName{"app", "citextin"}) ||
		ct.Output != (ObjectName{"app", "citextout"}) || ct.InternalLength != -1 || !ct.Collatable || ct.Category != TypeCategoryString ||
		ct.Storage != StorageExtended {
		t.Errorf("app.citext = %+v", ct)
	}

	// pg_catalog's, for what user types cannot reach without C.
	if i4r := ty("pg_catalog", "int4range"); !i4r.IsSystem || i4r.Canonical != pc("int4range_canonical") ||
		i4r.SubtypeDiff != pc("int4range_subdiff") || i4r.Multirange != pc("int4multirange") || i4r.Subtype != "integer" {
		t.Errorf("pg_catalog.int4range = %+v", i4r)
	}
	if p := ty("pg_catalog", "point"); p.Kind != TypeBase || p.Element != "double precision" || p.InternalLength != 16 ||
		p.Alignment != AlignDouble || p.Subscript != pc("raw_array_subscript_handler") || p.Category != TypeCategoryGeometric {
		t.Errorf("pg_catalog.point = %+v", p)
	}
	if bx := ty("pg_catalog", "box"); bx.Delimiter != ";" || bx.Element != "point" {
		t.Errorf("pg_catalog.box = %+v", bx)
	}
	if vc := ty("pg_catalog", "varchar"); vc.TypmodInput != pc("varchartypmodin") || vc.TypmodOutput != pc("varchartypmodout") ||
		!vc.Collatable || vc.PassedByValue {
		t.Errorf("pg_catalog.varchar = %+v", vc)
	}
	if tx := ty("pg_catalog", "text"); !tx.Preferred || tx.Category != TypeCategoryString {
		t.Errorf("pg_catalog.text = %+v: want the String category's preferred type", tx)
	}
	if ae := ty("pg_catalog", "anyelement"); ae.Kind != TypePseudo || ae.Category != TypeCategoryPseudo {
		t.Errorf("pg_catalog.anyelement = %+v", ae)
	}

	// What is no Type.
	for _, c := range []struct{ schema, name string }{
		{"app", "_mood"}, {"app", "_pair"}, {"app", "tab"}, {"app", "vw"}, {"app", "code"}, {"pg_catalog", "_int4"}, {"pg_catalog", "pg_class"},
		{"APP", "mood"},
	} {
		if _, err := d.TypeByName(ctx, c.schema, c.name); !errors.Is(err, ErrNotFound) {
			t.Errorf("TypeByName(%s.%s): err = %v, want ErrNotFound", c.schema, c.name, err)
		}
	}
	if _, err := d.DomainByName(ctx, "app", "mood"); !errors.Is(err, ErrNotFound) {
		t.Errorf("DomainByName(an enum): err = %v, want ErrNotFound", err)
	}

	// Every pg_type row is a listed Type, a domain, an array the server
	// made for its element, or a relation's row type.
	all, err := d.Types(ctx, ObjectFilter{})
	if err != nil {
		t.Fatal(err)
	}
	domains, err := d.Domains(ctx, ObjectFilter{})
	if err != nil {
		t.Fatal(err)
	}
	var total, arrays, rowTypes int
	if err := conn.QueryRow(ctx, `SELECT pg_catalog.count(*),
		pg_catalog.count(*) FILTER (WHERE EXISTS (SELECT FROM pg_catalog.pg_type e WHERE e.oid = t.typelem AND e.typarray = t.oid)),
		pg_catalog.count(*) FILTER (WHERE t.typrelid <> 0 AND (SELECT c.relkind FROM pg_catalog.pg_class c WHERE c.oid = t.typrelid) <> 'c')
		FROM pg_catalog.pg_type t`).Scan(&total, &arrays, &rowTypes); err != nil {
		t.Fatal(err)
	}
	if len(all)+len(domains)+arrays+rowTypes != total {
		t.Errorf("%d types + %d domains + %d arrays + %d row types, want pg_type's %d rows", len(all), len(domains), arrays, rowTypes, total)
	}

	dump := func(ctx context.Context) (string, error) {
		ts, err := d.Types(ctx, app)
		if err != nil {
			return "", err
		}
		var sb strings.Builder
		for _, ty := range ts {
			c := *ty
			c.db = nil
			fmt.Fprintf(&sb, "%+v\n", c)
		}
		return sb.String(), nil
	}
	want, err := dump(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// The type names are qualified whatever the path.
	err = s.InTransaction(ctx, func(ctx context.Context) error {
		if _, err := s.on(ctx).Exec(ctx, "SET LOCAL search_path = app, public"); err != nil {
			return err
		}
		got, err := dump(ctx)
		if err == nil && got != want {
			err = fmt.Errorf("under search_path app the types differ:\n%s\nwant\n%s", got, want)
		}
		return err
	})
	if err != nil {
		t.Error(err)
	}
	if got := asPlainRole(t, ctx, s, plain, dump); got != want {
		t.Errorf("as a plain role the types differ:\n%s\nwant\n%s", got, want)
	}
}

// TestLiveDomains reads the domains of a throwaway database the listing and
// the by-name read must describe in full:
//
//   - app.d1: varchar(10) COLLATE "C" DEFAULT 'x' NOT NULL and a named
//     CHECK, a comment, USAGE granted WITH GRANT OPTION;
//   - app.d2: numeric(10,2) with a NOT NULL named nn (a constraint row of
//     its own from 17, the name kept) and a CHECK added NOT VALID;
//   - app.d3 over app.d1: the base qualified, its collation inherited, not
//     its own; app."D1", by case alone; app.d4 over an array of an enum,
//     with a default calling a function off the search_path;
//   - information_schema's domains, system objects.
//
// The listing is read again under a search_path holding app — the
// structured fields unchanged, the rendered default following the path —
// and as a plain role: identical.
func TestLiveDomains(t *testing.T) {
	admin, ctx, done := liveDBTimeout(t, 2*time.Minute)
	t.Cleanup(done)
	db, _, b, plain, connected := liveTypeFixture(t, ctx, admin, []string{
		"CREATE SCHEMA app",
		`CREATE DOMAIN app.d1 AS varchar(10) COLLATE "C" DEFAULT 'x' NOT NULL CONSTRAINT pos CHECK (VALUE <> '')`,
		"COMMENT ON DOMAIN app.d1 IS 'the first'",
		"GRANT USAGE ON DOMAIN app.d1 TO {b} WITH GRANT OPTION",
		"CREATE DOMAIN app.d2 AS numeric(10,2) CONSTRAINT nn NOT NULL",
		"ALTER DOMAIN app.d2 ADD CONSTRAINT c2 CHECK (VALUE > 0) NOT VALID",
		"CREATE DOMAIN app.d3 AS app.d1",
		`CREATE DOMAIN app."D1" AS integer`,
		"CREATE TYPE app.mood AS ENUM ('ok')",
		"CREATE FUNCTION app.moods() RETURNS app.mood[] LANGUAGE sql IMMUTABLE AS $$SELECT '{ok}'::app.mood[]$$",
		"CREATE DOMAIN app.d4 AS app.mood[] DEFAULT app.moods()",
	})

	s, err := Connect(ctx, ConnectionOptions{ConnString: *liveConnString, Database: db, ApplicationName: "pgmo - live domains"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	d, err := s.DatabaseByName(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	app := ObjectFilter{Schema: []TextCriterion{{TextEquals, "app"}}}
	listed, err := d.Domains(ctx, app)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, dm := range listed {
		names = append(names, dm.Name)
	}
	if want := []string{"D1", "d1", "d2", "d3", "d4"}; !slices.Equal(names, want) {
		t.Fatalf("Domains(app) = %q, want %q", names, want)
	}
	yes := true
	sys, err := d.Domains(ctx, ObjectFilter{System: &yes})
	if err != nil || len(sys) == 0 {
		t.Fatalf("Domains(system) = %d, %v", len(sys), err)
	}
	for _, dm := range sys {
		if dm.Schema != "information_schema" || !dm.IsSystem {
			t.Errorf("system domain %s.%s (IsSystem %v): want information_schema's", dm.Schema, dm.Name, dm.IsSystem)
		}
	}
	dom := func(n string) *Domain {
		t.Helper()
		got, err := d.DomainByName(ctx, "app", n)
		if err != nil {
			t.Fatal(err)
		}
		if i := slices.IndexFunc(listed, func(l *Domain) bool { return l.Name == n }); i >= 0 && !reflect.DeepEqual(listed[i], got) {
			t.Errorf("listing's %s =\n%+v\nby name\n%+v", n, listed[i], got)
		}
		return got
	}
	v17 := s.Version() >= 170000

	d1 := dom("d1")
	want1 := []DomainConstraint{{Name: "pos", Kind: ConstraintCheck, Definition: "CHECK (((VALUE)::text <> ''::text))", Validated: true}}
	if v17 {
		want1 = append([]DomainConstraint{{Name: "d1_not_null", Kind: ConstraintNotNull, Definition: "NOT NULL", Validated: true}}, want1...)
	}
	if d1.OID == 0 || d1.Owner != connected || d1.BaseTypeOID != 1043 || d1.BaseType != "character varying(10)" || !d1.NotNull ||
		d1.Default != "'x'::character varying" || d1.Collation != (ObjectName{"pg_catalog", "C"}) ||
		!reflect.DeepEqual(d1.Constraints, want1) || d1.Comment != "the first" || d1.PrivilegesDefault || d1.Origin != (Origin{}) {
		t.Errorf("app.d1 = %+v\nconstraints want %+v", d1, want1)
	}
	for _, g := range []PrivilegeGrant{
		{Grantee: b, Grantor: connected, Privilege: PrivilegeUsage, Grantable: true},
		{Grantee: "", Grantor: connected, Privilege: PrivilegeUsage},
	} {
		if !slices.Contains(d1.Privileges, g) {
			t.Errorf("app.d1 privileges %+v: want %+v", d1.Privileges, g)
		}
	}

	d2 := dom("d2")
	want2 := []DomainConstraint{{Name: "c2", Kind: ConstraintCheck, Definition: "CHECK ((VALUE > (0)::numeric)) NOT VALID"}}
	if v17 {
		want2 = append(want2, DomainConstraint{Name: "nn", Kind: ConstraintNotNull, Definition: "NOT NULL", Validated: true})
	}
	if d2.BaseType != "numeric(10,2)" || !d2.NotNull || d2.Default != "" || d2.Collation != (ObjectName{}) ||
		!reflect.DeepEqual(d2.Constraints, want2) || !d2.PrivilegesDefault ||
		!reflect.DeepEqual(d2.Privileges, []PrivilegeGrant{{Grantee: "", Grantor: connected, Privilege: PrivilegeUsage},
			{Grantee: connected, Grantor: connected, Privilege: PrivilegeUsage}}) {
		t.Errorf("app.d2 = %+v\nconstraints want %+v", d2, want2)
	}
	// Over a domain: its own NOT NULL and constraints are none, its
	// collation is the base's (not its own), its default copied from it.
	if d3 := dom("d3"); d3.BaseTypeOID != d1.OID || d3.BaseType != "app.d1" || d3.NotNull || d3.Constraints != nil ||
		d3.Collation != (ObjectName{}) || d3.Default != "'x'::character varying" {
		t.Errorf("app.d3 = %+v", d3)
	}
	if upper := dom("D1"); upper.OID == d1.OID || upper.BaseType != "integer" {
		t.Errorf(`app."D1" = %+v: want its own domain`, upper)
	}
	if d4 := dom("d4"); d4.BaseType != "app.mood[]" || d4.Default != "app.moods()" {
		t.Errorf("app.d4 = %+v", d4)
	}
	for _, c := range []struct{ schema, name string }{{"app", "mood"}, {"app", "_d1"}, {"APP", "d1"}, {"app", "D2"}} {
		if _, err := d.DomainByName(ctx, c.schema, c.name); !errors.Is(err, ErrNotFound) {
			t.Errorf("DomainByName(%s.%s): err = %v, want ErrNotFound", c.schema, c.name, err)
		}
	}

	dump := func(ctx context.Context) (string, error) {
		ds, err := d.Domains(ctx, app)
		if err != nil {
			return "", err
		}
		var sb strings.Builder
		for _, dm := range ds {
			c := *dm
			c.db = nil
			fmt.Fprintf(&sb, "%+v\n", c)
		}
		return sb.String(), nil
	}
	want, err := dump(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Under a path holding app the structured fields are unchanged; the
	// default, rendered by the server, names the function as the path
	// finds it.
	err = s.InTransaction(ctx, func(ctx context.Context) error {
		if _, err := s.on(ctx).Exec(ctx, "SET LOCAL search_path = app, public"); err != nil {
			return err
		}
		d4, err := d.DomainByName(ctx, "app", "d4")
		if err == nil && (d4.BaseType != "app.mood[]" || d4.Default != "moods()") {
			err = fmt.Errorf("app.d4 under search_path app: base %q, default %q; want app.mood[], moods()", d4.BaseType, d4.Default)
		}
		return err
	})
	if err != nil {
		t.Error(err)
	}
	if got := asPlainRole(t, ctx, s, plain, dump); got != want {
		t.Errorf("as a plain role the domains differ:\n%s\nwant\n%s", got, want)
	}
}
