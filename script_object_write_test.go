package pgmo_test

import (
	"context"
	"testing"

	"github.com/radix29/pgmo"
	"github.com/radix29/pgmo/pgmotest"
)

// The statements of P4.13.3's other families — schemas, views and
// materialized views, functions and procedures, sequences, types and
// domains, triggers, extensions — whole, as TestScriptTableWrites pins the
// tables'. Every one is captured as run in the object's own database.
func TestScriptObjectWrites(t *testing.T) {
	f := pgmotest.New(t)
	d := f.Server().DatabaseRef(hostileDB)
	sch := d.SchemaRef(`My "S"`)
	v := d.ViewRef(`My "S"`, "user")
	mv := d.MaterializedViewRef(`My "S"`, "user")
	fn := d.FunctionRef(`My "S"`, "user", []string{"integer", `"My S".mood[]`})
	pr := d.ProcedureRef("app", `P"r`, nil)
	seq := d.SequenceRef(`My "S"`, "user")
	ty := d.TypeRef(`My "S"`, "user")
	dom := d.DomainRef(`My "S"`, "user")
	tr := d.TriggerRef(`My "S"`, "user", `T"r`)
	ext := d.ExtensionRef(`E"x`)

	runScriptCasesIn(t, f, hostileDB, []scriptCase{
		// Schemas
		{"schema create", func(ctx context.Context) error {
			_, err := d.CreateSchema(ctx, pgmo.CreateSchemaRequest{Name: `My "S"`})
			return err
		}, `CREATE SCHEMA "My ""S"""`},
		{"schema create, owner", func(ctx context.Context) error {
			_, err := d.CreateSchema(ctx, pgmo.CreateSchemaRequest{Name: "user", Owner: "o'brien"})
			return err
		}, `CREATE SCHEMA "user" AUTHORIZATION "o'brien"`},
		{"schema drop", func(ctx context.Context) error { return sch.Drop(ctx, pgmo.DropOptions{}) }, `DROP SCHEMA "My ""S"""`},
		{"schema drop cascade", func(ctx context.Context) error { return sch.Drop(ctx, pgmo.DropOptions{Cascade: true}) }, `DROP SCHEMA "My ""S""" CASCADE`},
		{"schema rename", func(ctx context.Context) error { return d.SchemaRef(`My "S"`).Rename(ctx, "user") }, `ALTER SCHEMA "My ""S""" RENAME TO "user"`},
		{"schema owner", func(ctx context.Context) error { return sch.SetOwner(ctx, "user") }, `ALTER SCHEMA "My ""S""" OWNER TO "user"`},
		{"schema comment", func(ctx context.Context) error { return sch.SetComment(ctx, "o'brien") }, `COMMENT ON SCHEMA "My ""S""" IS 'o''brien'`},

		// Views
		{"view create", func(ctx context.Context) error {
			_, err := d.CreateView(ctx, pgmo.CreateViewRequest{Schema: `My "S"`, Name: "user", Query: "SELECT 'o''brien' AS x"})
			return err
		}, `CREATE VIEW "My ""S"""."user" AS SELECT 'o''brien' AS x`},
		{"view create, every option", func(ctx context.Context) error {
			_, err := d.CreateView(ctx, pgmo.CreateViewRequest{Schema: "app", Name: "v", Query: "SELECT a, b FROM app.t WHERE a > 0",
				Columns: []string{"user", `B"b`}, OrReplace: true, CheckOption: pgmo.CheckOptionCascaded,
				SecurityBarrier: new(true), SecurityInvoker: new(false)})
			return err
		}, `CREATE OR REPLACE VIEW app.v ("user", "B""b") WITH (security_barrier = true, security_invoker = false)` +
			` AS SELECT a, b FROM app.t WHERE a > 0 WITH CASCADED CHECK OPTION`},
		{"view create, local check", func(ctx context.Context) error {
			_, err := d.CreateView(ctx, pgmo.CreateViewRequest{Schema: "app", Name: "v", Query: "SELECT 1", CheckOption: pgmo.CheckOptionLocal})
			return err
		}, `CREATE VIEW app.v AS SELECT 1 WITH LOCAL CHECK OPTION`},
		{"view drop", func(ctx context.Context) error { return v.Drop(ctx, pgmo.DropOptions{Cascade: true}) }, `DROP VIEW "My ""S"""."user" CASCADE`},
		{"view rename", func(ctx context.Context) error { return d.ViewRef(`My "S"`, "user").Rename(ctx, "v") }, `ALTER VIEW "My ""S"""."user" RENAME TO v`},
		{"view set schema", func(ctx context.Context) error { return d.ViewRef(`My "S"`, "user").SetSchema(ctx, "app") }, `ALTER VIEW "My ""S"""."user" SET SCHEMA app`},
		{"view owner", func(ctx context.Context) error { return v.SetOwner(ctx, "o'brien") }, `ALTER VIEW "My ""S"""."user" OWNER TO "o'brien"`},
		{"view comment", func(ctx context.Context) error { return v.SetComment(ctx, `a\b`) }, `COMMENT ON VIEW "My ""S"""."user" IS E'a\\b'`},

		// Materialized views
		{"matview create", func(ctx context.Context) error {
			_, err := d.CreateMaterializedView(ctx, pgmo.CreateMaterializedViewRequest{Schema: `My "S"`, Name: "user", Query: "SELECT 1 AS id"})
			return err
		}, `CREATE MATERIALIZED VIEW "My ""S"""."user" AS SELECT 1 AS id`},
		{"matview create, every option", func(ctx context.Context) error {
			_, err := d.CreateMaterializedView(ctx, pgmo.CreateMaterializedViewRequest{Schema: "app", Name: "mv", Query: "SELECT 1",
				Columns: []string{"user"}, NoData: true, Tablespace: `T"s`, Options: []pgmo.ParameterSetting{{Name: "fillfactor", Value: "50"}}})
			return err
		}, `CREATE MATERIALIZED VIEW app.mv ("user") WITH (fillfactor = '50') TABLESPACE "T""s" AS SELECT 1 WITH NO DATA`},
		{"matview refresh", func(ctx context.Context) error { return mv.Refresh(ctx, pgmo.RefreshMaterializedViewOptions{}) }, `REFRESH MATERIALIZED VIEW "My ""S"""."user"`},
		{"matview refresh concurrently", func(ctx context.Context) error {
			return mv.Refresh(ctx, pgmo.RefreshMaterializedViewOptions{Concurrently: true})
		}, `REFRESH MATERIALIZED VIEW CONCURRENTLY "My ""S"""."user"`},
		{"matview refresh no data", func(ctx context.Context) error {
			return mv.Refresh(ctx, pgmo.RefreshMaterializedViewOptions{NoData: true})
		}, `REFRESH MATERIALIZED VIEW "My ""S"""."user" WITH NO DATA`},
		{"matview drop", func(ctx context.Context) error { return mv.Drop(ctx, pgmo.DropOptions{}) }, `DROP MATERIALIZED VIEW "My ""S"""."user"`},
		{"matview rename", func(ctx context.Context) error {
			return d.MaterializedViewRef(`My "S"`, "user").Rename(ctx, "x")
		}, `ALTER MATERIALIZED VIEW "My ""S"""."user" RENAME TO x`},
		{"matview set schema", func(ctx context.Context) error {
			return d.MaterializedViewRef(`My "S"`, "user").SetSchema(ctx, "user")
		}, `ALTER MATERIALIZED VIEW "My ""S"""."user" SET SCHEMA "user"`},
		{"matview owner", func(ctx context.Context) error { return mv.SetOwner(ctx, "user") }, `ALTER MATERIALIZED VIEW "My ""S"""."user" OWNER TO "user"`},
		{"matview comment", func(ctx context.Context) error { return mv.SetComment(ctx, "") }, `COMMENT ON MATERIALIZED VIEW "My ""S"""."user" IS NULL`},

		// Functions and procedures
		{"function create, sql", func(ctx context.Context) error {
			_, err := d.CreateFunction(ctx, pgmo.CreateFunctionRequest{Schema: "app", Name: "f", Returns: "integer", Language: "sql", Body: "SELECT 1"})
			return err
		}, `CREATE FUNCTION app.f() RETURNS integer LANGUAGE sql AS $$SELECT 1$$`},
		{"function create, every option", func(ctx context.Context) error {
			_, err := d.CreateFunction(ctx, pgmo.CreateFunctionRequest{
				Schema: `My "S"`, Name: "user", OrReplace: true,
				Arguments: []pgmo.RoutineArgumentSpec{
					{Name: "a", Type: "integer"},
					{Mode: pgmo.ArgumentIn, Name: `B"b`, Type: "text", Default: "'o''brien'"},
					{Mode: pgmo.ArgumentInOut, Type: "bigint"},
					{Mode: pgmo.ArgumentOut, Name: "user", Type: "date"},
					{Mode: pgmo.ArgumentVariadic, Name: "rest", Type: "text[]"},
				},
				Language: "plpgsql", Body: "BEGIN $1 := 1; END",
				Volatility: pgmo.VolatilityStable, Strict: true, SecurityDefiner: true, Leakproof: true,
				Parallel: pgmo.ParallelRestricted, Cost: new(2.5), Rows: new(1000.0),
				Settings: []pgmo.ParameterSetting{{Name: "search_path", Value: `pg_catalog, "My S"`}, {Name: "work_mem", Value: "64MB"}},
			})
			return err
		}, `CREATE OR REPLACE FUNCTION "My ""S"""."user"(a integer, "B""b" text DEFAULT 'o''brien', INOUT bigint, OUT "user" date, VARIADIC rest text[])` +
			` LANGUAGE plpgsql STABLE STRICT SECURITY DEFINER LEAKPROOF PARALLEL RESTRICTED COST 2.5 ROWS 1000` +
			` SET search_path = 'pg_catalog', 'My S' SET work_mem = '64MB' AS $_$BEGIN $1 := 1; END$_$`},
		{"function create, returns table", func(ctx context.Context) error {
			_, err := d.CreateFunction(ctx, pgmo.CreateFunctionRequest{Schema: "app", Name: "t",
				Arguments: []pgmo.RoutineArgumentSpec{{Name: "n", Type: "integer"}, {Mode: pgmo.ArgumentTable, Name: "user", Type: "text"},
					{Mode: pgmo.ArgumentTable, Name: "i", Type: "integer"}},
				Language: "sql", Body: "SELECT 'x', n"})
			return err
		}, `CREATE FUNCTION app.t(n integer) RETURNS TABLE ("user" text, i integer) LANGUAGE sql AS $$SELECT 'x', n$$`},
		{"function create, setof, immutable, parallel safe", func(ctx context.Context) error {
			_, err := d.CreateFunction(ctx, pgmo.CreateFunctionRequest{Schema: "app", Name: "s", Returns: "app.t", ReturnsSet: true,
				Language: "sql", Body: "SELECT * FROM app.t", Volatility: pgmo.VolatilityImmutable, Parallel: pgmo.ParallelSafe})
			return err
		}, `CREATE FUNCTION app.s() RETURNS SETOF app.t LANGUAGE sql IMMUTABLE PARALLEL SAFE AS $$SELECT * FROM app.t$$`},
		{"function drop", func(ctx context.Context) error { return fn.Drop(ctx, pgmo.DropOptions{Cascade: true}) },
			`DROP FUNCTION "My ""S"""."user"(integer, "My S".mood[]) CASCADE`},
		{"function rename", func(ctx context.Context) error {
			return d.FunctionRef("app", "f", nil).Rename(ctx, "user")
		}, `ALTER FUNCTION app.f() RENAME TO "user"`},
		{"function set schema", func(ctx context.Context) error {
			return d.FunctionRef("app", "f", []string{"text"}).SetSchema(ctx, `My "S"`)
		}, `ALTER FUNCTION app.f(text) SET SCHEMA "My ""S"""`},
		{"function owner", func(ctx context.Context) error { return fn.SetOwner(ctx, "user") },
			`ALTER FUNCTION "My ""S"""."user"(integer, "My S".mood[]) OWNER TO "user"`},
		{"function comment", func(ctx context.Context) error { return fn.SetComment(ctx, "o'brien") },
			`COMMENT ON FUNCTION "My ""S"""."user"(integer, "My S".mood[]) IS 'o''brien'`},
		{"procedure create", func(ctx context.Context) error {
			_, err := d.CreateProcedure(ctx, pgmo.CreateProcedureRequest{Schema: "app", Name: `P"r`,
				Arguments: []pgmo.RoutineArgumentSpec{{Name: "a", Type: "integer"}, {Mode: pgmo.ArgumentInOut, Name: "b", Type: "text"},
					{Mode: pgmo.ArgumentOut, Name: "c", Type: "date"}},
				Language: "plpgsql", Body: "BEGIN COMMIT; END", OrReplace: true, SecurityDefiner: true,
				Settings: []pgmo.ParameterSetting{{Name: "lock_timeout", Value: "1s"}}})
			return err
		}, `CREATE OR REPLACE PROCEDURE app."P""r"(a integer, INOUT b text, OUT c date) LANGUAGE plpgsql SECURITY DEFINER` +
			` SET lock_timeout = '1s' AS $$BEGIN COMMIT; END$$`},
		{"procedure create, plain", func(ctx context.Context) error {
			_, err := d.CreateProcedure(ctx, pgmo.CreateProcedureRequest{Schema: "app", Name: "p", Language: "sql", Body: "SELECT 1"})
			return err
		}, `CREATE PROCEDURE app.p() LANGUAGE sql AS $$SELECT 1$$`},
		{"procedure drop", func(ctx context.Context) error { return pr.Drop(ctx, pgmo.DropOptions{}) }, `DROP PROCEDURE app."P""r"()`},
		{"procedure rename", func(ctx context.Context) error {
			return d.ProcedureRef("app", "p", []string{"integer"}).Rename(ctx, "q")
		},
			`ALTER PROCEDURE app.p(integer) RENAME TO q`},
		{"procedure set schema", func(ctx context.Context) error { return d.ProcedureRef("app", "p", nil).SetSchema(ctx, "user") },
			`ALTER PROCEDURE app.p() SET SCHEMA "user"`},
		{"procedure owner", func(ctx context.Context) error { return pr.SetOwner(ctx, "o'brien") }, `ALTER PROCEDURE app."P""r"() OWNER TO "o'brien"`},
		{"procedure comment", func(ctx context.Context) error { return pr.SetComment(ctx, "x") }, `COMMENT ON PROCEDURE app."P""r"() IS 'x'`},

		// Sequences
		{"sequence create", func(ctx context.Context) error {
			_, err := d.CreateSequence(ctx, pgmo.CreateSequenceRequest{Schema: `My "S"`, Name: "user"})
			return err
		}, `CREATE SEQUENCE "My ""S"""."user"`},
		{"sequence create, every option", func(ctx context.Context) error {
			_, err := d.CreateSequence(ctx, pgmo.CreateSequenceRequest{Schema: "app", Name: "s", Unlogged: true,
				SequenceOptions: pgmo.SequenceOptions{DataType: "integer", Increment: new(int64(-2)), MinValue: new(int64(-100)),
					MaxValue: new(int64(-1)), Start: new(int64(-1)), Cache: new(int64(10)), Cycle: new(true),
					OwnedBy: &pgmo.SequenceOwner{Table: pgmo.ObjectName{Schema: `My "S"`, Name: "user"}, Column: `I"d`}}})
			return err
		}, `CREATE UNLOGGED SEQUENCE app.s AS integer INCREMENT BY -2 MINVALUE -100 MAXVALUE -1 START WITH -1 CACHE 10 CYCLE` +
			` OWNED BY "My ""S"""."user"."I""d"`},
		{"sequence alter", func(ctx context.Context) error {
			return seq.Alter(ctx, pgmo.SequenceChanges{SequenceOptions: pgmo.SequenceOptions{Cycle: new(false), OwnedBy: &pgmo.SequenceOwner{}},
				Restart: new(int64(42))})
		}, `ALTER SEQUENCE "My ""S"""."user" NO CYCLE OWNED BY NONE RESTART WITH 42`},
		{"sequence drop", func(ctx context.Context) error { return seq.Drop(ctx, pgmo.DropOptions{Cascade: true}) }, `DROP SEQUENCE "My ""S"""."user" CASCADE`},
		{"sequence rename", func(ctx context.Context) error { return d.SequenceRef("app", "s").Rename(ctx, "user") }, `ALTER SEQUENCE app.s RENAME TO "user"`},
		{"sequence set schema", func(ctx context.Context) error { return d.SequenceRef("app", "s").SetSchema(ctx, `My "S"`) },
			`ALTER SEQUENCE app.s SET SCHEMA "My ""S"""`},
		{"sequence owner", func(ctx context.Context) error { return seq.SetOwner(ctx, "o'brien") }, `ALTER SEQUENCE "My ""S"""."user" OWNER TO "o'brien"`},
		{"sequence comment", func(ctx context.Context) error { return seq.SetComment(ctx, "o'brien") }, `COMMENT ON SEQUENCE "My ""S"""."user" IS 'o''brien'`},

		// Types
		{"enum create", func(ctx context.Context) error {
			_, err := d.CreateEnumType(ctx, pgmo.CreateEnumTypeRequest{Schema: `My "S"`, Name: "user", Labels: []string{"o'brien", `a\b`, "Z"}})
			return err
		}, `CREATE TYPE "My ""S"""."user" AS ENUM ('o''brien', E'a\\b', 'Z')`},
		{"enum create, empty", func(ctx context.Context) error {
			_, err := d.CreateEnumType(ctx, pgmo.CreateEnumTypeRequest{Schema: "app", Name: "e"})
			return err
		}, `CREATE TYPE app.e AS ENUM ()`},
		{"composite create", func(ctx context.Context) error {
			_, err := d.CreateCompositeType(ctx, pgmo.CreateCompositeTypeRequest{Schema: "app", Name: "c", Attributes: []pgmo.TypeAttributeSpec{
				{Name: "user", Type: "text", Collation: pgmo.ObjectName{Schema: "pg_catalog", Name: "C"}}, {Name: "n", Type: "numeric(5,1)"}}})
			return err
		}, `CREATE TYPE app.c AS ("user" text COLLATE pg_catalog."C", n numeric(5,1))`},
		{"range create", func(ctx context.Context) error {
			_, err := d.CreateRangeType(ctx, pgmo.CreateRangeTypeRequest{Schema: "app", Name: "r", Subtype: "float8"})
			return err
		}, `CREATE TYPE app.r AS RANGE (SUBTYPE = float8)`},
		{"range create, every option", func(ctx context.Context) error {
			_, err := d.CreateRangeType(ctx, pgmo.CreateRangeTypeRequest{Schema: "app", Name: "r", Subtype: "text",
				SubtypeOperatorClass: pgmo.ObjectName{Schema: "pg_catalog", Name: "text_ops"}, Collation: pgmo.ObjectName{Name: "C"},
				Canonical: pgmo.ObjectName{Schema: "app", Name: "canon"}, SubtypeDiff: pgmo.ObjectName{Schema: "app", Name: `D"iff`},
				Multirange: pgmo.ObjectName{Schema: `My "S"`, Name: "user"}})
			return err
		}, `CREATE TYPE app.r AS RANGE (SUBTYPE = text, SUBTYPE_OPCLASS = pg_catalog.text_ops, COLLATION = "C",` +
			` CANONICAL = app.canon, SUBTYPE_DIFF = app."D""iff", MULTIRANGE_TYPE_NAME = "My ""S"""."user")`},
		{"enum add value", func(ctx context.Context) error { return ty.AddEnumValue(ctx, "o'brien", pgmo.EnumValuePosition{}) },
			`ALTER TYPE "My ""S"""."user" ADD VALUE 'o''brien'`},
		{"enum add value before", func(ctx context.Context) error {
			return ty.AddEnumValue(ctx, "a", pgmo.EnumValuePosition{Before: `a\b`})
		}, `ALTER TYPE "My ""S"""."user" ADD VALUE 'a' BEFORE E'a\\b'`},
		{"enum add value after", func(ctx context.Context) error { return ty.AddEnumValue(ctx, "a", pgmo.EnumValuePosition{After: "Z"}) },
			`ALTER TYPE "My ""S"""."user" ADD VALUE 'a' AFTER 'Z'`},
		{"enum rename value", func(ctx context.Context) error { return ty.RenameEnumValue(ctx, "o'brien", "x") },
			`ALTER TYPE "My ""S"""."user" RENAME VALUE 'o''brien' TO 'x'`},
		{"type drop", func(ctx context.Context) error { return ty.Drop(ctx, pgmo.DropOptions{}) }, `DROP TYPE "My ""S"""."user"`},
		{"type rename", func(ctx context.Context) error { return d.TypeRef("app", "e").Rename(ctx, "user") }, `ALTER TYPE app.e RENAME TO "user"`},
		{"type set schema", func(ctx context.Context) error { return d.TypeRef("app", "e").SetSchema(ctx, "x") }, `ALTER TYPE app.e SET SCHEMA x`},
		{"type owner", func(ctx context.Context) error { return ty.SetOwner(ctx, "user") }, `ALTER TYPE "My ""S"""."user" OWNER TO "user"`},
		{"type comment", func(ctx context.Context) error { return ty.SetComment(ctx, "t") }, `COMMENT ON TYPE "My ""S"""."user" IS 't'`},

		// Domains
		{"domain create", func(ctx context.Context) error {
			_, err := d.CreateDomain(ctx, pgmo.CreateDomainRequest{Schema: "app", Name: "d", BaseType: "integer"})
			return err
		}, `CREATE DOMAIN app.d AS integer`},
		{"domain create, every option", func(ctx context.Context) error {
			_, err := d.CreateDomain(ctx, pgmo.CreateDomainRequest{Schema: `My "S"`, Name: "user", BaseType: "text",
				Collation: pgmo.ObjectName{Name: "C"}, Default: "'o''brien'", NotNull: true,
				Constraints: []pgmo.DomainConstraintSpec{{Name: "user", Check: "VALUE <> ''"}, {Check: "length(VALUE) < 10"}}})
			return err
		}, `CREATE DOMAIN "My ""S"""."user" AS text COLLATE "C" DEFAULT 'o''brien' NOT NULL` +
			` CONSTRAINT "user" CHECK (VALUE <> '') CHECK (length(VALUE) < 10)`},
		{"domain set default", func(ctx context.Context) error { return dom.SetDefault(ctx, "'x'") }, `ALTER DOMAIN "My ""S"""."user" SET DEFAULT 'x'`},
		{"domain drop default", func(ctx context.Context) error { return dom.SetDefault(ctx, "") }, `ALTER DOMAIN "My ""S"""."user" DROP DEFAULT`},
		{"domain set not null", func(ctx context.Context) error { return dom.SetNotNull(ctx, true) }, `ALTER DOMAIN "My ""S"""."user" SET NOT NULL`},
		{"domain drop not null", func(ctx context.Context) error { return dom.SetNotNull(ctx, false) }, `ALTER DOMAIN "My ""S"""."user" DROP NOT NULL`},
		{"domain add constraint", func(ctx context.Context) error {
			return dom.AddConstraint(ctx, pgmo.DomainConstraintSpec{Name: `C"k`, Check: "VALUE ~ '^a'", NotValid: true})
		}, `ALTER DOMAIN "My ""S"""."user" ADD CONSTRAINT "C""k" CHECK (VALUE ~ '^a') NOT VALID`},
		{"domain drop constraint", func(ctx context.Context) error {
			return dom.DropConstraint(ctx, `C"k`, pgmo.DropOptions{Cascade: true})
		},
			`ALTER DOMAIN "My ""S"""."user" DROP CONSTRAINT "C""k" CASCADE`},
		{"domain validate constraint", func(ctx context.Context) error { return dom.ValidateConstraint(ctx, "user") },
			`ALTER DOMAIN "My ""S"""."user" VALIDATE CONSTRAINT "user"`},
		{"domain drop", func(ctx context.Context) error { return dom.Drop(ctx, pgmo.DropOptions{Cascade: true}) }, `DROP DOMAIN "My ""S"""."user" CASCADE`},
		{"domain rename", func(ctx context.Context) error { return d.DomainRef("app", "d").Rename(ctx, "user") }, `ALTER DOMAIN app.d RENAME TO "user"`},
		{"domain set schema", func(ctx context.Context) error { return d.DomainRef("app", "d").SetSchema(ctx, "user") }, `ALTER DOMAIN app.d SET SCHEMA "user"`},
		{"domain owner", func(ctx context.Context) error { return dom.SetOwner(ctx, "o'brien") }, `ALTER DOMAIN "My ""S"""."user" OWNER TO "o'brien"`},
		{"domain comment", func(ctx context.Context) error { return dom.SetComment(ctx, "") }, `COMMENT ON DOMAIN "My ""S"""."user" IS NULL`},

		// Triggers
		{"trigger create", func(ctx context.Context) error {
			_, err := d.CreateTrigger(ctx, pgmo.CreateTriggerRequest{Schema: "app", Table: "t", Name: "tr", Timing: pgmo.TriggerBefore,
				Events: []pgmo.TriggerEvent{pgmo.TriggerInsert}, ForEachRow: true, Function: pgmo.ObjectName{Schema: "app", Name: "f"}})
			return err
		}, `CREATE TRIGGER tr BEFORE INSERT ON app.t FOR EACH ROW EXECUTE FUNCTION app.f()`},
		{"trigger create, every option", func(ctx context.Context) error {
			_, err := d.CreateTrigger(ctx, pgmo.CreateTriggerRequest{Schema: `My "S"`, Table: "user", Name: `T"r`, OrReplace: true,
				Timing: pgmo.TriggerAfter, Events: []pgmo.TriggerEvent{pgmo.TriggerInsert, pgmo.TriggerUpdate, pgmo.TriggerDelete},
				UpdateColumns: []string{"a", "user"}, When: "pg_trigger_depth() < 1",
				Function: pgmo.ObjectName{Schema: `My "S"`, Name: "user"}, Arguments: []string{"o'brien", `a\b`}})
			return err
		}, `CREATE OR REPLACE TRIGGER "T""r" AFTER INSERT OR UPDATE OF a, "user" OR DELETE ON "My ""S"""."user"` +
			` FOR EACH STATEMENT WHEN (pg_trigger_depth() < 1) EXECUTE FUNCTION "My ""S"""."user"('o''brien', E'a\\b')`},
		{"trigger create, constraint", func(ctx context.Context) error {
			_, err := d.CreateTrigger(ctx, pgmo.CreateTriggerRequest{Schema: "app", Table: "t", Name: "c", Constraint: true,
				Deferrable: true, InitiallyDeferred: true, Timing: pgmo.TriggerAfter, Events: []pgmo.TriggerEvent{pgmo.TriggerUpdate},
				ForEachRow: true, Function: pgmo.ObjectName{Schema: "app", Name: "f"}})
			return err
		}, `CREATE CONSTRAINT TRIGGER c AFTER UPDATE ON app.t DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION app.f()`},
		{"trigger create, transition tables", func(ctx context.Context) error {
			_, err := d.CreateTrigger(ctx, pgmo.CreateTriggerRequest{Schema: "app", Table: "t", Name: "s", Timing: pgmo.TriggerAfter,
				Events: []pgmo.TriggerEvent{pgmo.TriggerUpdate}, OldTable: "old rows", NewTable: "user",
				Function: pgmo.ObjectName{Schema: "app", Name: "f"}})
			return err
		}, `CREATE TRIGGER s AFTER UPDATE ON app.t REFERENCING OLD TABLE AS "old rows" NEW TABLE AS "user" FOR EACH STATEMENT EXECUTE FUNCTION app.f()`},
		{"trigger create, instead of, truncate", func(ctx context.Context) error {
			_, err := d.CreateTrigger(ctx, pgmo.CreateTriggerRequest{Schema: "app", Table: "v", Name: "i", Timing: pgmo.TriggerInsteadOf,
				Events: []pgmo.TriggerEvent{pgmo.TriggerTruncate}, Function: pgmo.ObjectName{Schema: "app", Name: "f"}})
			return err
		}, `CREATE TRIGGER i INSTEAD OF TRUNCATE ON app.v FOR EACH STATEMENT EXECUTE FUNCTION app.f()`},
		{"trigger drop", func(ctx context.Context) error { return tr.Drop(ctx, pgmo.DropOptions{}) }, `DROP TRIGGER "T""r" ON "My ""S"""."user"`},
		{"trigger drop cascade", func(ctx context.Context) error { return tr.Drop(ctx, pgmo.DropOptions{Cascade: true}) },
			`DROP TRIGGER "T""r" ON "My ""S"""."user" CASCADE`},
		{"trigger rename", func(ctx context.Context) error { return d.TriggerRef("app", "t", "tr").Rename(ctx, "user") },
			`ALTER TRIGGER tr ON app.t RENAME TO "user"`},
		{"trigger comment", func(ctx context.Context) error { return tr.SetComment(ctx, "o'brien") },
			`COMMENT ON TRIGGER "T""r" ON "My ""S"""."user" IS 'o''brien'`},
		{"trigger enable", func(ctx context.Context) error { return tr.SetEnabled(ctx, pgmo.TriggerEnabledOrigin) },
			`ALTER TABLE "My ""S"""."user" ENABLE TRIGGER "T""r"`},
		{"trigger enable replica", func(ctx context.Context) error { return tr.SetEnabled(ctx, pgmo.TriggerEnabledReplica) },
			`ALTER TABLE "My ""S"""."user" ENABLE REPLICA TRIGGER "T""r"`},
		{"trigger enable always", func(ctx context.Context) error { return tr.SetEnabled(ctx, pgmo.TriggerEnabledAlways) },
			`ALTER TABLE "My ""S"""."user" ENABLE ALWAYS TRIGGER "T""r"`},
		{"trigger disable", func(ctx context.Context) error { return tr.SetEnabled(ctx, pgmo.TriggerDisabled) },
			`ALTER TABLE "My ""S"""."user" DISABLE TRIGGER "T""r"`},

		// Event triggers
		{"event trigger create", func(ctx context.Context) error {
			_, err := d.CreateEventTrigger(ctx, pgmo.CreateEventTriggerRequest{Name: `E"t`, Event: pgmo.EventTriggerDDLCommandEnd,
				Function: pgmo.ObjectName{Schema: `My "S"`, Name: "user"}})
			return err
		}, `CREATE EVENT TRIGGER "E""t" ON ddl_command_end EXECUTE FUNCTION "My ""S"""."user"()`},
		{"event trigger create, tags", func(ctx context.Context) error {
			_, err := d.CreateEventTrigger(ctx, pgmo.CreateEventTriggerRequest{Name: "user", Event: pgmo.EventTriggerLogin,
				Tags: []string{"CREATE TABLE", "o'brien"}, Function: pgmo.ObjectName{Schema: "app", Name: "f"}})
			return err
		}, `CREATE EVENT TRIGGER "user" ON login WHEN TAG IN ('CREATE TABLE', 'o''brien') EXECUTE FUNCTION app.f()`},
		{"event trigger enable", func(ctx context.Context) error {
			return d.EventTriggerRef(`E"t`).SetEnabled(ctx, pgmo.TriggerEnabledOrigin)
		},
			`ALTER EVENT TRIGGER "E""t" ENABLE`},
		{"event trigger enable replica", func(ctx context.Context) error {
			return d.EventTriggerRef("e").SetEnabled(ctx, pgmo.TriggerEnabledReplica)
		},
			`ALTER EVENT TRIGGER e ENABLE REPLICA`},
		{"event trigger enable always", func(ctx context.Context) error {
			return d.EventTriggerRef("e").SetEnabled(ctx, pgmo.TriggerEnabledAlways)
		},
			`ALTER EVENT TRIGGER e ENABLE ALWAYS`},
		{"event trigger disable", func(ctx context.Context) error { return d.EventTriggerRef("e").SetEnabled(ctx, pgmo.TriggerDisabled) },
			`ALTER EVENT TRIGGER e DISABLE`},
		{"event trigger drop", func(ctx context.Context) error {
			return d.EventTriggerRef(`E"t`).Drop(ctx, pgmo.DropOptions{Cascade: true})
		},
			`DROP EVENT TRIGGER "E""t" CASCADE`},
		{"event trigger rename", func(ctx context.Context) error { return d.EventTriggerRef(`E"t`).Rename(ctx, "user") },
			`ALTER EVENT TRIGGER "E""t" RENAME TO "user"`},
		{"event trigger owner", func(ctx context.Context) error { return d.EventTriggerRef("e").SetOwner(ctx, "o'brien") },
			`ALTER EVENT TRIGGER e OWNER TO "o'brien"`},
		{"event trigger comment", func(ctx context.Context) error { return d.EventTriggerRef("e").SetComment(ctx, "o'brien") },
			`COMMENT ON EVENT TRIGGER e IS 'o''brien'`},

		// Extensions
		{"extension create", func(ctx context.Context) error {
			_, err := d.CreateExtension(ctx, pgmo.CreateExtensionRequest{Name: "citext"})
			return err
		}, `CREATE EXTENSION citext`},
		{"extension create, every option", func(ctx context.Context) error {
			_, err := d.CreateExtension(ctx, pgmo.CreateExtensionRequest{Name: `E"x`, Schema: `My "S"`, Version: "1.6'", Cascade: true})
			return err
		}, `CREATE EXTENSION "E""x" SCHEMA "My ""S""" VERSION '1.6''' CASCADE`},
		{"extension update", func(ctx context.Context) error { return ext.Update(ctx, "") }, `ALTER EXTENSION "E""x" UPDATE`},
		{"extension update to", func(ctx context.Context) error { return ext.Update(ctx, "1.6") }, `ALTER EXTENSION "E""x" UPDATE TO '1.6'`},
		{"extension drop", func(ctx context.Context) error { return ext.Drop(ctx, pgmo.DropOptions{Cascade: true}) }, `DROP EXTENSION "E""x" CASCADE`},
		{"extension set schema", func(ctx context.Context) error { return d.ExtensionRef("citext").SetSchema(ctx, "user") },
			`ALTER EXTENSION citext SET SCHEMA "user"`},
		{"extension comment", func(ctx context.Context) error { return ext.SetComment(ctx, `a\b`) }, `COMMENT ON EXTENSION "E""x" IS E'a\\b'`},
	})
}

// The cluster-wide writes of P4.13.3 — tablespaces, and the comments on a
// database and a role — run on the Server's own database.
func TestScriptClusterObjectWrites(t *testing.T) {
	f := pgmotest.New(t)
	s := f.Server()
	ts := s.TablespaceRef(`T"s`)
	runScriptCases(t, f, []scriptCase{
		{"tablespace create", func(ctx context.Context) error {
			_, err := s.CreateTablespace(ctx, pgmo.CreateTablespaceRequest{Name: `T"s`, Location: "/srv/o'brien"})
			return err
		}, `CREATE TABLESPACE "T""s" LOCATION '/srv/o''brien'`},
		{"tablespace create, every option", func(ctx context.Context) error {
			_, err := s.CreateTablespace(ctx, pgmo.CreateTablespaceRequest{Name: "user", Owner: "user",
				Options: []pgmo.ParameterSetting{{Name: "seq_page_cost", Value: "2"}, {Name: "random_page_cost", Value: "1.1"}}})
			return err
		}, `CREATE TABLESPACE "user" OWNER "user" LOCATION '' WITH (seq_page_cost = '2', random_page_cost = '1.1')`},
		{"tablespace drop", func(ctx context.Context) error { return ts.Drop(ctx) }, `DROP TABLESPACE "T""s"`},
		{"tablespace rename", func(ctx context.Context) error { return s.TablespaceRef(`T"s`).Rename(ctx, "user") },
			`ALTER TABLESPACE "T""s" RENAME TO "user"`},
		{"tablespace owner", func(ctx context.Context) error { return ts.SetOwner(ctx, "o'brien") }, `ALTER TABLESPACE "T""s" OWNER TO "o'brien"`},
		{"tablespace comment", func(ctx context.Context) error { return ts.SetComment(ctx, "o'brien") },
			`COMMENT ON TABLESPACE "T""s" IS 'o''brien'`},
		{"tablespace set options", func(ctx context.Context) error {
			return ts.SetOptions(ctx, pgmo.ParameterSetting{Name: "effective_io_concurrency", Value: "200"})
		}, `ALTER TABLESPACE "T""s" SET (effective_io_concurrency = '200')`},
		{"tablespace reset options", func(ctx context.Context) error { return ts.ResetOptions(ctx, "seq_page_cost", "random_page_cost") },
			`ALTER TABLESPACE "T""s" RESET (seq_page_cost, random_page_cost)`},
		{"database comment", func(ctx context.Context) error { return s.DatabaseRef(hostileDB).SetComment(ctx, "o'brien") },
			`COMMENT ON DATABASE "My ""DB""" IS 'o''brien'`},
		{"database comment removed", func(ctx context.Context) error { return s.DatabaseRef("app").SetComment(ctx, "") },
			`COMMENT ON DATABASE app IS NULL`},
		{"role comment", func(ctx context.Context) error { return s.RoleRef("user").SetComment(ctx, `a\b`) },
			`COMMENT ON ROLE "user" IS E'a\\b'`},
	})
}
