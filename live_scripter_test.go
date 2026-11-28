//go:build livedb

package pgmo

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/radix29/pgmo/pgsql"
)

// liveScripter is TestLiveScripter's fixture: a throwaway database holding
// one object of every scripted family, two roles they are owned by and
// granted to, a Server connected to postgres scripting the database through
// its pool (the render transaction on another database), and an admin
// connection to the database.
type liveScripter struct {
	t          *testing.T
	ctx        context.Context
	admin      *pgx.Conn // on postgres, for databases, roles, tablespaces
	conn       *pgx.Conn // on the throwaway database
	s          *Server
	db         string
	own, own2  string
	role, tbsp string
	num        int
}

func (l *liveScripter) exec(conn *pgx.Conn, stmts ...string) {
	l.t.Helper()
	for _, stmt := range stmts {
		if _, err := conn.Exec(l.ctx, stmt); err != nil {
			l.t.Fatalf("%s: %v", stmt, err)
		}
	}
}

func (l *liveScripter) oid(query string) OID {
	l.t.Helper()
	var o OID
	if err := l.conn.QueryRow(l.ctx, query).Scan(&o); err != nil {
		l.t.Fatalf("%s: %v", query, err)
	}
	return o
}

// connectDB (re)opens the admin connection to the throwaway database.
func (l *liveScripter) connectDB() {
	l.t.Helper()
	cfg, err := pgx.ParseConfig(*liveConnString)
	if err != nil {
		l.t.Fatal(err)
	}
	cfg.Database = l.db
	if l.conn, err = pgx.ConnectConfig(l.ctx, cfg); err != nil {
		l.t.Fatal(err)
	}
}

func newLiveScripter(t *testing.T) *liveScripter {
	pool, ctx, done := liveDBTimeout(t, 4*time.Minute)
	t.Cleanup(done)
	stamp := time.Now().UnixNano()
	l := &liveScripter{t: t, ctx: ctx,
		db:   fmt.Sprintf(`pgmo_live_script_%d "D"`, stamp),
		own:  fmt.Sprintf("pgmo_live_script_%d_own", stamp),
		own2: fmt.Sprintf("pgmo_live_script_%d Own'2", stamp),
		role: fmt.Sprintf("pgmo_live_script_%d_r", stamp),
		tbsp: fmt.Sprintf("pgmo_live_script_%d_ts", stamp),
	}
	q := pgsql.QuoteIdent
	cfg, err := pgx.ParseConfig(*liveConnString)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Database = "postgres"
	if l.admin, err = pgx.ConnectConfig(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		bg := context.Background()
		for _, stmt := range []string{"DROP DATABASE IF EXISTS " + q(l.db) + " WITH (FORCE)",
			"DROP DATABASE IF EXISTS " + q(l.db+"2") + " WITH (FORCE)",
			"DROP TABLESPACE IF EXISTS " + q(l.tbsp),
			"DROP ROLE IF EXISTS " + q(l.role), "DROP ROLE IF EXISTS " + q(l.own), "DROP ROLE IF EXISTS " + q(l.own2)} {
			if _, err := pool.Exec(bg, stmt); err != nil {
				t.Errorf("%s: %v", stmt, err)
			}
		}
		l.admin.Close(bg)
	})
	l.exec(l.admin, "CREATE DATABASE "+q(l.db), "CREATE ROLE "+q(l.own), "CREATE ROLE "+q(l.own2))
	l.connectDB()
	t.Cleanup(func() { l.conn.Close(context.Background()) })
	if l.s, err = Connect(ctx, ConnectionOptions{ConnString: *liveConnString, Database: "postgres",
		ApplicationName: "pgmo - live scripter"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(l.s.Close)
	l.num = int(l.s.Version())
	return l
}

// fixture creates one object of every database-scoped family the
// Scripter scripts, with the options each script has to restate.
func (l *liveScripter) fixture() {
	own, own2 := pgsql.QuoteIdent(l.own), pgsql.QuoteIdent(l.own2)
	l.exec(l.conn,
		`CREATE SCHEMA "S s" AUTHORIZATION `+own,
		`COMMENT ON SCHEMA "S s" IS 'schema''s'`,
		`GRANT USAGE ON SCHEMA "S s" TO `+own2,
		`ALTER DEFAULT PRIVILEGES FOR ROLE `+own+` IN SCHEMA "S s" GRANT SELECT ON TABLES TO `+own2,
		`CREATE EXTENSION citext WITH SCHEMA "S s"`,
		`CREATE EXTENSION btree_gist`,
		`CREATE COLLATION "S s".ci (provider = icu, locale = 'und-u-ks-level2', deterministic = false)`,
		`CREATE COLLATION "S s".cc (lc_collate = 'C', lc_ctype = 'POSIX')`,
		`CREATE TYPE "S s".mood AS ENUM ('sad', 'o''k', 'happy')`,
		`CREATE TYPE "S s".pair AS (a integer, "B" text COLLATE "C")`,
		`CREATE TYPE "S s".fr AS RANGE (subtype = float8, subtype_diff = float8mi)`,
		`CREATE TYPE "S s".shell`,
		`ALTER TYPE "S s".mood OWNER TO `+own,
		`COMMENT ON TYPE "S s".mood IS 'feelings'`,
		`REVOKE USAGE ON TYPE "S s".pair FROM PUBLIC`,
		`CREATE DOMAIN "S s".pos AS integer DEFAULT 1 NOT NULL CONSTRAINT pos_check CHECK (VALUE > 0)`,
		`ALTER DOMAIN "S s".pos ADD CONSTRAINT pos_small CHECK (VALUE < 1000000) NOT VALID`,
		`COMMENT ON DOMAIN "S s".pos IS 'positive'`,
		`CREATE SEQUENCE "S s".seq AS integer START 5 INCREMENT 2 MINVALUE 1 MAXVALUE 1000 CACHE 3 CYCLE`,
		`GRANT USAGE ON SEQUENCE "S s".seq TO `+own2,
		`CREATE FUNCTION "S s".add1(i integer) RETURNS integer LANGUAGE sql IMMUTABLE STRICT AS 'SELECT i + 1'`,
		`REVOKE ALL ON FUNCTION "S s".add1(integer) FROM PUBLIC`,
		`GRANT EXECUTE ON FUNCTION "S s".add1(integer) TO `+own2+` WITH GRANT OPTION`,
		`COMMENT ON FUNCTION "S s".add1(integer) IS 'adds one'`,
		`CREATE FUNCTION "S s".twice(a int) RETURNS int LANGUAGE sql BEGIN ATOMIC SELECT a * 2; END`,
		`CREATE FUNCTION "S s".trg() RETURNS trigger LANGUAGE plpgsql AS $$BEGIN RETURN NEW; END$$`,
		`CREATE FUNCTION "S s".evtf() RETURNS event_trigger LANGUAGE plpgsql AS $$BEGIN END$$`,
		`CREATE FUNCTION "S s".pair_text("S s".pair) RETURNS text LANGUAGE sql AS 'SELECT $1::text'`,
		`CREATE PROCEDURE "S s".p(INOUT x integer) LANGUAGE plpgsql SET work_mem = '8MB' AS $$BEGIN x := x + 1; END$$`,
		`ALTER PROCEDURE "S s".p(integer) OWNER TO `+own,
		`CREATE AGGREGATE "S s".mysum(integer) (SFUNC = int4pl, STYPE = integer, INITCOND = '0', PARALLEL = SAFE)`,
		`CREATE AGGREGATE "S s".cnt(*) (SFUNC = int8inc, STYPE = bigint, INITCOND = '0')`,
		`CREATE AGGREGATE "S s".pct(float8 ORDER BY float8) (SFUNC = ordered_set_transition, STYPE = internal,
			FINALFUNC = percentile_cont_float8_final)`,
		`COMMENT ON AGGREGATE "S s".cnt(*) IS 'counts'`,
		`CREATE OPERATOR "S s".=== (FUNCTION = int4eq, LEFTARG = integer, RIGHTARG = integer,
			COMMUTATOR = OPERATOR("S s".===), RESTRICT = eqsel, JOIN = eqjoinsel, MERGES)`,
		`CREATE OPERATOR "S s".!! (FUNCTION = int4um, RIGHTARG = integer)`,
		`CREATE CAST ("S s".mood AS integer) WITH INOUT AS ASSIGNMENT`,
		`CREATE CAST ("S s".pair AS text) WITH FUNCTION "S s".pair_text("S s".pair)`,
		`COMMENT ON CAST ("S s".pair AS text) IS 'pair as text'`,
		`CREATE TEXT SEARCH PARSER "S s".prs (START = prsd_start, GETTOKEN = prsd_nexttoken, END = prsd_end,
			LEXTYPES = prsd_lextype, HEADLINE = prsd_headline)`,
		`CREATE TEXT SEARCH TEMPLATE "S s".tmpl (INIT = dsimple_init, LEXIZE = dsimple_lexize)`,
		`CREATE TEXT SEARCH DICTIONARY "S s".dict (TEMPLATE = snowball, language = 'english', stopwords = 'english')`,
		`CREATE TEXT SEARCH CONFIGURATION "S s".cfg (PARSER = "S s".prs)`,
		`ALTER TEXT SEARCH CONFIGURATION "S s".cfg ADD MAPPING FOR asciiword WITH "S s".dict, english_stem`,
		`ALTER TEXT SEARCH CONFIGURATION "S s".cfg ADD MAPPING FOR int WITH simple`,
		`CREATE LANGUAGE plx HANDLER plpgsql_call_handler INLINE plpgsql_inline_handler VALIDATOR plpgsql_validator`,
		`COMMENT ON LANGUAGE plx IS 'another plpgsql'`,
		`CREATE TABLE "S s".parent (id integer GENERATED ALWAYS AS IDENTITY PRIMARY KEY, label text)`,
		`CREATE TABLE "S s".t (
			id integer GENERATED BY DEFAULT AS IDENTITY,
			name text COLLATE "C" NOT NULL DEFAULT 'x''y',
			m "S s".mood,
			p "S s".pos,
			n numeric(10,2) CHECK (n >= 0),
			g integer GENERATED ALWAYS AS (id * 2) STORED,
			r int4range,
			parent_id integer,
			CONSTRAINT t_pk PRIMARY KEY (id),
			CONSTRAINT t_name_uq UNIQUE (name),
			CONSTRAINT t_excl EXCLUDE USING gist (r WITH &&),
			CONSTRAINT t_fk FOREIGN KEY (parent_id) REFERENCES "S s".parent (id) ON DELETE CASCADE DEFERRABLE INITIALLY DEFERRED
		) WITH (fillfactor = 70)`,
		`ALTER TABLE "S s".t ADD CONSTRAINT t_nv CHECK (id < 1000000) NOT VALID`,
		`ALTER TABLE "S s".t ALTER COLUMN name SET STATISTICS 500`,
		`CREATE INDEX t_lower ON "S s".t (lower(name)) INCLUDE (m) WHERE n > 0`,
		`CREATE INDEX t_cl ON "S s".t (p) WITH (fillfactor = 50)`,
		`ALTER TABLE "S s".t CLUSTER ON t_cl`,
		`ALTER TABLE "S s".t ENABLE ROW LEVEL SECURITY`,
		`ALTER TABLE "S s".t FORCE ROW LEVEL SECURITY`,
		`CREATE POLICY pol ON "S s".t AS RESTRICTIVE FOR SELECT TO `+own+`, `+own2+` USING (name <> 'secret')`,
		`CREATE POLICY pol2 ON "S s".t USING (true) WITH CHECK (n > 0)`,
		`CREATE TRIGGER trg BEFORE INSERT OR UPDATE OF name ON "S s".t FOR EACH ROW WHEN (NEW.name IS NOT NULL)
			EXECUTE FUNCTION "S s".trg('a\b', 'x')`,
		`ALTER TABLE "S s".t DISABLE TRIGGER trg`,
		`COMMENT ON TABLE "S s".t IS 'the table'`,
		`COMMENT ON COLUMN "S s".t.name IS 'its name'`,
		`COMMENT ON CONSTRAINT t_fk ON "S s".t IS 'to parent'`,
		`COMMENT ON INDEX "S s".t_lower IS 'by lower name'`,
		`COMMENT ON TRIGGER trg ON "S s".t IS 'fires'`,
		`COMMENT ON POLICY pol ON "S s".t IS 'hides'`,
		`ALTER TABLE "S s".t OWNER TO `+own,
		`GRANT SELECT, INSERT ON "S s".t TO `+own2,
		`GRANT UPDATE (name) ON "S s".t TO `+own2+` WITH GRANT OPTION`,
		`REVOKE TRUNCATE ON "S s".t FROM `+own,
		`CREATE UNLOGGED TABLE "S s".u (a int)`,
		`CREATE TABLE "S s".meas (at date NOT NULL, v int, CONSTRAINT meas_v CHECK (v > 0)) PARTITION BY RANGE (at)`,
		`CREATE INDEX meas_at ON "S s".meas (at)`,
		`CREATE TABLE "S s".meas_2026 PARTITION OF "S s".meas FOR VALUES FROM ('2026-01-01') TO ('2027-01-01')`,
		`CREATE TABLE "S s".base (k int NOT NULL, note text)`,
		`CREATE TABLE "S s".child (extra text) INHERITS ("S s".base)`,
		`CREATE VIEW "S s".v WITH (security_barrier = true) AS SELECT id, name FROM "S s".t WHERE n > 0 WITH LOCAL CHECK OPTION`,
		`ALTER VIEW "S s".v ALTER COLUMN name SET DEFAULT 'dflt'`,
		`COMMENT ON COLUMN "S s".v.id IS 'view id'`,
		`CREATE MATERIALIZED VIEW "S s".mv AS SELECT id, "S s".add1(id) AS a FROM "S s".t WITH NO DATA`,
		`CREATE UNIQUE INDEX mv_id ON "S s".mv (id)`,
		`CREATE FOREIGN DATA WRAPPER fdw OPTIONS (debug 'true')`,
		`CREATE SERVER srv TYPE 'x' VERSION '1' FOREIGN DATA WRAPPER fdw OPTIONS (host 'h')`,
		`GRANT USAGE ON FOREIGN SERVER srv TO `+own2,
		`CREATE USER MAPPING FOR `+own+` SERVER srv OPTIONS (user 'u')`,
		`CREATE USER MAPPING FOR PUBLIC SERVER srv`,
		`CREATE FOREIGN TABLE "S s".ft (a integer OPTIONS (col 'c') NOT NULL, b text CHECK (b <> '')) SERVER srv OPTIONS (tbl 't')`,
		`CREATE EVENT TRIGGER evt ON ddl_command_end WHEN TAG IN ('CREATE TABLE', 'ALTER TABLE') EXECUTE FUNCTION "S s".evtf()`,
		`ALTER EVENT TRIGGER evt DISABLE`,
	)
}

// scripted is one object's script, by what names it.
type scripted struct {
	what   string
	script func(*Scripter) (string, error)
}

// objects are the fixture's objects in creation order, each with the call
// scripting it.
func (l *liveScripter) objects() []scripted {
	s := func(what string, f func(sc *Scripter) (string, error)) scripted { return scripted{what, f} }
	ctx := l.ctx
	// Type OIDs change when the database is replayed: read at each call.
	mood := func() OID { return l.oid(`SELECT '"S s".mood'::pg_catalog.regtype::pg_catalog.oid`) }
	pair := func() OID { return l.oid(`SELECT '"S s".pair'::pg_catalog.regtype::pg_catalog.oid`) }
	return []scripted{
		s("schema", func(sc *Scripter) (string, error) { return sc.ScriptSchema(ctx, "S s") }),
		s("extension citext", func(sc *Scripter) (string, error) { return sc.ScriptExtension(ctx, "citext") }),
		s("extension btree_gist", func(sc *Scripter) (string, error) { return sc.ScriptExtension(ctx, "btree_gist") }),
		s("collation ci", func(sc *Scripter) (string, error) { return sc.ScriptCollation(ctx, "S s", "ci") }),
		s("collation cc", func(sc *Scripter) (string, error) { return sc.ScriptCollation(ctx, "S s", "cc") }),
		s("type mood", func(sc *Scripter) (string, error) { return sc.ScriptType(ctx, "S s", "mood") }),
		s("type pair", func(sc *Scripter) (string, error) { return sc.ScriptType(ctx, "S s", "pair") }),
		s("type fr", func(sc *Scripter) (string, error) { return sc.ScriptType(ctx, "S s", "fr") }),
		s("type fr_multirange", func(sc *Scripter) (string, error) { return sc.ScriptType(ctx, "S s", "fr_multirange") }),
		s("type shell", func(sc *Scripter) (string, error) { return sc.ScriptType(ctx, "S s", "shell") }),
		s("domain", func(sc *Scripter) (string, error) { return sc.ScriptDomain(ctx, "S s", "pos") }),
		s("sequence", func(sc *Scripter) (string, error) { return sc.ScriptSequence(ctx, "S s", "seq") }),
		s("function add1", func(sc *Scripter) (string, error) { return sc.ScriptFunction(ctx, "S s", "add1", nil) }),
		s("function twice", func(sc *Scripter) (string, error) { return sc.ScriptFunction(ctx, "S s", "twice", nil) }),
		s("function trg", func(sc *Scripter) (string, error) { return sc.ScriptFunction(ctx, "S s", "trg", nil) }),
		s("function evtf", func(sc *Scripter) (string, error) { return sc.ScriptFunction(ctx, "S s", "evtf", nil) }),
		s("function pair_text", func(sc *Scripter) (string, error) { return sc.ScriptFunction(ctx, "S s", "pair_text", nil) }),
		s("procedure", func(sc *Scripter) (string, error) { return sc.ScriptProcedure(ctx, "S s", "p", nil) }),
		s("aggregate mysum", func(sc *Scripter) (string, error) { return sc.ScriptAggregate(ctx, "S s", "mysum", nil) }),
		s("aggregate cnt", func(sc *Scripter) (string, error) { return sc.ScriptAggregate(ctx, "S s", "cnt", nil) }),
		s("aggregate pct", func(sc *Scripter) (string, error) { return sc.ScriptAggregate(ctx, "S s", "pct", nil) }),
		s("operator ===", func(sc *Scripter) (string, error) { return sc.ScriptOperator(ctx, "S s", "===", 23, 23) }),
		s("operator !!", func(sc *Scripter) (string, error) { return sc.ScriptOperator(ctx, "S s", "!!", 0, 23) }),
		s("cast mood", func(sc *Scripter) (string, error) { return sc.ScriptCast(ctx, mood(), 23) }),
		s("cast pair", func(sc *Scripter) (string, error) { return sc.ScriptCast(ctx, pair(), 25) }),
		s("ts parser", func(sc *Scripter) (string, error) { return sc.ScriptTextSearchParser(ctx, "S s", "prs") }),
		s("ts template", func(sc *Scripter) (string, error) { return sc.ScriptTextSearchTemplate(ctx, "S s", "tmpl") }),
		s("ts dictionary", func(sc *Scripter) (string, error) { return sc.ScriptTextSearchDictionary(ctx, "S s", "dict") }),
		s("ts configuration", func(sc *Scripter) (string, error) {
			return sc.ScriptTextSearchConfiguration(ctx, "S s", "cfg")
		}),
		s("language", func(sc *Scripter) (string, error) { return sc.ScriptLanguage(ctx, "plx") }),
		s("table parent", func(sc *Scripter) (string, error) { return sc.ScriptTable(ctx, "S s", "parent") }),
		s("table t", func(sc *Scripter) (string, error) { return sc.ScriptTable(ctx, "S s", "t") }),
		s("identity sequence", func(sc *Scripter) (string, error) { return sc.ScriptSequence(ctx, "S s", "t_id_seq") }),
		s("table u", func(sc *Scripter) (string, error) { return sc.ScriptTable(ctx, "S s", "u") }),
		s("table meas", func(sc *Scripter) (string, error) { return sc.ScriptTable(ctx, "S s", "meas") }),
		s("table meas_2026", func(sc *Scripter) (string, error) { return sc.ScriptTable(ctx, "S s", "meas_2026") }),
		s("table base", func(sc *Scripter) (string, error) { return sc.ScriptTable(ctx, "S s", "base") }),
		s("table child", func(sc *Scripter) (string, error) { return sc.ScriptTable(ctx, "S s", "child") }),
		s("view", func(sc *Scripter) (string, error) { return sc.ScriptView(ctx, "S s", "v") }),
		s("materialized view", func(sc *Scripter) (string, error) { return sc.ScriptMaterializedView(ctx, "S s", "mv") }),
		s("fdw", func(sc *Scripter) (string, error) { return sc.ScriptForeignDataWrapper(ctx, "fdw") }),
		s("server", func(sc *Scripter) (string, error) { return sc.ScriptForeignServer(ctx, "srv") }),
		s("user mapping", func(sc *Scripter) (string, error) { return sc.ScriptUserMapping(ctx, "srv", l.own) }),
		s("public user mapping", func(sc *Scripter) (string, error) { return sc.ScriptUserMapping(ctx, "srv", "") }),
		s("foreign table", func(sc *Scripter) (string, error) { return sc.ScriptForeignTable(ctx, "S s", "ft") }),
		s("event trigger", func(sc *Scripter) (string, error) { return sc.ScriptEventTrigger(ctx, "evt") }),
	}
}

// scriptAll scripts every object with opts.
func (l *liveScripter) scriptAll(objs []scripted, opts ScriptOptions) []string {
	l.t.Helper()
	sc := NewScripter(l.s.DatabaseRef(l.db), opts)
	out := make([]string, len(objs))
	for i, o := range objs {
		var err error
		if out[i], err = o.script(sc); err != nil {
			l.t.Fatalf("script %s: %v", o.what, err)
		}
	}
	return out
}

// recreate drops the throwaway database and creates it empty.
func (l *liveScripter) recreate() {
	l.t.Helper()
	l.conn.Close(l.ctx)
	q := pgsql.QuoteIdent(l.db)
	l.exec(l.admin, "DROP DATABASE "+q+" WITH (FORCE)", "CREATE DATABASE "+q)
	l.connectDB()
	// The drop ended the Server's pooled sessions on the database: a
	// Server of its own from here.
	s, err := Connect(l.ctx, ConnectionOptions{ConnString: *liveConnString, Database: "postgres",
		ApplicationName: "pgmo - live scripter"})
	if err != nil {
		l.t.Fatal(err)
	}
	l.t.Cleanup(s.Close)
	l.s = s
}

// TestLiveScripter scripts one object of every family, runs the scripts
// into the database emptied, and scripts them again: every script must run
// and come back unchanged — the objects are restated whole, as pgmo reads
// them. Then the DROP scripts, run in reverse, leave nothing; DROP AND
// CREATE runs on the objects in place. The Server scripting is connected to
// postgres, so every read goes through the render transaction on another
// database.
func TestLiveScripter(t *testing.T) {
	l := newLiveScripter(t)
	l.fixture()
	objs := l.objects()
	before := l.scriptAll(objs, DefaultScriptOptions())
	t.Logf("table t:\n%s", before[slicesIndex(objs, "table t")])

	l.recreate()
	l.exec(l.conn, strings.Join(before, "\n"))
	after := l.scriptAll(objs, DefaultScriptOptions())
	for i := range objs {
		if after[i] != before[i] {
			t.Errorf("%s: script after replay differs:\nbefore:\n%s\nafter:\n%s", objs[i].what, before[i], after[i])
		}
	}

	// DROP AND CREATE with IF EXISTS replays over the objects in place.
	dc := DefaultScriptOptions()
	dc.Verb, dc.IfExists, dc.Cascade = ScriptDropAndCreate, true, true
	again := l.scriptAll(objs, dc)
	for i, s := range again {
		if !strings.HasPrefix(s, "DROP ") && !strings.HasPrefix(s, "-- ") {
			t.Errorf("%s: DROP AND CREATE does not start with the DROP:\n%s", objs[i].what, s)
		}
	}
	// Run in order, each object's DROP … CASCADE takes what depends on it
	// with it — the schema's its contents — and the scripts after it put
	// them back.
	for _, s := range again {
		l.exec(l.conn, s)
	}
	for i, s := range l.scriptAll(objs, DefaultScriptOptions()) {
		if s != before[i] {
			t.Errorf("%s: script after DROP AND CREATE differs:\nbefore:\n%s\nafter:\n%s", objs[i].what, before[i], s)
		}
	}

	// DROP, run in reverse, drops everything the scripts created.
	drop := DefaultScriptOptions()
	drop.Verb = ScriptDrop
	drops := l.scriptAll(objs, drop)
	for i := len(drops) - 1; i >= 0; i-- {
		if strings.HasPrefix(drops[i], "-- ") {
			continue // an extension member or an identity sequence: a note
		}
		l.exec(l.conn, drops[i])
	}
	var left int
	if err := l.conn.QueryRow(l.ctx, `SELECT pg_catalog.count(*) FROM pg_catalog.pg_namespace WHERE nspname = 'S s'`).Scan(&left); err != nil || left != 0 {
		t.Errorf("after the DROP scripts: schema left %d, %v", left, err)
	}
}

func slicesIndex(objs []scripted, what string) int {
	for i, o := range objs {
		if o.what == what {
			return i
		}
	}
	return -1
}

// TestLiveScripterServerObjects round-trips a role, a tablespace and a
// database the same way: scripted, dropped, the script run, scripted again
// unchanged.
func TestLiveScripterServerObjects(t *testing.T) {
	l := newLiveScripter(t)
	q := pgsql.QuoteIdent
	l.exec(l.admin,
		"CREATE ROLE "+q(l.role)+" LOGIN CREATEDB CONNECTION LIMIT 3 VALID UNTIL '2100-01-01 00:00:00+00'",
		"ALTER ROLE "+q(l.role)+" SET work_mem = '16MB'",
		"ALTER ROLE "+q(l.role)+" SET search_path = 'a', \"B b\"",
		"ALTER ROLE "+q(l.role)+" IN DATABASE "+q(l.db)+" SET statement_timeout = '5s'",
		"GRANT "+q(l.own)+" TO "+q(l.role)+" WITH ADMIN OPTION",
		"GRANT "+q(l.role)+" TO "+q(l.own2),
		"COMMENT ON ROLE "+q(l.role)+" IS 'scripted role'",
		"SET allow_in_place_tablespaces = on",
		"CREATE TABLESPACE "+q(l.tbsp)+" LOCATION '' WITH (random_page_cost = 1.5)",
		"RESET allow_in_place_tablespaces",
		"ALTER TABLESPACE "+q(l.tbsp)+" OWNER TO "+q(l.own),
		"GRANT CREATE ON TABLESPACE "+q(l.tbsp)+" TO "+q(l.own2),
		"COMMENT ON TABLESPACE "+q(l.tbsp)+" IS 'in place'",
		"CREATE DATABASE "+q(l.db+"2")+" ENCODING 'UTF8' TEMPLATE template0 CONNECTION LIMIT 7 TABLESPACE "+q(l.tbsp),
		"ALTER DATABASE "+q(l.db+"2")+" SET work_mem = '32MB'",
		"ALTER DATABASE "+q(l.db+"2")+" OWNER TO "+q(l.own),
		"COMMENT ON DATABASE "+q(l.db+"2")+" IS 'second'",
	)
	ssc := NewServerScripter(l.s, DefaultScriptOptions())
	dsc := NewScripter(l.s.DatabaseRef(l.db+"2"), DefaultScriptOptions())
	scriptRole := func() string {
		s, err := ssc.ScriptRole(l.ctx, l.role)
		if err != nil {
			t.Fatalf("ScriptRole: %v", err)
		}
		return s
	}
	scriptTablespace := func() string {
		s, err := ssc.ScriptTablespace(l.ctx, l.tbsp)
		if err != nil {
			t.Fatalf("ScriptTablespace: %v", err)
		}
		return s
	}
	scriptDatabase := func() string {
		s, err := dsc.ScriptDatabase(l.ctx)
		if err != nil {
			t.Fatalf("ScriptDatabase: %v", err)
		}
		return s
	}
	role, tbsp, db := scriptRole(), scriptTablespace(), scriptDatabase()
	t.Logf("role:\n%s\ntablespace:\n%s\ndatabase:\n%s", role, tbsp, db)

	// Each statement on its own: CREATE DATABASE and CREATE TABLESPACE refuse
	// a transaction block, which a multi-statement string is.
	run := func(script string) {
		t.Helper()
		for _, stmt := range splitScript(script) {
			l.exec(l.admin, stmt)
		}
	}
	l.exec(l.admin, "DROP DATABASE "+q(l.db+"2"))
	l.exec(l.admin, "DROP TABLESPACE "+q(l.tbsp))
	l.exec(l.admin, "DROP ROLE "+q(l.role))
	run(role)
	l.exec(l.admin, "SET allow_in_place_tablespaces = on")
	run(tbsp)
	l.exec(l.admin, "RESET allow_in_place_tablespaces")
	run(db)
	if got := scriptRole(); got != role {
		t.Errorf("role after replay:\n%s\nwant\n%s", got, role)
	}
	if got := scriptTablespace(); got != tbsp {
		t.Errorf("tablespace after replay:\n%s\nwant\n%s", got, tbsp)
	}
	if got := scriptDatabase(); got != db {
		t.Errorf("database after replay:\n%s\nwant\n%s", got, db)
	}
}

// splitScript splits a Scripter's script into its statements, dropping
// notes: each item is separated by a blank line and ends in ";".
func splitScript(script string) []string {
	var out []string
	for _, item := range strings.Split(script, "\n\n") {
		item = strings.TrimSpace(item)
		if item == "" || strings.HasPrefix(item, "-- ") {
			continue
		}
		out = append(out, item)
	}
	return out
}

// TestLiveScripterInTransaction scripts a table created inside the caller's
// InTransaction, uncommitted: the render read is a savepoint of that
// transaction, sees the table, and leaves the transaction's search_path as
// it was. A database other than the transaction's is refused.
func TestLiveScripterInTransaction(t *testing.T) {
	l := newLiveScripter(t)
	s, err := Connect(l.ctx, ConnectionOptions{ConnString: *liveConnString, Database: l.db, ApplicationName: "pgmo - live scripter tx"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	sc := NewScripter(s.DatabaseRef(l.db), DefaultScriptOptions())
	other := NewScripter(s.DatabaseRef("postgres"), DefaultScriptOptions())
	err = s.InTransaction(l.ctx, func(ctx context.Context) error {
		for _, stmt := range []string{"SET LOCAL search_path = public", "CREATE SCHEMA tx", "CREATE TABLE tx.t (a int DEFAULT 1)"} {
			if _, err := s.exec(ctx, stmt); err != nil {
				return err
			}
		}
		got, err := sc.ScriptTable(ctx, "tx", "t")
		if err != nil {
			return err
		}
		if !strings.Contains(got, "CREATE TABLE tx.t") {
			t.Errorf("script = %s", got)
		}
		var path string
		if err := s.on(ctx).QueryRow(ctx, "SELECT pg_catalog.current_setting('search_path')").Scan(&path); err != nil {
			return err
		}
		if path != "public" {
			t.Errorf("search_path after the script = %q, want public", path)
		}
		if _, err := other.ScriptSchema(ctx, "public"); !errors.Is(err, ErrUnsupported) {
			t.Errorf("script in another database inside InTransaction: %v, want ErrUnsupported", err)
		}
		return errors.New("roll back")
	})
	if err == nil || err.Error() != "roll back" {
		t.Errorf("InTransaction = %v", err)
	}
}
