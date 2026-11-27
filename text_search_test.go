package pgmo_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/radix29/pgmo"
	"github.com/radix29/pgmo/pgmotest"
)

// A text search family as the tests drive it: its read's column types, a
// row and the object it reads as (without its Database), the fragments its
// select holds, and its reads and Ref behind one shape.
type textSearchCase struct {
	noun      string
	match     string // tells the read apart in a fake
	types     []uint32
	row       []any
	want      any // the value the row reads as, Database cleared
	fields    int // want's field count, the back-pointer included
	fragments []string
	filter    string // the WHERE the test's filter becomes
	order     string
	byName    string // the by-name read's tail
	list      func(context.Context, *pgmo.Database, pgmo.ObjectFilter) ([]any, error)
	read      func(context.Context, *pgmo.Database, string, string) (any, error)
	ref       func(*pgmo.Database, string, string) any
	wantRef   any
}

// cleared copies an object of one of the families with its Database
// cleared, checking it first pointed to d.
func cleared(t *testing.T, v any, d *pgmo.Database) any {
	t.Helper()
	var back *pgmo.Database
	var out any
	switch x := v.(type) {
	case *pgmo.TextSearchParser:
		g := *x
		back = g.Database()
		g.SetDatabase(nil)
		out = g
	case *pgmo.TextSearchTemplate:
		g := *x
		back = g.Database()
		g.SetDatabase(nil)
		out = g
	case *pgmo.TextSearchDictionary:
		g := *x
		back = g.Database()
		g.SetDatabase(nil)
		out = g
	case *pgmo.TextSearchConfiguration:
		g := *x
		back = g.Database()
		g.SetDatabase(nil)
		out = g
	default:
		t.Fatalf("not a text search object: %T", v)
	}
	if back != d {
		t.Errorf("%T.Database() = %v, want %v", v, back, d)
	}
	return out
}

func anyOf[T any](xs []T, err error) ([]any, error) {
	out := make([]any, len(xs))
	for i, x := range xs {
		out[i] = x
	}
	return out, err
}

var textSearchCases = []textSearchCase{
	{
		noun:  "text search parser",
		match: "FROM pg_catalog.pg_ts_parser pr\n",
		types: []uint32{pgtype.OIDOID, pgtype.TextOID, pgtype.TextOID, pgtype.BoolOID, pgtype.TextOID,
			pgtype.TextArrayOID, pgtype.TextArrayOID, pgtype.TextArrayOID, pgtype.TextArrayOID, pgtype.TextArrayOID, pgtype.TextOID},
		row: []any{uint32(3722), "pg_catalog", "default", true, "x",
			[]string{"pg_catalog", "prsd_start"}, []string{"pg_catalog", "prsd_nexttoken"}, []string{"app", "prsd_end"},
			[]string{"pg_catalog", "prsd_lextype"}, []string{}, "default word parser"},
		want: pgmo.TextSearchParser{Origin: pgmo.Origin{IsSystem: true, Extension: "x"}, OID: 3722, Schema: "pg_catalog", Name: "default",
			Start: pgmo.ObjectName{Schema: "pg_catalog", Name: "prsd_start"}, Token: pgmo.ObjectName{Schema: "pg_catalog", Name: "prsd_nexttoken"},
			End: pgmo.ObjectName{Schema: "app", Name: "prsd_end"}, LexTypes: pgmo.ObjectName{Schema: "pg_catalog", Name: "prsd_lextype"},
			Comment: "default word parser"},
		fields: 11,
		fragments: []string{
			"SELECT pr.oid, n.nspname::pg_catalog.text, pr.prsname::pg_catalog.text,",
			"JOIN pg_catalog.pg_namespace n ON n.oid = pr.prsnamespace",
			"LEFT JOIN pg_catalog.pg_proc fst ON fst.oid = pr.prsstart::pg_catalog.oid",
			"LEFT JOIN pg_catalog.pg_proc ftok ON ftok.oid = pr.prstoken::pg_catalog.oid",
			"LEFT JOIN pg_catalog.pg_proc fend ON fend.oid = pr.prsend::pg_catalog.oid",
			"LEFT JOIN pg_catalog.pg_proc flex ON flex.oid = pr.prslextype::pg_catalog.oid",
			"LEFT JOIN pg_catalog.pg_proc fhl ON fhl.oid = pr.prsheadline::pg_catalog.oid",
			"d.classid = 'pg_catalog.pg_ts_parser'::pg_catalog.regclass AND d.objid = pr.oid",
			"LEFT JOIN pg_catalog.pg_description ds ON ds.objoid = pr.oid\n\tAND ds.classoid = 'pg_catalog.pg_ts_parser'::pg_catalog.regclass AND ds.objsubid = 0",
		},
		filter: "pr.prsname = $1::pg_catalog.text",
		order:  "ORDER BY n.nspname, pr.prsname",
		byName: "AND n.nspname = $1::pg_catalog.text AND pr.prsname = $2::pg_catalog.text",
		list: func(ctx context.Context, d *pgmo.Database, f pgmo.ObjectFilter) ([]any, error) {
			return anyOf(d.TextSearchParsers(ctx, f))
		},
		read: func(ctx context.Context, d *pgmo.Database, s, n string) (any, error) {
			return d.TextSearchParserByName(ctx, s, n)
		},
		ref:     func(d *pgmo.Database, s, n string) any { return d.TextSearchParserRef(s, n) },
		wantRef: pgmo.TextSearchParser{Schema: "app", Name: "p q"},
	},
	{
		noun:  "text search template",
		match: "FROM pg_catalog.pg_ts_template tm\n",
		types: []uint32{pgtype.OIDOID, pgtype.TextOID, pgtype.TextOID, pgtype.BoolOID, pgtype.TextOID,
			pgtype.TextArrayOID, pgtype.TextArrayOID, pgtype.TextOID},
		row: []any{uint32(70011), "app", "T t", false, "",
			[]string{}, []string{"pg_catalog", "dsimple_lexize"}, "the template"},
		want: pgmo.TextSearchTemplate{OID: 70011, Schema: "app", Name: "T t",
			Lexize: pgmo.ObjectName{Schema: "pg_catalog", Name: "dsimple_lexize"}, Comment: "the template"},
		fields: 8,
		fragments: []string{
			"SELECT tm.oid, n.nspname::pg_catalog.text, tm.tmplname::pg_catalog.text,",
			"JOIN pg_catalog.pg_namespace n ON n.oid = tm.tmplnamespace",
			"LEFT JOIN pg_catalog.pg_proc fin ON fin.oid = tm.tmplinit::pg_catalog.oid",
			"LEFT JOIN pg_catalog.pg_proc flx ON flx.oid = tm.tmpllexize::pg_catalog.oid",
			"d.classid = 'pg_catalog.pg_ts_template'::pg_catalog.regclass AND d.objid = tm.oid",
			"LEFT JOIN pg_catalog.pg_description ds ON ds.objoid = tm.oid\n\tAND ds.classoid = 'pg_catalog.pg_ts_template'::pg_catalog.regclass AND ds.objsubid = 0",
		},
		filter: "tm.tmplname = $1::pg_catalog.text",
		order:  "ORDER BY n.nspname, tm.tmplname",
		byName: "AND n.nspname = $1::pg_catalog.text AND tm.tmplname = $2::pg_catalog.text",
		list: func(ctx context.Context, d *pgmo.Database, f pgmo.ObjectFilter) ([]any, error) {
			return anyOf(d.TextSearchTemplates(ctx, f))
		},
		read: func(ctx context.Context, d *pgmo.Database, s, n string) (any, error) {
			return d.TextSearchTemplateByName(ctx, s, n)
		},
		ref:     func(d *pgmo.Database, s, n string) any { return d.TextSearchTemplateRef(s, n) },
		wantRef: pgmo.TextSearchTemplate{Schema: "app", Name: "p q"},
	},
	{
		noun:  "text search dictionary",
		match: "FROM pg_catalog.pg_ts_dict dc\n",
		types: []uint32{pgtype.OIDOID, pgtype.TextOID, pgtype.TextOID, pgtype.TextOID, pgtype.BoolOID, pgtype.TextOID,
			pgtype.TextOID, pgtype.TextOID, pgtype.TextOID, pgtype.TextOID},
		row: []any{uint32(70012), "app", "D d", "own", true, "citext",
			"pg_catalog", "snowball", "language = 'english', stopwords = 'english'", "stems"},
		want: pgmo.TextSearchDictionary{Origin: pgmo.Origin{IsSystem: true, Extension: "citext"}, OID: 70012, Schema: "app", Name: "D d",
			Owner: "own", Template: pgmo.ObjectName{Schema: "pg_catalog", Name: "snowball"},
			Options: "language = 'english', stopwords = 'english'", Comment: "stems"},
		fields: 9,
		fragments: []string{
			"SELECT dc.oid, n.nspname::pg_catalog.text, dc.dictname::pg_catalog.text,\n\tpg_catalog.pg_get_userbyid(dc.dictowner)::pg_catalog.text,",
			"tn.nspname::pg_catalog.text, tm.tmplname::pg_catalog.text,\n\tCOALESCE(dc.dictinitoption, ''),",
			"JOIN pg_catalog.pg_ts_template tm ON tm.oid = dc.dicttemplate",
			"d.classid = 'pg_catalog.pg_ts_dict'::pg_catalog.regclass AND d.objid = dc.oid",
			"LEFT JOIN pg_catalog.pg_description ds ON ds.objoid = dc.oid\n\tAND ds.classoid = 'pg_catalog.pg_ts_dict'::pg_catalog.regclass AND ds.objsubid = 0",
		},
		filter: "dc.dictname = $1::pg_catalog.text",
		order:  "ORDER BY n.nspname, dc.dictname",
		byName: "AND n.nspname = $1::pg_catalog.text AND dc.dictname = $2::pg_catalog.text",
		list: func(ctx context.Context, d *pgmo.Database, f pgmo.ObjectFilter) ([]any, error) {
			return anyOf(d.TextSearchDictionaries(ctx, f))
		},
		read: func(ctx context.Context, d *pgmo.Database, s, n string) (any, error) {
			return d.TextSearchDictionaryByName(ctx, s, n)
		},
		ref:     func(d *pgmo.Database, s, n string) any { return d.TextSearchDictionaryRef(s, n) },
		wantRef: pgmo.TextSearchDictionary{Schema: "app", Name: "p q"},
	},
	{
		noun:  "text search configuration",
		match: "FROM pg_catalog.pg_ts_config c\n",
		types: []uint32{pgtype.OIDOID, pgtype.TextOID, pgtype.TextOID, pgtype.TextOID, pgtype.BoolOID, pgtype.TextOID,
			pgtype.TextOID, pgtype.TextOID, pgtype.TextArrayOID, pgtype.TextArrayOID, pgtype.TextArrayOID, pgtype.TextOID},
		// Three mapping rows: asciiword's two dictionaries, then int's one.
		row: []any{uint32(70013), "app", "C c", "own", false, "",
			"pg_catalog", "default",
			[]string{"asciiword", "asciiword", "int"}, []string{"app", "pg_catalog", "pg_catalog"},
			[]string{"D d", "english_stem", "simple"}, "the config"},
		want: pgmo.TextSearchConfiguration{OID: 70013, Schema: "app", Name: "C c", Owner: "own",
			Parser: pgmo.ObjectName{Schema: "pg_catalog", Name: "default"},
			Mappings: []pgmo.TextSearchMapping{
				{TokenType: "asciiword", Dictionaries: []pgmo.ObjectName{{Schema: "app", Name: "D d"}, {Schema: "pg_catalog", Name: "english_stem"}}},
				{TokenType: "int", Dictionaries: []pgmo.ObjectName{{Schema: "pg_catalog", Name: "simple"}}},
			},
			Comment: "the config"},
		fields: 9,
		fragments: []string{
			"SELECT c.oid, n.nspname::pg_catalog.text, c.cfgname::pg_catalog.text,\n\tpg_catalog.pg_get_userbyid(c.cfgowner)::pg_catalog.text,",
			"pn.nspname::pg_catalog.text, pr.prsname::pg_catalog.text,\n\ttmap.tokens, tmap.schemas, tmap.names,",
			"JOIN pg_catalog.pg_ts_parser pr ON pr.oid = c.cfgparser",
			"pg_catalog.array_agg(COALESCE(tt.alias, m.maptokentype::pg_catalog.text) ORDER BY m.maptokentype, m.mapseqno)",
			"CROSS JOIN LATERAL pg_catalog.ts_token_type(p.oid) AS t) AS tt\n\t\tON tt.parser = mc.cfgparser AND tt.tokid = m.maptokentype",
			"GROUP BY m.mapcfg) AS tmap(cfg, tokens, schemas, names) ON tmap.cfg = c.oid",
			"d.classid = 'pg_catalog.pg_ts_config'::pg_catalog.regclass AND d.objid = c.oid",
			"LEFT JOIN pg_catalog.pg_description ds ON ds.objoid = c.oid\n\tAND ds.classoid = 'pg_catalog.pg_ts_config'::pg_catalog.regclass AND ds.objsubid = 0",
		},
		filter: "c.cfgname = $1::pg_catalog.text",
		order:  "ORDER BY n.nspname, c.cfgname",
		byName: "AND n.nspname = $1::pg_catalog.text AND c.cfgname = $2::pg_catalog.text",
		list: func(ctx context.Context, d *pgmo.Database, f pgmo.ObjectFilter) ([]any, error) {
			return anyOf(d.TextSearchConfigurations(ctx, f))
		},
		read: func(ctx context.Context, d *pgmo.Database, s, n string) (any, error) {
			return d.TextSearchConfigurationByName(ctx, s, n)
		},
		ref:     func(d *pgmo.Database, s, n string) any { return d.TextSearchConfigurationRef(s, n) },
		wantRef: pgmo.TextSearchConfiguration{Schema: "app", Name: "p q"},
	},
}

// Each family's listing fills every field, pushes its filter into the
// query, orders by schema then name, and goes to the database's own pool;
// its by-name read binds the names, refuses an empty schema unsent, and
// tells a missing object from a failed read; its Ref reads nothing.
func TestTextSearchFamilies(t *testing.T) {
	yes := true
	for _, c := range textSearchCases {
		t.Run(c.noun, func(t *testing.T) {
			if n := reflect.TypeOf(c.want).NumField(); n != c.fields {
				t.Errorf("%T has %d fields; this test checks %d — add the new one", c.want, n, c.fields)
			}
			f := pgmotest.New(t)
			other := f.Database("appdb")
			other.On(c.match, rowsOf(c.types, c.row))
			d := f.Server().DatabaseRef("appdb")
			got, err := c.list(t.Context(), d, pgmo.ObjectFilter{
				Name:            []pgmo.TextCriterion{{Op: pgmo.TextEquals, Value: "X"}},
				Schema:          []pgmo.TextCriterion{{Op: pgmo.TextEquals, Value: "app"}},
				System:          &yes,
				ExtensionMember: &yes,
			})
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != 1 || !reflect.DeepEqual(cleared(t, got[0], d), c.want) {
				t.Errorf("listing =\n%+v\nwant\n%+v", got, c.want)
			}
			if n := len(f.Queries()); n != 1 {
				t.Errorf("%d statements on the Server's own database, want 1: the listing went to appdb", n)
			}
			last := lastQuery(other)
			for _, w := range c.fragments {
				if !strings.Contains(last.Text, w) {
					t.Errorf("no %q in\n%s", w, last.Text)
				}
			}
			if !strings.Contains(last.Text, "WHERE true AND "+c.filter+" AND n.nspname = $2::pg_catalog.text"+
				" AND (pg_catalog.starts_with(n.nspname, 'pg_') OR n.nspname = 'information_schema') AND EXISTS (") ||
				!strings.HasSuffix(last.Text, "\n"+c.order) || len(last.Args) != 2 || last.Arg(0) != "x" || last.Arg(1) != "app" {
				t.Errorf("filter not pushed down, then ordered: args %v\n%s", last.Args, last.Text)
			}

			one, err := c.read(t.Context(), d, "app", "X y")
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(cleared(t, one, d), c.want) {
				t.Errorf("by name = %+v, want %+v", one, c.want)
			}
			last = lastQuery(other)
			if !strings.HasSuffix(last.Text, "WHERE true\n\t"+c.byName) || last.Arg(0) != "app" || last.Arg(1) != "X y" {
				t.Errorf("names not bound as $1, $2: args %v\n%s", last.Args, last.Text)
			}
			n := len(other.Queries())
			if _, err := c.read(t.Context(), d, "", "X y"); !errors.Is(err, pgmo.ErrSchemaRequired) {
				t.Errorf("no schema: err = %v, want ErrSchemaRequired", err)
			}
			if len(other.Queries()) != n {
				t.Errorf("a refused read was sent")
			}

			f = pgmotest.New(t)
			f.On(c.match, rowsOf(c.types))
			d = f.Server().DatabaseRef(pgmotest.DefaultDatabase)
			if _, err := c.read(t.Context(), d, "app", "Gone"); !errors.Is(err, pgmo.ErrNotFound) ||
				!strings.Contains(err.Error(), c.noun+` app."Gone" not found in database "`+pgmotest.DefaultDatabase+`"`) {
				t.Errorf("missing: err = %v, want ErrNotFound naming it", err)
			}
			f = pgmotest.New(t)
			f.On(c.match, pgmotest.Fail(&pgconn.PgError{Code: "57014", Message: "canceled"}))
			d = f.Server().DatabaseRef(pgmotest.DefaultDatabase)
			_, err = c.read(t.Context(), d, "app", "x")
			if err == nil || errors.Is(err, pgmo.ErrNotFound) || !strings.HasPrefix(err.Error(), "pgmo: read "+c.noun+" app.x of database ") ||
				strings.Count(err.Error(), "pgmo: ") != 1 {
				t.Errorf("server error: err = %v, want it wrapped once naming the read", err)
			}
			_, err = c.list(t.Context(), d, pgmo.ObjectFilter{})
			if err == nil || !strings.HasPrefix(err.Error(), "pgmo: list "+c.noun) || strings.Count(err.Error(), "pgmo: ") != 1 {
				t.Errorf("listing, server error: err = %v, want it wrapped once naming the read", err)
			}
			n = len(f.Queries())
			if _, err := c.list(t.Context(), d, pgmo.ObjectFilter{Name: []pgmo.TextCriterion{{Op: 99, Value: "x"}}}); !errors.Is(err, pgmo.ErrInvalidRequest) {
				t.Errorf("unknown operator: err = %v, want ErrInvalidRequest", err)
			}
			if r := c.ref(d, "app", "p q"); !reflect.DeepEqual(cleared(t, r, d), c.wantRef) {
				t.Errorf("Ref = %+v", r)
			}
			if len(f.Queries()) != n {
				t.Errorf("a refused filter or a Ref sent a statement")
			}
		})
	}
}

// A configuration's mapping arrays that do not line up are refused, not
// read as a shorter list.
func TestTextSearchConfigurationMappingArraysMustLineUp(t *testing.T) {
	c := textSearchCases[3]
	row := slicesClone(c.row)
	row[9] = []string{"app"}
	f := pgmotest.New(t)
	f.On(c.match, rowsOf(c.types, row))
	_, err := f.Server().DatabaseRef(pgmotest.DefaultDatabase).TextSearchConfigurationByName(t.Context(), "app", "C c")
	if err == nil || !strings.Contains(err.Error(), "mapping arrays of different lengths: 3 token types, 1 schemas, 3 names") {
		t.Errorf("err = %v, want the arrays refused", err)
	}
}

func slicesClone(s []any) []any { return append([]any(nil), s...) }

// The qualified names quote each part as needed.
func TestTextSearchQualifiedNames(t *testing.T) {
	d := pgmotest.New(t).Server().DatabaseRef(pgmotest.DefaultDatabase)
	for got, want := range map[string]string{
		d.TextSearchParserRef("app", "P p").QualifiedName():      `app."P p"`,
		d.TextSearchTemplateRef("A a", "t").QualifiedName():      `"A a".t`,
		d.TextSearchDictionaryRef("app", "d").QualifiedName():    `app.d`,
		d.TextSearchConfigurationRef("app", "C").QualifiedName(): `app."C"`,
	} {
		if got != want {
			t.Errorf("QualifiedName() = %s, want %s", got, want)
		}
	}
}
