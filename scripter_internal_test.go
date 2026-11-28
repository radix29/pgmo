package pgmo

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// The script text: statements terminated once, notes never, a blank line
// between items, empty statements skipped.
func TestScriptTextRenders(t *testing.T) {
	var s scriptText
	s.add("CREATE TABLE a (x int)", "", "SELECT 1;")
	s.notef("a %s\nnote", "multi-line")
	s.add("SELECT 2 -- trailing")
	want := "CREATE TABLE a (x int);\n\nSELECT 1;\n\n-- a multi-line note\n\nSELECT 2 -- trailing\n;\n"
	if got := s.String(); got != want {
		t.Errorf("String =\n%q\nwant\n%q", got, want)
	}
}

// Each verb writes what it says; DROP AND CREATE puts the DROP first and
// discards it when the CREATE half fails; an unknown verb is refused.
func TestScriptOptionsVerbs(t *testing.T) {
	create := func(s *scriptText) error { s.add("CREATE X"); return nil }
	for _, tc := range []struct {
		verb ScriptVerb
		want string
	}{
		{ScriptCreate, "CREATE X;\n"},
		{ScriptAlter, "CREATE X;\n"},
		{ScriptDrop, "DROP X;\n"},
		{ScriptDropAndCreate, "DROP X;\n\nCREATE X;\n"},
	} {
		got, err := ScriptOptions{Verb: tc.verb}.script("op", "DROP X", create)
		if err != nil || got != tc.want {
			t.Errorf("%v: %q, %v; want %q", tc.verb, got, err, tc.want)
		}
	}
	boom := errors.New("boom")
	if got, err := (ScriptOptions{Verb: ScriptDropAndCreate}).script("op", "DROP X", func(*scriptText) error { return boom }); got != "" || !errors.Is(err, boom) {
		t.Errorf("failing create: %q, %v", got, err)
	}
	if _, err := (ScriptOptions{Verb: 9}).script("script x", "DROP X", create); !errors.Is(err, ErrInvalidRequest) || !strings.HasPrefix(err.Error(), "pgmo: script x: ") {
		t.Errorf("unknown verb: %v", err)
	}
	if got := ScriptVerb(9).String(); got != "ScriptVerb(9)" {
		t.Errorf("String = %q", got)
	}
}

// DROP's modifiers, names by SchemaQualify, OR REPLACE only under ALTER,
// IF NOT EXISTS only when asked, owner and comment only when included.
func TestScriptOptionsClauses(t *testing.T) {
	o := ScriptOptions{IfExists: true, Cascade: true}
	for _, tc := range []struct{ got, want string }{
		{o.drop("TABLE", "a.t"), "DROP TABLE IF EXISTS a.t CASCADE"},
		{o.dropOn("TRIGGER", "tr", "a.t"), "DROP TRIGGER IF EXISTS tr ON a.t CASCADE"},
		{o.dropPlain("ROLE", "r"), "DROP ROLE IF EXISTS r"},
		{ScriptOptions{}.drop("VIEW", "v"), "DROP VIEW v"},
		{ScriptOptions{SchemaQualify: true}.name("S s", "T"), `"S s"."T"`},
		{ScriptOptions{}.name("S s", "T"), `"T"`},
		{ScriptOptions{}.name("", "t"), "t"},
		{ScriptOptions{Verb: ScriptAlter}.orReplace(), "OR REPLACE "},
		{ScriptOptions{Verb: ScriptDropAndCreate}.orReplace(), ""},
		{ScriptOptions{IfNotExists: true}.ifNotExists(), " IF NOT EXISTS"},
		{ScriptOptions{}.ifNotExists(), ""},
		{ScriptOptions{IncludeOwner: true}.owner("TABLE", "t", "B b"), `ALTER TABLE t OWNER TO "B b"`},
		{ScriptOptions{IncludeOwner: true}.owner("TABLE", "t", ""), ""},
		{ScriptOptions{}.owner("TABLE", "t", "o"), ""},
		{ScriptOptions{IncludeComments: true}.comment("TABLE t", "it's"), "COMMENT ON TABLE t IS 'it''s'"},
		{ScriptOptions{IncludeComments: true}.comment("TABLE t", ""), ""},
		{ScriptOptions{}.comment("TABLE t", "x"), ""},
		{ScriptOptions{SchemaQualify: true}.renderPath("S s"), "pg_catalog, pg_temp"},
		{ScriptOptions{}.renderPath("S s"), `"S s", pg_catalog, pg_temp`},
		{ScriptOptions{}.renderPath(""), "pg_catalog, pg_temp"},
		{ScriptOptions{SchemaQualify: true}.operatorName("S s", "==="), `"S s".===`},
		{ScriptOptions{}.operatorName("S s", "==="), "==="},
	} {
		if tc.got != tc.want {
			t.Errorf("got %q, want %q", tc.got, tc.want)
		}
	}
	if s, ok := (ScriptOptions{}).member("table t", Origin{}); ok || s != "" {
		t.Errorf("member of none = %q, %v", s, ok)
	}
	if s, ok := (ScriptOptions{}).member("table t", Origin{Extension: "e\nx"}); !ok ||
		s != "-- table t is a member of extension \"e x\": CREATE EXTENSION creates it and DROP EXTENSION drops it\n" {
		t.Errorf("member = %q, %v", s, ok)
	}
}

func grant(grantee string, p Privilege, grantable bool) PrivilegeGrant {
	return PrivilegeGrant{Grantee: grantee, Grantor: "own", Privilege: p, Grantable: grantable}
}

// An ACL is restated from the kind's default: nothing for one never set;
// REVOKE ALL from a default grantee missing one of its privileges, then a
// GRANT of what it keeps; GRANT of the rest, WITH GRANT OPTION apart.
func TestACLStatements(t *testing.T) {
	v17, v16 := ServerVersion(170000), ServerVersion(160000)
	all17 := []PrivilegeGrant{grant("own", PrivilegeInsert, false), grant("own", PrivilegeSelect, false),
		grant("own", PrivilegeUpdate, false), grant("own", PrivilegeDelete, false), grant("own", PrivilegeTruncate, false),
		grant("own", PrivilegeReferences, false), grant("own", PrivilegeTrigger, false)}
	for _, tc := range []struct {
		name      string
		kind      SecurableKind
		grants    []PrivilegeGrant
		isDefault bool
		v         ServerVersion
		want      []string
	}{
		{"never set", SecurableTable, nil, true, v17, nil},
		{"owner's all on 16", SecurableTable, all17, false, v16, nil},
		{"owner lacks MAINTAIN on 17", SecurableTable, all17, false, v17, []string{
			"REVOKE ALL ON TABLE t FROM own",
			"GRANT INSERT, SELECT, UPDATE, DELETE, TRUNCATE, REFERENCES, TRIGGER ON TABLE t TO own"}},
		{"grants beyond", SecurableTable, append(slices.Clone(all17), grant("B b", PrivilegeSelect, false),
			grant("B b", PrivilegeUpdate, true), grant("", PrivilegeSelect, false)), false, v16, []string{
			`GRANT SELECT ON TABLE t TO "B b"`, `GRANT UPDATE ON TABLE t TO "B b" WITH GRANT OPTION`,
			"GRANT SELECT ON TABLE t TO PUBLIC"}},
		{"function revoked from PUBLIC", SecurableFunction, []PrivilegeGrant{grant("own", PrivilegeExecute, false),
			grant("x", PrivilegeExecute, true)}, false, v16, []string{
			"REVOKE ALL ON TABLE t FROM PUBLIC", "GRANT EXECUTE ON TABLE t TO x WITH GRANT OPTION"}},
		{"owner's held with grant option", SecurableSchema, []PrivilegeGrant{grant("own", PrivilegeUsage, true),
			grant("own", PrivilegeCreate, false)}, false, v16, []string{
			"GRANT USAGE ON TABLE t TO own WITH GRANT OPTION"}},
		{"everything revoked", SecurableDatabase, nil, false, v16, []string{
			"REVOKE ALL ON TABLE t FROM PUBLIC", "REVOKE ALL ON TABLE t FROM own"}},
		{"granted twice by two grantors", SecurableType, []PrivilegeGrant{grant("own", PrivilegeUsage, false),
			grant("", PrivilegeUsage, false), {Grantee: "x", Grantor: "y", Privilege: PrivilegeUsage, Grantable: true},
			grant("x", PrivilegeUsage, false)}, false, v16, []string{
			"GRANT USAGE ON TABLE t TO x WITH GRANT OPTION"}},
	} {
		got := aclStatements("TABLE t", tc.kind, "own", tc.grants, tc.isDefault, tc.v)
		if !slices.Equal(got, tc.want) {
			t.Errorf("%s:\n%q\nwant\n%q", tc.name, got, tc.want)
		}
	}
	// An unknown owner has no default to restate: PUBLIC's alone.
	if got := aclStatements("TYPE t", SecurableType, "", nil, false, v16); !slices.Equal(got, []string{"REVOKE ALL ON TYPE t FROM PUBLIC"}) {
		t.Errorf("unknown owner: %q", got)
	}
	if got := (ScriptOptions{}).privileges("TABLE t", SecurableTable, "own", nil, false, v16); got != nil {
		t.Errorf("privileges without IncludePrivileges = %q", got)
	}
}

func TestColumnACLStatements(t *testing.T) {
	cols := []*Column{{Name: "a"}, {Name: "B b", Privileges: []PrivilegeGrant{grant("x", PrivilegeSelect, false),
		grant("x", PrivilegeUpdate, false), grant("x", PrivilegeInsert, true)}}}
	want := []string{`GRANT SELECT ("B b"), UPDATE ("B b") ON TABLE t TO x`, `GRANT INSERT ("B b") ON TABLE t TO x WITH GRANT OPTION`}
	if got := columnACLStatements("t", cols); !slices.Equal(got, want) {
		t.Errorf("got %q\nwant %q", got, want)
	}
}

func TestDefaultPrivilegeStatements(t *testing.T) {
	got, err := defaultPrivilegeStatements([]DefaultPrivileges{
		{Role: "own", Schema: "S s", ObjectType: DefaultPrivilegeTables, Grants: []PrivilegeGrant{
			grant("x", PrivilegeSelect, false), grant("x", PrivilegeInsert, true), grant("", PrivilegeSelect, false)}},
		{Role: "own", ObjectType: DefaultPrivilegeFunctions, Grants: []PrivilegeGrant{grant("x", PrivilegeExecute, false)}},
	}, 160000)
	want := []string{
		`ALTER DEFAULT PRIVILEGES FOR ROLE own IN SCHEMA "S s" GRANT SELECT ON TABLES TO x`,
		`ALTER DEFAULT PRIVILEGES FOR ROLE own IN SCHEMA "S s" GRANT INSERT ON TABLES TO x WITH GRANT OPTION`,
		`ALTER DEFAULT PRIVILEGES FOR ROLE own IN SCHEMA "S s" GRANT SELECT ON TABLES TO PUBLIC`,
		`ALTER DEFAULT PRIVILEGES FOR ROLE own GRANT EXECUTE ON FUNCTIONS TO x`,
	}
	if err != nil || !slices.Equal(got, want) {
		t.Errorf("got %q, %v\nwant %q", got, err, want)
	}
	if _, err := defaultPrivilegeStatements([]DefaultPrivileges{{Role: "own", ObjectType: "?", Grants: []PrivilegeGrant{grant("x", PrivilegeSelect, false)}}}, 160000); !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("unknown object type: %v", err)
	}
}

// topLevelKeyword finds a keyword in code at depth zero only: never inside
// parentheses, a literal, a quoted name or a comment, never inside a word.
func TestTopLevelKeyword(t *testing.T) {
	for _, tc := range []struct {
		sql, kw string
		want    int
	}{
		{"CREATE INDEX i ON t USING btree (a) WHERE (a > 0)", "WHERE", 36},
		{"CREATE INDEX i ON t USING btree ((a WHERE)) ", "WHERE", -1},
		{`CREATE INDEX "WHERE" ON t (a)`, "WHERE", -1},
		{"CREATE INDEX i ON t (a) /* WHERE */", "WHERE", -1},
		{"CREATE INDEX i ON t (a) -- WHERE", "WHERE", -1},
		{"CREATE INDEX i ON nowhere (a)", "WHERE", -1},
		{"CREATE INDEX i ON t (a) where_x", "WHERE", -1},
		{"CREATE INDEX i ON t (a) where (b)", "WHERE", 24},
		{"CREATE INDEX i ON ONLY s.t USING btree (a)", "ON ONLY", 15},
		{"CREATE INDEX i ON s.t USING btree (a)", "ON ONLY", -1},
		{"SELECT 'WHERE' WHERE", "WHERE", 15},
	} {
		if got := topLevelKeyword(tc.sql, tc.kw); got != tc.want {
			t.Errorf("topLevelKeyword(%q, %q) = %d, want %d", tc.sql, tc.kw, got, tc.want)
		}
	}
}

// An index: IF NOT EXISTS after INDEX, a tablespace before WHERE, ON ONLY
// dropped for a partitioned table's; the rest as the server wrote it.
func TestIndexCreate(t *testing.T) {
	sc := &Scripter{}
	for _, tc := range []struct {
		opts        ScriptOptions
		index       Index
		partitioned bool
		want        string
	}{
		{ScriptOptions{}, Index{Definition: "CREATE INDEX i ON s.t USING btree (a)"}, false,
			"CREATE INDEX i ON s.t USING btree (a)"},
		{ScriptOptions{IfNotExists: true}, Index{Definition: "CREATE UNIQUE INDEX i ON s.t USING btree (a)"}, false,
			"CREATE UNIQUE INDEX IF NOT EXISTS i ON s.t USING btree (a)"},
		{ScriptOptions{}, Index{Definition: "CREATE INDEX i ON s.t USING btree (a) WHERE (a > 0)", Tablespace: "f s"}, false,
			`CREATE INDEX i ON s.t USING btree (a) TABLESPACE "f s" WHERE (a > 0)`},
		{ScriptOptions{}, Index{Definition: "CREATE INDEX i ON s.t USING btree (a) WITH (fillfactor='50')", Tablespace: "f"}, false,
			"CREATE INDEX i ON s.t USING btree (a) WITH (fillfactor='50') TABLESPACE f"},
		{ScriptOptions{}, Index{Definition: "CREATE INDEX i ON ONLY s.p USING btree (a)"}, true,
			"CREATE INDEX i ON s.p USING btree (a)"},
		{ScriptOptions{}, Index{Definition: "CREATE INDEX i ON ONLY s.p USING btree (a)"}, false,
			"CREATE INDEX i ON ONLY s.p USING btree (a)"},
	} {
		sc.opts = tc.opts
		if got := sc.indexCreate(&tc.index, tc.partitioned); got != tc.want {
			t.Errorf("got  %q\nwant %q", got, tc.want)
		}
	}
	// A constraint's index and a partition's attached one are not written,
	// and CLUSTER ON follows the clustered one.
	sc.opts = ScriptOptions{}
	got := sc.indexStatements([]*Index{{Name: "pk", Definition: "CREATE UNIQUE INDEX pk ON t (a)", Constraint: "pk"},
		{Name: "att", Definition: "CREATE INDEX att ON t (a)", Parent: ObjectName{"s", "p_a"}},
		{Name: "C c", Definition: "CREATE INDEX \"C c\" ON t (b)", Clustered: true}}, "t", false)
	if want := []string{`CREATE INDEX "C c" ON t (b)`, `ALTER TABLE t CLUSTER ON "C c"`}; !slices.Equal(got, want) {
		t.Errorf("indexStatements = %q, want %q", got, want)
	}
}

func TestTypeCreate(t *testing.T) {
	for _, tc := range []struct {
		typ  Type
		want string
	}{
		{Type{Shell: true, Kind: TypePseudo}, "CREATE TYPE x"},
		{Type{Kind: TypeEnum, Labels: []string{"a", "o'k"}}, "CREATE TYPE x AS ENUM (\n    'a',\n    'o''k'\n)"},
		{Type{Kind: TypeEnum}, "CREATE TYPE x AS ENUM ()"},
		{Type{Kind: TypeComposite, Attributes: []TypeAttribute{{Name: "a", Type: "integer"},
			{Name: "B", Type: "text", Collation: ObjectName{"pg_catalog", "C"}}}},
			"CREATE TYPE x AS (\n    a integer,\n    \"B\" text COLLATE pg_catalog.\"C\"\n)"},
		{Type{Kind: TypeComposite}, "CREATE TYPE x AS ()"},
		{Type{Kind: TypeRange, Subtype: "double precision", SubtypeOperatorClass: ObjectName{"pg_catalog", "float8_ops"},
			Collation: ObjectName{"pg_catalog", "C"}, Canonical: ObjectName{"s", "canon"}, SubtypeDiff: ObjectName{"pg_catalog", "float8mi"},
			Multirange: ObjectName{"s", "x_multirange"}},
			"CREATE TYPE x AS RANGE (\n    SUBTYPE = double precision,\n    SUBTYPE_OPCLASS = pg_catalog.float8_ops,\n" +
				"    COLLATION = pg_catalog.\"C\",\n    CANONICAL = s.canon,\n    SUBTYPE_DIFF = pg_catalog.float8mi,\n" +
				"    MULTIRANGE_TYPE_NAME = s.x_multirange\n)"},
		{Type{Kind: TypeBase, Input: ObjectName{"s", "in"}, Output: ObjectName{"s", "out"}, Receive: ObjectName{"s", "recv"},
			Subscript: ObjectName{"pg_catalog", "array_subscript_handler"}, InternalLength: -1, PassedByValue: false,
			Alignment: AlignInt4, Storage: StorageExtended, Category: TypeCategoryUser, Preferred: true,
			HasDefault: true, Default: "", Element: "integer", Delimiter: ";", Collatable: true},
			"CREATE TYPE x (\n    INPUT = s.\"in\",\n    OUTPUT = s.\"out\",\n    RECEIVE = s.recv,\n" +
				"    SUBSCRIPT = pg_catalog.array_subscript_handler,\n    INTERNALLENGTH = VARIABLE,\n    ALIGNMENT = int4,\n" +
				"    STORAGE = EXTENDED,\n    CATEGORY = 'U',\n    PREFERRED = true,\n    DEFAULT = '',\n    ELEMENT = integer,\n" +
				"    DELIMITER = ';',\n    COLLATABLE = true\n)"},
		{Type{Kind: TypeBase, Input: ObjectName{"s", "i"}, Output: ObjectName{"s", "o"}, InternalLength: 8, PassedByValue: true, Delimiter: ","},
			"CREATE TYPE x (\n    INPUT = s.i,\n    OUTPUT = s.o,\n    INTERNALLENGTH = 8,\n    PASSEDBYVALUE\n)"},
	} {
		got, err := typeCreate(&tc.typ, "x")
		if err != nil || got != tc.want {
			t.Errorf("got  %q, %v\nwant %q", got, err, tc.want)
		}
	}
	if _, err := typeCreate(&Type{Kind: "?"}, "x"); !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("unknown kind: %v", err)
	}
}

func TestAggregateCreate(t *testing.T) {
	o := ScriptOptions{SchemaQualify: true}
	id := func(args ...string) RoutineIdentity {
		return RoutineIdentity{Schema: "S s", Name: "ag", Kind: RoutineAggregate, ArgumentTypeNames: args}
	}
	for _, tc := range []struct {
		opts ScriptOptions
		agg  Aggregate
		want string
	}{
		{o, Aggregate{RoutineIdentity: id(), AggregateKind: AggregateNormal, TransitionFunction: ObjectName{"pg_catalog", "int8inc"},
			StateType: "bigint", FinalModify: FinalReadOnly, HasInitialCondition: true, InitialCondition: "0", Parallel: ParallelUnsafe},
			"CREATE AGGREGATE \"S s\".ag (*) (\n    SFUNC = pg_catalog.int8inc,\n    STYPE = bigint,\n    INITCOND = '0'\n)"},
		{ScriptOptions{Verb: ScriptAlter}, Aggregate{RoutineIdentity: id("integer"), AggregateKind: AggregateNormal,
			Arguments:          []RoutineArgument{{Mode: ArgumentIn, Name: "v", Type: "integer"}},
			TransitionFunction: ObjectName{"pg_catalog", "int4pl"}, StateType: "integer", StateSpace: 4,
			FinalFunction: ObjectName{"s", "fin"}, FinalExtra: true, FinalModify: FinalShareable,
			CombineFunction: ObjectName{"s", "comb"}, SerialFunction: ObjectName{"s", "ser"}, DeserialFunction: ObjectName{"s", "deser"},
			MovingTransitionFunction: ObjectName{"s", "mf"}, MovingInverseFunction: ObjectName{"s", "mi"}, MovingStateType: "integer",
			MovingStateSpace: 8, MovingFinalFunction: ObjectName{"s", "mfin"}, MovingFinalExtra: true, MovingFinalModify: FinalReadOnly,
			HasMovingInitialCondition: true, MovingInitialCondition: "", SortOperator: "OPERATOR(pg_catalog.<)", Parallel: ParallelSafe},
			"CREATE OR REPLACE AGGREGATE ag (v integer) (\n    SFUNC = pg_catalog.int4pl,\n    STYPE = integer,\n    SSPACE = 4,\n" +
				"    FINALFUNC = s.fin,\n    FINALFUNC_EXTRA,\n    FINALFUNC_MODIFY = SHAREABLE,\n    COMBINEFUNC = s.comb,\n" +
				"    SERIALFUNC = s.ser,\n    DESERIALFUNC = s.deser,\n    MSFUNC = s.mf,\n    MINVFUNC = s.mi,\n    MSTYPE = integer,\n" +
				"    MSSPACE = 8,\n    MFINALFUNC = s.mfin,\n    MFINALFUNC_EXTRA,\n    MINITCOND = '',\n" +
				"    SORTOP = OPERATOR(pg_catalog.<),\n    PARALLEL = SAFE\n)"},
		{o, Aggregate{RoutineIdentity: id("double precision", "double precision"), AggregateKind: AggregateOrderedSet, DirectArguments: 1,
			Arguments:          []RoutineArgument{{Mode: ArgumentIn, Type: "double precision"}, {Mode: ArgumentIn, Type: "double precision"}},
			TransitionFunction: ObjectName{"pg_catalog", "ordered_set_transition"}, StateType: "internal",
			FinalFunction: ObjectName{"pg_catalog", "percentile_cont_float8_final"}, FinalModify: FinalReadWrite, Parallel: ParallelRestricted},
			"CREATE AGGREGATE \"S s\".ag (double precision ORDER BY double precision) (\n    SFUNC = pg_catalog.ordered_set_transition,\n" +
				"    STYPE = internal,\n    FINALFUNC = pg_catalog.percentile_cont_float8_final,\n    PARALLEL = RESTRICTED\n)"},
		{o, Aggregate{RoutineIdentity: id(`"any"`), AggregateKind: AggregateHypothetical, DirectArguments: 1,
			Arguments:          []RoutineArgument{{Mode: ArgumentVariadic, Type: `"any"`}, {Mode: ArgumentVariadic, Type: `"any"`}},
			TransitionFunction: ObjectName{"pg_catalog", "ordered_set_transition_multi"}, StateType: "internal", FinalModify: FinalReadOnly},
			"CREATE AGGREGATE \"S s\".ag (VARIADIC \"any\" ORDER BY VARIADIC \"any\") (\n    SFUNC = pg_catalog.ordered_set_transition_multi,\n" +
				"    STYPE = internal,\n    FINALFUNC_MODIFY = READ_ONLY,\n    HYPOTHETICAL\n)"},
		{o, Aggregate{RoutineIdentity: id(`"any"`), AggregateKind: AggregateOrderedSet, DirectArguments: 0,
			Arguments: []RoutineArgument{{Mode: ArgumentIn, Type: "anyelement"}}, TransitionFunction: ObjectName{"pg_catalog", "t"},
			StateType: "internal", FinalModify: FinalReadWrite},
			"CREATE AGGREGATE \"S s\".ag (ORDER BY anyelement) (\n    SFUNC = pg_catalog.t,\n    STYPE = internal\n)"},
	} {
		if got := tc.opts.aggregateCreate(&tc.agg); got != tc.want {
			t.Errorf("got\n%s\nwant\n%s", got, tc.want)
		}
	}
	r := id()
	if got := o.aggregateSignature(&r); got != `"S s".ag(*)` {
		t.Errorf("aggregateSignature = %q", got)
	}
	r = id("integer", "text")
	if got := (ScriptOptions{}).aggregateSignature(&r); got != "ag(integer, text)" {
		t.Errorf("aggregateSignature = %q", got)
	}
}

// pg_get_functiondef's header: OR REPLACE kept only under ALTER, the name
// bare without SchemaQualify; an unexpected header is left as it is.
func TestRoutineCreate(t *testing.T) {
	r := &RoutineIdentity{Schema: "S s", Name: "f", ArgumentTypeNames: []string{"integer"}}
	def := "CREATE OR REPLACE FUNCTION \"S s\".f(i integer)\n RETURNS integer\n LANGUAGE sql\nAS $function$SELECT i$function$\n"
	for _, tc := range []struct {
		opts ScriptOptions
		def  string
		want string
	}{
		{ScriptOptions{SchemaQualify: true}, def, "CREATE FUNCTION \"S s\".f(i integer)\n RETURNS integer\n LANGUAGE sql\nAS $function$SELECT i$function$"},
		{ScriptOptions{Verb: ScriptAlter, SchemaQualify: true}, def, strings.TrimSuffix(def, "\n")},
		{ScriptOptions{}, def, "CREATE FUNCTION f(i integer)\n RETURNS integer\n LANGUAGE sql\nAS $function$SELECT i$function$"},
		{ScriptOptions{}, "CREATE OR REPLACE FUNCTION other.f()\n", "CREATE OR REPLACE FUNCTION other.f()"},
	} {
		if got := tc.opts.routineCreate(tc.def, "FUNCTION", r); got != tc.want {
			t.Errorf("got  %q\nwant %q", got, tc.want)
		}
	}
	if got := (ScriptOptions{}).signature(r); got != "f(integer)" {
		t.Errorf("signature = %q", got)
	}
}

func TestPolicyCreate(t *testing.T) {
	for _, tc := range []struct {
		p    Policy
		want string
	}{
		{Policy{Name: "p", Command: PolicyAll, Permissive: true, Roles: []string{""}},
			"CREATE POLICY p ON t"},
		{Policy{Name: "P p", Command: PolicySelect, Permissive: false, Roles: []string{"a", "B b"}, Using: "(x > 0)"},
			`CREATE POLICY "P p" ON t AS RESTRICTIVE FOR SELECT TO a, "B b" USING ((x > 0))`},
		{Policy{Name: "p", Command: PolicyUpdate, Permissive: true, Roles: []string{"", "a"}, Using: "true", WithCheck: "(y)"},
			"CREATE POLICY p ON t FOR UPDATE TO PUBLIC, a USING (true) WITH CHECK ((y))"},
	} {
		if got := policyCreate(&tc.p, "t"); got != tc.want {
			t.Errorf("got  %q\nwant %q", got, tc.want)
		}
	}
}

func TestTriggerStatements(t *testing.T) {
	sc := &Scripter{}
	def := "CREATE TRIGGER tr BEFORE INSERT ON s.t FOR EACH ROW EXECUTE FUNCTION s.f()"
	for _, tc := range []struct {
		tr        Trigger
		orReplace bool
		want      []string
	}{
		{Trigger{Name: "tr", Definition: def, Enabled: TriggerEnabledOrigin}, false, []string{def}},
		{Trigger{Name: "tr", Definition: def, Enabled: TriggerDisabled}, true,
			[]string{"CREATE OR REPLACE TRIGGER tr BEFORE INSERT ON s.t FOR EACH ROW EXECUTE FUNCTION s.f()", "ALTER TABLE t DISABLE TRIGGER tr"}},
		{Trigger{Name: "T r", Definition: "CREATE CONSTRAINT TRIGGER x", IsConstraint: true, Enabled: TriggerEnabledReplica}, true,
			[]string{"CREATE CONSTRAINT TRIGGER x", `ALTER TABLE t ENABLE REPLICA TRIGGER "T r"`}},
		{Trigger{Name: "tr", Definition: def, Enabled: TriggerEnabledAlways, TableKind: RelationForeignTable}, false,
			[]string{def, "ALTER FOREIGN TABLE t ENABLE ALWAYS TRIGGER tr"}},
	} {
		if got := sc.triggerStatements(&tc.tr, "t", tc.orReplace); !slices.Equal(got, tc.want) {
			t.Errorf("got  %q\nwant %q", got, tc.want)
		}
	}
	for e, want := range map[TriggerEnabled]string{TriggerEnabledOrigin: "", TriggerDisabled: "A DISABLE",
		TriggerEnabledReplica: "A ENABLE REPLICA", TriggerEnabledAlways: "A ENABLE ALWAYS"} {
		if got := enableStatement("A", e); got != want {
			t.Errorf("enableStatement(%q) = %q, want %q", e, got, want)
		}
	}
}

// A table's own constraints are written inside CREATE TABLE but foreign
// keys, NOT VALID ones and 18's NOT NULL rows; an inherited one never.
func TestInlineConstraint(t *testing.T) {
	for _, tc := range []struct {
		c    Constraint
		want bool
	}{
		{Constraint{Kind: ConstraintCheck, IsLocal: true, Validated: true}, true},
		{Constraint{Kind: ConstraintPrimaryKey, IsLocal: true, Validated: true}, true},
		{Constraint{Kind: ConstraintForeignKey, IsLocal: true, Validated: true}, false},
		{Constraint{Kind: ConstraintCheck, IsLocal: true}, false},
		{Constraint{Kind: ConstraintCheck, Validated: true}, false},
		{Constraint{Kind: ConstraintNotNull, IsLocal: true, Validated: true}, false},
	} {
		if got := inlineConstraint(&tc.c); got != tc.want {
			t.Errorf("inlineConstraint(%+v) = %v", tc.c, got)
		}
	}
	if got := alterTable(&Table{Kind: RelationTable}, "t"); got != "ALTER TABLE ONLY t" {
		t.Errorf("alterTable(table) = %q", got)
	}
	if got := alterTable(&Table{Kind: RelationPartitionedTable}, "t"); got != "ALTER TABLE t" {
		t.Errorf("alterTable(partitioned) = %q", got)
	}
}

func TestColumnDefinitionWithFDWOptions(t *testing.T) {
	got, err := columnDefinition(&Column{Name: "a", Type: "integer", NotNull: true,
		FDWOptions: []ParameterSetting{{Name: "column_name", Value: "it's"}}}, 160000)
	if want := "a integer OPTIONS (column_name 'it''s') NOT NULL"; err != nil || got != want {
		t.Errorf("got %q, %v; want %q", got, err, want)
	}
	if _, err := columnDefinition(&Column{Name: "a", Type: "integer", Generated: GeneratedVirtual, GenerationExpression: "1"}, 170000); !errors.Is(err, ErrUnsupportedVersion) {
		t.Errorf("virtual column on 17: %v", err)
	}
}

// A user mapping's password never reaches the script: the placeholder,
// unquoted, stands in for it.
func TestUserMappingOptions(t *testing.T) {
	got := userMappingOptions([]ParameterSetting{{Name: "user", Value: "u"}, {Name: "password", Value: "s3cret"}})
	if want := "(\"user\" 'u', password " + PasswordPlaceholder + ")"; got != want || strings.Contains(got, "s3cret") {
		t.Errorf("got %q, want %q", got, want)
	}
	if got := genericOptions([]ParameterSetting{{Name: "Host", Value: "h'"}}); got != `("Host" 'h''')` {
		t.Errorf("genericOptions = %q", got)
	}
}

func TestCollationOptions(t *testing.T) {
	for _, tc := range []struct {
		c    Collation
		want []string
	}{
		{Collation{Provider: LocaleProviderLibc, Collate: "C", CType: "C", Deterministic: true}, []string{"provider = libc", "locale = 'C'"}},
		{Collation{Provider: LocaleProviderLibc, Collate: "C", CType: "POSIX", Deterministic: true},
			[]string{"provider = libc", "lc_collate = 'C'", "lc_ctype = 'POSIX'"}},
		{Collation{Provider: LocaleProviderICU, Locale: "und-u-ks-level2", ICURules: "&a < b"},
			[]string{"provider = icu", "locale = 'und-u-ks-level2'", "deterministic = false", "rules = '&a < b'"}},
		{Collation{Provider: LocaleProviderBuiltin, Locale: "C.UTF-8", Deterministic: true}, []string{"provider = builtin", "locale = 'C.UTF-8'"}},
	} {
		got, err := collationOptions(&tc.c)
		if err != nil || !slices.Equal(got, tc.want) {
			t.Errorf("got %q, %v; want %q", got, err, tc.want)
		}
	}
	if _, err := collationOptions(&Collation{Provider: LocaleProviderDefault}); !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("default provider: %v", err)
	}
}

func TestCastClause(t *testing.T) {
	for _, tc := range []struct {
		c    Cast
		want string
	}{
		{Cast{Method: CastByFunction, Context: CastExplicit, Function: ObjectName{"S s", "f"}, FunctionArguments: []string{"integer", "boolean"}},
			`WITH FUNCTION "S s".f(integer, boolean)`},
		{Cast{Method: CastByInOut, Context: CastAssignment}, "WITH INOUT AS ASSIGNMENT"},
		{Cast{Method: CastBinary, Context: CastImplicit}, "WITHOUT FUNCTION AS IMPLICIT"},
	} {
		got, err := castClause(&tc.c)
		if err != nil || got != tc.want {
			t.Errorf("got %q, %v; want %q", got, err, tc.want)
		}
	}
	if _, err := castClause(&Cast{Method: "?"}); !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("unknown method: %v", err)
	}
	if got := (&Cast{}).FunctionSignature(); got != "" {
		t.Errorf("FunctionSignature without a function = %q", got)
	}
}

func TestMembershipOptions(t *testing.T) {
	m := RoleMembership{Role: "r", Member: "m", Admin: true, Inherit: false, Set: true}
	got15, err := grantMembershipStatement(150000, m.Role, m.Member, membershipOptions(m, 150000))
	if err != nil || got15 != "GRANT r TO m WITH ADMIN OPTION" {
		t.Errorf("15: %q, %v", got15, err)
	}
	m.Admin = false
	if got, err := grantMembershipStatement(150000, m.Role, m.Member, membershipOptions(m, 150000)); err != nil || got != "GRANT r TO m" {
		t.Errorf("15 without admin: %q, %v", got, err)
	}
	if got, err := grantMembershipStatement(160000, m.Role, m.Member, membershipOptions(m, 160000)); err != nil ||
		got != "GRANT r TO m WITH ADMIN FALSE, INHERIT FALSE, SET TRUE" {
		t.Errorf("16: %q, %v", got, err)
	}
}

// Inside a render transaction on another database, a statement on the
// Server's own database is not the transaction's: on gives the Server's
// own Querier; onDatabase gives the transaction for its database only.
func TestRenderTransactionRouting(t *testing.T) {
	own, tx := &nopQuerier{}, &nopTx{}
	s := &Server{q: own, database: "postgres"}
	ctx := context.WithValue(t.Context(), txCtxKey{}, &serverTx{server: s, tx: tx, database: "appdb"})
	if q := s.on(ctx); q != Querier(own) {
		t.Errorf("on = %T, want the Server's own Querier", q)
	}
	if q, err := s.onDatabase(ctx, "appdb"); err != nil || q != Querier(tx) {
		t.Errorf("onDatabase(appdb) = %T, %v; want the transaction", q, err)
	}
	if _, err := s.onDatabase(ctx, "third"); !errors.Is(err, ErrUnsupported) {
		t.Errorf("onDatabase(third): %v", err)
	}
	ctx = context.WithValue(t.Context(), txCtxKey{}, &serverTx{server: s, tx: tx, database: "postgres"})
	if q := s.on(ctx); q != Querier(tx) {
		t.Errorf("on in a transaction on its own database = %T", q)
	}
}

type nopQuerier struct{ Querier }

type nopTx struct{ pgx.Tx }

func TestIsWindowsAbsolute(t *testing.T) {
	for p, want := range map[string]bool{`C:\ts`: true, "d:/ts": true, "pg_tblspc/1": false, "/ts": false, "C:": false, "1:/x": false} {
		if got := isWindowsAbsolute(p); got != want {
			t.Errorf("isWindowsAbsolute(%q) = %v", p, got)
		}
	}
}
