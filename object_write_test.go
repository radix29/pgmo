package pgmo_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/radix29/pgmo"
	"github.com/radix29/pgmo/pgmotest"
)

// refusalCase is one write that must be refused before anything is sent,
// with the sentinel it reaches.
type refusalCase struct {
	name string
	call func(context.Context) error
	kind error
}

// runRefusals runs each case with and without WithScript: each must reach
// its sentinel with one "pgmo: " prefix, sending and capturing nothing — to
// the Server's own database or to f's database "app", where the
// database-scoped cases write.
func runRefusals(t *testing.T, f *pgmotest.Fake, cases []refusalCase) {
	t.Helper()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			for _, scripting := range []bool{false, true} {
				ctx := t.Context()
				var script *pgmo.ScriptCollector
				if scripting {
					ctx, script = pgmo.WithScript(ctx)
				}
				before, beforeApp := len(f.Queries()), len(f.Database("app").Queries())
				err := c.call(ctx)
				if !errors.Is(err, c.kind) {
					t.Errorf("scripting %v: %v, want %v", scripting, err, c.kind)
				}
				if err != nil && (!strings.HasPrefix(err.Error(), "pgmo: ") || strings.Count(err.Error(), "pgmo:") != 1) {
					t.Errorf("error %q: want exactly one pgmo: prefix, leading", err)
				}
				if q := f.Queries()[before:]; len(q) != 0 {
					t.Errorf("scripting %v: sent %v before refusing", scripting, q)
				}
				if q := f.Database("app").Queries()[beforeApp:]; len(q) != 0 {
					t.Errorf("scripting %v: sent %v to app before refusing", scripting, q)
				}
				if script != nil && script.Len() != 0 {
					t.Errorf("captured %q before refusing", script)
				}
			}
		})
	}
}

// Every refusal of P4.13.3 is decided before anything is sent: a missing
// name ErrInvalidRequest, a missing schema ErrSchemaRequired, a request no
// server takes ErrInvalidRequest, an option the server's release predates
// ErrUnsupportedVersion.
func TestObjectWriteRefusals(t *testing.T) {
	f := pgmotest.New(t)
	f.Version = pgmo.PostgreSQL17
	s := f.Server()
	d := s.DatabaseRef("app")
	tb := d.TableRef("app", "t")
	noSchema := d.TableRef("", "t")
	fn := pgmo.ObjectName{Schema: "app", Name: "f"}
	table := func(req pgmo.CreateTableRequest) func(context.Context) error {
		return func(ctx context.Context) error {
			if req.Schema == "" && req.Name == "" {
				req.Schema, req.Name = "app", "t"
			}
			_, err := d.CreateTable(ctx, req)
			return err
		}
	}
	col := func(c pgmo.ColumnSpec) func(context.Context) error {
		return func(ctx context.Context) error { return tb.AddColumn(ctx, c) }
	}
	con := func(c pgmo.ConstraintSpec) func(context.Context) error {
		return func(ctx context.Context) error { return tb.AddConstraint(ctx, c) }
	}
	index := func(r pgmo.CreateIndexRequest) func(context.Context) error {
		return func(ctx context.Context) error { return tb.CreateIndex(ctx, r) }
	}
	function := func(r pgmo.CreateFunctionRequest) func(context.Context) error {
		return func(ctx context.Context) error {
			r.Schema, r.Name = "app", "f"
			if r.Language == "" {
				r.Language = "sql"
			}
			_, err := d.CreateFunction(ctx, r)
			return err
		}
	}
	trigger := func(r pgmo.CreateTriggerRequest) func(context.Context) error {
		return func(ctx context.Context) error {
			r.Schema, r.Table, r.Name = "app", "t", "tr"
			_, err := d.CreateTrigger(ctx, r)
			return err
		}
	}
	ins := []pgmo.TriggerEvent{pgmo.TriggerInsert}
	fk := pgmo.ConstraintSpec{Kind: pgmo.ConstraintForeignKey, Columns: []string{"a"}, ReferencedTable: pgmo.ObjectName{Schema: "app", Name: "p"}}
	withFK := func(m func(*pgmo.ConstraintSpec)) pgmo.ConstraintSpec { c := fk; m(&c); return c }
	inv, sch, ver := pgmo.ErrInvalidRequest, pgmo.ErrSchemaRequired, pgmo.ErrUnsupportedVersion

	runRefusals(t, f, []refusalCase{
		// Names
		{"create schema, no name", func(ctx context.Context) error { _, err := d.CreateSchema(ctx, pgmo.CreateSchemaRequest{}); return err }, inv},
		{"schema drop, no name", func(ctx context.Context) error { return d.SchemaRef("").Drop(ctx, pgmo.DropOptions{}) }, inv},
		{"create table, no name", table(pgmo.CreateTableRequest{Schema: "app"}), inv},
		{"create table, no schema", table(pgmo.CreateTableRequest{Name: "t"}), sch},
		{"table drop, no schema", func(ctx context.Context) error { return noSchema.Drop(ctx, pgmo.DropOptions{}) }, sch},
		{"table drop, no name", func(ctx context.Context) error { return d.TableRef("app", "").Drop(ctx, pgmo.DropOptions{}) }, inv},
		{"table rename, no new name", func(ctx context.Context) error { return tb.Rename(ctx, "") }, inv},
		{"table set schema, none", func(ctx context.Context) error { return tb.SetSchema(ctx, "") }, sch},
		{"table owner, no role", func(ctx context.Context) error { return tb.SetOwner(ctx, "") }, inv},
		{"table comment, no schema", func(ctx context.Context) error { return noSchema.SetComment(ctx, "x") }, sch},
		{"add column, no schema", func(ctx context.Context) error {
			return noSchema.AddColumn(ctx, pgmo.ColumnSpec{Name: "a", Type: "int"})
		}, sch},
		{"drop column, no name", func(ctx context.Context) error { return tb.DropColumn(ctx, "", pgmo.DropOptions{}) }, inv},
		{"rename column, no new name", func(ctx context.Context) error { return tb.RenameColumn(ctx, "a", "") }, inv},
		{"alter column, no name", func(ctx context.Context) error {
			return tb.AlterColumn(ctx, "", pgmo.ColumnChanges{NotNull: new(true)})
		}, inv},
		{"column comment, no name", func(ctx context.Context) error { return tb.SetColumnComment(ctx, "", "x") }, inv},
		{"drop constraint, no name", func(ctx context.Context) error { return tb.DropConstraint(ctx, "", pgmo.DropOptions{}) }, inv},
		{"rename constraint, no new name", func(ctx context.Context) error { return tb.RenameConstraint(ctx, "c", "") }, inv},
		{"validate constraint, no name", func(ctx context.Context) error { return tb.ValidateConstraint(ctx, "") }, inv},
		{"constraint comment, no name", func(ctx context.Context) error { return tb.SetConstraintComment(ctx, "", "x") }, inv},
		{"drop index, no name", func(ctx context.Context) error { return tb.DropIndex(ctx, "", pgmo.DropIndexOptions{}) }, inv},
		{"rename index, no new name", func(ctx context.Context) error { return tb.RenameIndex(ctx, "i", "") }, inv},
		{"index comment, no schema", func(ctx context.Context) error { return noSchema.SetIndexComment(ctx, "i", "x") }, sch},
		{"create view, no schema", func(ctx context.Context) error {
			_, err := d.CreateView(ctx, pgmo.CreateViewRequest{Name: "v", Query: "SELECT 1"})
			return err
		}, sch},
		{"create function, no name", func(ctx context.Context) error {
			_, err := d.CreateFunction(ctx, pgmo.CreateFunctionRequest{Schema: "app", Language: "sql"})
			return err
		}, inv},
		{"function drop, no schema", func(ctx context.Context) error { return d.FunctionRef("", "f", nil).Drop(ctx, pgmo.DropOptions{}) }, sch},
		{"procedure rename, no schema", func(ctx context.Context) error { return d.ProcedureRef("", "p", nil).Rename(ctx, "q") }, sch},
		{"sequence alter, no schema", func(ctx context.Context) error {
			return d.SequenceRef("", "s").Alter(ctx, pgmo.SequenceChanges{Restart: new(int64(1))})
		}, sch},
		{"type comment, no name", func(ctx context.Context) error { return d.TypeRef("app", "").SetComment(ctx, "x") }, inv},
		{"domain drop constraint, no name", func(ctx context.Context) error {
			return d.DomainRef("app", "d").DropConstraint(ctx, "", pgmo.DropOptions{})
		}, inv},
		{"create trigger, no table", func(ctx context.Context) error {
			_, err := d.CreateTrigger(ctx, pgmo.CreateTriggerRequest{Schema: "app", Name: "tr"})
			return err
		}, inv},
		{"trigger drop, no schema", func(ctx context.Context) error { return d.TriggerRef("", "t", "tr").Drop(ctx, pgmo.DropOptions{}) }, sch},
		{"trigger rename, no name", func(ctx context.Context) error { return d.TriggerRef("app", "t", "").Rename(ctx, "x") }, inv},
		{"create extension, no name", func(ctx context.Context) error {
			_, err := d.CreateExtension(ctx, pgmo.CreateExtensionRequest{})
			return err
		}, inv},
		{"extension update, no name", func(ctx context.Context) error { return d.ExtensionRef("").Update(ctx, "") }, inv},
		{"create tablespace, no name", func(ctx context.Context) error {
			_, err := s.CreateTablespace(ctx, pgmo.CreateTablespaceRequest{})
			return err
		}, inv},
		{"tablespace drop, no name", func(ctx context.Context) error { return s.TablespaceRef("").Drop(ctx) }, inv},
		{"database comment, no name", func(ctx context.Context) error { return s.DatabaseRef("").SetComment(ctx, "x") }, inv},
		{"role comment, no name", func(ctx context.Context) error { return s.RoleRef("").SetComment(ctx, "x") }, inv},

		// Columns
		{"column, no name", col(pgmo.ColumnSpec{Type: "int"}), inv},
		{"column, no type", col(pgmo.ColumnSpec{Name: "a"}), inv},
		{"column, unknown identity", col(pgmo.ColumnSpec{Name: "a", Type: "int", Identity: "x"}), inv},
		{"column, unknown generated", col(pgmo.ColumnSpec{Name: "a", Type: "int", Generated: "x", GenerationExpression: "1"}), inv},
		{"column, generated, no expression", col(pgmo.ColumnSpec{Name: "a", Type: "int", Generated: pgmo.GeneratedStored}), inv},
		{"column, expression, not generated", col(pgmo.ColumnSpec{Name: "a", Type: "int", GenerationExpression: "1"}), inv},
		{"column, identity and default", col(pgmo.ColumnSpec{Name: "a", Type: "int", Identity: pgmo.IdentityAlways, Default: "1"}), inv},
		{"column, generated and identity", col(pgmo.ColumnSpec{Name: "a", Type: "int", Identity: pgmo.IdentityAlways,
			Generated: pgmo.GeneratedStored, GenerationExpression: "1"}), inv},
		{"column, virtual on 17", col(pgmo.ColumnSpec{Name: "a", Type: "int", Generated: pgmo.GeneratedVirtual, GenerationExpression: "1"}), ver},
		{"create table, bad column", table(pgmo.CreateTableRequest{Columns: []pgmo.ColumnSpec{{Name: "a"}}}), inv},

		// Constraints
		{"constraint, unknown kind", con(pgmo.ConstraintSpec{Kind: "z"}), inv},
		{"constraint, not null kind", con(pgmo.ConstraintSpec{Kind: pgmo.ConstraintNotNull, Columns: []string{"a"}}), inv},
		{"constraint, key with no columns", con(pgmo.ConstraintSpec{Kind: pgmo.ConstraintPrimaryKey}), inv},
		{"constraint, empty column name", con(pgmo.ConstraintSpec{Kind: pgmo.ConstraintUnique, Columns: []string{"a", ""}}), inv},
		{"constraint, check with columns", con(pgmo.ConstraintSpec{Kind: pgmo.ConstraintCheck, Check: "a", Columns: []string{"a"}}), inv},
		{"constraint, check with no expression", con(pgmo.ConstraintSpec{Kind: pgmo.ConstraintCheck}), inv},
		{"constraint, check deferrable", con(pgmo.ConstraintSpec{Kind: pgmo.ConstraintCheck, Check: "a", Deferrable: true}), inv},
		{"constraint, include on a check", con(pgmo.ConstraintSpec{Kind: pgmo.ConstraintCheck, Check: "a", Include: []string{"b"}}), inv},
		{"constraint, nulls not distinct on a key", con(pgmo.ConstraintSpec{Kind: pgmo.ConstraintPrimaryKey, Columns: []string{"a"}, NullsNotDistinct: true}), inv},
		{"constraint, no inherit on a key", con(pgmo.ConstraintSpec{Kind: pgmo.ConstraintUnique, Columns: []string{"a"}, NoInherit: true}), inv},
		{"constraint, references on a key", con(pgmo.ConstraintSpec{Kind: pgmo.ConstraintUnique, Columns: []string{"a"}, OnDelete: pgmo.ForeignKeyCascade}), inv},
		{"constraint, exclude body on a check", con(pgmo.ConstraintSpec{Kind: pgmo.ConstraintCheck, Check: "a", Exclude: "x"}), inv},
		{"constraint, exclusion, no body", con(pgmo.ConstraintSpec{Kind: pgmo.ConstraintExclusion}), inv},
		{"constraint, not valid key", con(pgmo.ConstraintSpec{Kind: pgmo.ConstraintUnique, Columns: []string{"a"}, NotValid: true}), inv},
		{"constraint, not enforced key", con(pgmo.ConstraintSpec{Kind: pgmo.ConstraintUnique, Columns: []string{"a"}, NotEnforced: true}), inv},
		{"constraint, not enforced on 17", con(withFK(func(c *pgmo.ConstraintSpec) { c.NotEnforced = true })), ver},
		{"constraint, foreign key, no columns", con(withFK(func(c *pgmo.ConstraintSpec) { c.Columns = nil })), inv},
		{"constraint, foreign key, no schema", con(withFK(func(c *pgmo.ConstraintSpec) { c.ReferencedTable.Schema = "" })), sch},
		{"constraint, foreign key, unknown match", con(withFK(func(c *pgmo.ConstraintSpec) { c.Match = "x" })), inv},
		{"constraint, foreign key, unknown action", con(withFK(func(c *pgmo.ConstraintSpec) { c.OnUpdate = "x" })), inv},
		{"create table, not valid", table(pgmo.CreateTableRequest{Constraints: []pgmo.ConstraintSpec{
			withFK(func(c *pgmo.ConstraintSpec) { c.NotValid = true })}}), inv},

		// Tables
		{"partition with columns", table(pgmo.CreateTableRequest{PartitionOf: pgmo.ObjectName{Schema: "app", Name: "p"},
			PartitionBound: "DEFAULT", Columns: []pgmo.ColumnSpec{{Name: "a", Type: "int"}}}), inv},
		{"partition with parents", table(pgmo.CreateTableRequest{PartitionOf: pgmo.ObjectName{Schema: "app", Name: "p"},
			PartitionBound: "DEFAULT", Inherits: []pgmo.ObjectName{{Schema: "app", Name: "b"}}}), inv},
		{"partition, no bound", table(pgmo.CreateTableRequest{PartitionOf: pgmo.ObjectName{Schema: "app", Name: "p"}}), inv},
		{"partition, no parent schema", table(pgmo.CreateTableRequest{PartitionOf: pgmo.ObjectName{Name: "p"}, PartitionBound: "DEFAULT"}), sch},
		{"bound, no parent", table(pgmo.CreateTableRequest{PartitionBound: "DEFAULT"}), inv},
		{"inherits, no schema", table(pgmo.CreateTableRequest{Inherits: []pgmo.ObjectName{{Name: "b"}}}), sch},
		{"storage parameter, no name", table(pgmo.CreateTableRequest{Options: []pgmo.ParameterSetting{{Value: "1"}}}), inv},
		{"storage parameter, two dots", table(pgmo.CreateTableRequest{Options: []pgmo.ParameterSetting{{Name: "a.b.c", Value: "1"}}}), inv},
		{"storage parameter, empty namespace", table(pgmo.CreateTableRequest{Options: []pgmo.ParameterSetting{{Name: ".b", Value: "1"}}}), inv},
		{"alter column, using without type", func(ctx context.Context) error {
			return tb.AlterColumn(ctx, "a", pgmo.ColumnChanges{Using: "a::text"})
		}, inv},
		{"alter column, collation without type", func(ctx context.Context) error {
			return tb.AlterColumn(ctx, "a", pgmo.ColumnChanges{Collation: pgmo.ObjectName{Name: "C"}})
		}, inv},
		{"alter column, statistics below -1", func(ctx context.Context) error {
			return tb.AlterColumn(ctx, "a", pgmo.ColumnChanges{StatisticsTarget: new(-2)})
		}, inv},
		{"alter column, unknown storage", func(ctx context.Context) error {
			return tb.AlterColumn(ctx, "a", pgmo.ColumnChanges{Storage: "z"})
		}, inv},

		// Indexes
		{"index, no columns", index(pgmo.CreateIndexRequest{}), inv},
		{"index, column and expression", index(pgmo.CreateIndexRequest{Columns: []pgmo.IndexColumn{{Column: "a", Expression: "a"}}}), inv},
		{"index, neither", index(pgmo.CreateIndexRequest{Columns: []pgmo.IndexColumn{{Descending: true}}}), inv},
		{"index, nulls not distinct, not unique", index(pgmo.CreateIndexRequest{Columns: []pgmo.IndexColumn{{Column: "a"}}, NullsNotDistinct: true}), inv},
		{"index, empty include", index(pgmo.CreateIndexRequest{Columns: []pgmo.IndexColumn{{Column: "a"}}, Include: []string{""}}), inv},
		{"index, no schema", func(ctx context.Context) error {
			return noSchema.CreateIndex(ctx, pgmo.CreateIndexRequest{Columns: []pgmo.IndexColumn{{Column: "a"}}})
		}, sch},
		{"drop index, concurrently and cascade", func(ctx context.Context) error {
			return tb.DropIndex(ctx, "i", pgmo.DropIndexOptions{Concurrently: true, Cascade: true})
		}, inv},

		// Views
		{"view, no query", func(ctx context.Context) error {
			_, err := d.CreateView(ctx, pgmo.CreateViewRequest{Schema: "app", Name: "v"})
			return err
		}, inv},
		{"view, empty column", func(ctx context.Context) error {
			_, err := d.CreateView(ctx, pgmo.CreateViewRequest{Schema: "app", Name: "v", Query: "SELECT 1", Columns: []string{""}})
			return err
		}, inv},
		{"view, unknown check option", func(ctx context.Context) error {
			_, err := d.CreateView(ctx, pgmo.CreateViewRequest{Schema: "app", Name: "v", Query: "SELECT 1", CheckOption: "x"})
			return err
		}, inv},
		{"matview, no query", func(ctx context.Context) error {
			_, err := d.CreateMaterializedView(ctx, pgmo.CreateMaterializedViewRequest{Schema: "app", Name: "m"})
			return err
		}, inv},
		{"matview refresh, concurrently and no data", func(ctx context.Context) error {
			return d.MaterializedViewRef("app", "m").Refresh(ctx, pgmo.RefreshMaterializedViewOptions{Concurrently: true, NoData: true})
		}, inv},

		// Routines
		{"function, no language", func(ctx context.Context) error {
			_, err := d.CreateFunction(ctx, pgmo.CreateFunctionRequest{Schema: "app", Name: "f"})
			return err
		}, inv},
		{"function, argument with no type", function(pgmo.CreateFunctionRequest{Arguments: []pgmo.RoutineArgumentSpec{{Name: "a"}}}), inv},
		{"function, unknown mode", function(pgmo.CreateFunctionRequest{Arguments: []pgmo.RoutineArgumentSpec{{Mode: "x", Type: "int"}}}), inv},
		{"function, table column with no name", function(pgmo.CreateFunctionRequest{Arguments: []pgmo.RoutineArgumentSpec{{Mode: pgmo.ArgumentTable, Type: "int"}}}), inv},
		{"function, OUT with a default", function(pgmo.CreateFunctionRequest{Arguments: []pgmo.RoutineArgumentSpec{{Mode: pgmo.ArgumentOut, Type: "int", Default: "1"}}}), inv},
		{"function, table and returns", function(pgmo.CreateFunctionRequest{Returns: "int",
			Arguments: []pgmo.RoutineArgumentSpec{{Mode: pgmo.ArgumentTable, Name: "a", Type: "int"}}}), inv},
		{"function, setof with no type", function(pgmo.CreateFunctionRequest{ReturnsSet: true}), inv},
		{"function, unknown volatility", function(pgmo.CreateFunctionRequest{Volatility: "x"}), inv},
		{"function, unknown parallel", function(pgmo.CreateFunctionRequest{Parallel: "x"}), inv},
		{"function, setting with no name", function(pgmo.CreateFunctionRequest{Settings: []pgmo.ParameterSetting{{Value: "1"}}}), inv},
		{"procedure, returns table", func(ctx context.Context) error {
			_, err := d.CreateProcedure(ctx, pgmo.CreateProcedureRequest{Schema: "app", Name: "p", Language: "sql",
				Arguments: []pgmo.RoutineArgumentSpec{{Mode: pgmo.ArgumentTable, Name: "a", Type: "int"}}})
			return err
		}, inv},
		{"procedure, no language", func(ctx context.Context) error {
			_, err := d.CreateProcedure(ctx, pgmo.CreateProcedureRequest{Schema: "app", Name: "p"})
			return err
		}, inv},

		// Sequences
		{"sequence, owned by a table with no column", func(ctx context.Context) error {
			_, err := d.CreateSequence(ctx, pgmo.CreateSequenceRequest{Schema: "app", Name: "s",
				SequenceOptions: pgmo.SequenceOptions{OwnedBy: &pgmo.SequenceOwner{Table: pgmo.ObjectName{Schema: "app", Name: "t"}}}})
			return err
		}, inv},
		{"sequence, owned by, no schema", func(ctx context.Context) error {
			return d.SequenceRef("app", "s").Alter(ctx, pgmo.SequenceChanges{SequenceOptions: pgmo.SequenceOptions{
				OwnedBy: &pgmo.SequenceOwner{Table: pgmo.ObjectName{Name: "t"}, Column: "c"}}})
		}, sch},

		// Types and domains
		{"composite, attribute with no type", func(ctx context.Context) error {
			_, err := d.CreateCompositeType(ctx, pgmo.CreateCompositeTypeRequest{Schema: "app", Name: "c", Attributes: []pgmo.TypeAttributeSpec{{Name: "a"}}})
			return err
		}, inv},
		{"range, no subtype", func(ctx context.Context) error {
			_, err := d.CreateRangeType(ctx, pgmo.CreateRangeTypeRequest{Schema: "app", Name: "r"})
			return err
		}, inv},
		{"enum, no schema", func(ctx context.Context) error {
			_, err := d.CreateEnumType(ctx, pgmo.CreateEnumTypeRequest{Name: "e"})
			return err
		}, sch},
		{"enum value, before and after", func(ctx context.Context) error {
			return d.TypeRef("app", "e").AddEnumValue(ctx, "x", pgmo.EnumValuePosition{Before: "a", After: "b"})
		}, inv},
		{"domain, no base type", func(ctx context.Context) error {
			_, err := d.CreateDomain(ctx, pgmo.CreateDomainRequest{Schema: "app", Name: "d"})
			return err
		}, inv},
		{"domain, not valid in create", func(ctx context.Context) error {
			_, err := d.CreateDomain(ctx, pgmo.CreateDomainRequest{Schema: "app", Name: "d", BaseType: "int",
				Constraints: []pgmo.DomainConstraintSpec{{Check: "VALUE > 0", NotValid: true}}})
			return err
		}, inv},
		{"domain constraint, no check", func(ctx context.Context) error {
			return d.DomainRef("app", "d").AddConstraint(ctx, pgmo.DomainConstraintSpec{Name: "c"})
		}, inv},

		// Triggers
		{"trigger, unknown timing", trigger(pgmo.CreateTriggerRequest{Timing: "x", Events: ins, Function: fn}), inv},
		{"trigger, no events", trigger(pgmo.CreateTriggerRequest{Timing: pgmo.TriggerAfter, Function: fn}), inv},
		{"trigger, unknown event", trigger(pgmo.CreateTriggerRequest{Timing: pgmo.TriggerAfter, Events: []pgmo.TriggerEvent{"x"}, Function: fn}), inv},
		{"trigger, deferrable, not constraint", trigger(pgmo.CreateTriggerRequest{Timing: pgmo.TriggerAfter, Events: ins, Function: fn, Deferrable: true}), inv},
		{"trigger, function with no schema", trigger(pgmo.CreateTriggerRequest{Timing: pgmo.TriggerAfter, Events: ins,
			Function: pgmo.ObjectName{Name: "f"}}), sch},
		{"trigger, update columns with no update", trigger(pgmo.CreateTriggerRequest{Timing: pgmo.TriggerAfter, Events: ins,
			Function: fn, UpdateColumns: []string{"a"}}), inv},
		{"trigger, unknown state", func(ctx context.Context) error { return d.TriggerRef("app", "t", "tr").SetEnabled(ctx, "x") }, inv},

		// Event triggers
		{"event trigger, no name", func(ctx context.Context) error {
			_, err := d.CreateEventTrigger(ctx, pgmo.CreateEventTriggerRequest{Event: pgmo.EventTriggerSQLDrop, Function: fn})
			return err
		}, inv},
		{"event trigger, unknown event", func(ctx context.Context) error {
			_, err := d.CreateEventTrigger(ctx, pgmo.CreateEventTriggerRequest{Name: "e", Event: "x", Function: fn})
			return err
		}, inv},
		{"event trigger, function with no schema", func(ctx context.Context) error {
			_, err := d.CreateEventTrigger(ctx, pgmo.CreateEventTriggerRequest{Name: "e", Event: pgmo.EventTriggerSQLDrop, Function: pgmo.ObjectName{Name: "f"}})
			return err
		}, sch},
		{"event trigger, empty tag", func(ctx context.Context) error {
			_, err := d.CreateEventTrigger(ctx, pgmo.CreateEventTriggerRequest{Name: "e", Event: pgmo.EventTriggerSQLDrop, Function: fn, Tags: []string{""}})
			return err
		}, inv},
		{"event trigger, unknown state", func(ctx context.Context) error { return d.EventTriggerRef("e").SetEnabled(ctx, "x") }, inv},
		{"event trigger rename, no name", func(ctx context.Context) error { return d.EventTriggerRef("").Rename(ctx, "x") }, inv},

		// Tablespaces
		{"tablespace options, none", func(ctx context.Context) error { return s.TablespaceRef("t").SetOptions(ctx) }, inv},
		{"tablespace reset, none", func(ctx context.Context) error { return s.TablespaceRef("t").ResetOptions(ctx) }, inv},
		{"tablespace reset, bad name", func(ctx context.Context) error { return s.TablespaceRef("t").ResetOptions(ctx, "a.") }, inv},
	})
}

// The login event trigger is 17+.
func TestEventTriggerLoginGate(t *testing.T) {
	for _, c := range []struct {
		v    pgmo.ServerVersion
		kind error
	}{{pgmo.PostgreSQL16, pgmo.ErrUnsupportedVersion}, {pgmo.PostgreSQL17, nil}} {
		f := pgmotest.New(t)
		f.Version = c.v
		ctx, _ := pgmo.WithScript(t.Context())
		_, err := f.Server().DatabaseRef("app").CreateEventTrigger(ctx, pgmo.CreateEventTriggerRequest{Name: "e",
			Event: pgmo.EventTriggerLogin, Function: pgmo.ObjectName{Schema: "app", Name: "f"}})
		if !errors.Is(err, c.kind) && (c.kind != nil || err != nil) {
			t.Errorf("login on %d: %v, want %v", c.v, err, c.kind)
		}
	}
}

// On 18 a virtual generated column and NOT ENFORCED are written.
func TestObjectWriteGatesFollowTheServer(t *testing.T) {
	f := pgmotest.New(t)
	f.Version = pgmo.PostgreSQL18
	tb := f.Server().DatabaseRef("app").TableRef("app", "t")
	ctx, script := pgmo.WithScript(t.Context())
	if err := tb.AddColumn(ctx, pgmo.ColumnSpec{Name: "v", Type: "int", Generated: pgmo.GeneratedVirtual, GenerationExpression: "1"}); err != nil {
		t.Errorf("virtual column on 18: %v", err)
	}
	if err := tb.AddConstraint(ctx, pgmo.ConstraintSpec{Kind: pgmo.ConstraintCheck, Check: "v > 0", NotEnforced: true}); err != nil {
		t.Errorf("NOT ENFORCED on 18: %v", err)
	}
	if script.Len() != 2 {
		t.Errorf("captured %q", script)
	}
}

// CONCURRENTLY index builds and drops, CREATE TABLESPACE and DROP TABLESPACE
// cannot run in a transaction block: refused inside InTransaction,
// scripting or not. Every other write runs on the transaction when it is in
// the transaction's database, and is refused in any other.
func TestObjectWritesInTransaction(t *testing.T) {
	f := pgmotest.New(t)
	f.On("FROM pg_catalog.pg_namespace n", pgmotest.Rows())
	s := f.Server()
	own := s.DatabaseRef(pgmotest.DefaultDatabase)
	tb := own.TableRef("app", "t")
	other := s.DatabaseRef("other").TableRef("app", "t")
	refused := map[string]func(context.Context) error{
		"create index concurrently": func(ctx context.Context) error {
			return tb.CreateIndex(ctx, pgmo.CreateIndexRequest{Concurrently: true, Columns: []pgmo.IndexColumn{{Column: "a"}}})
		},
		"drop index concurrently": func(ctx context.Context) error {
			return tb.DropIndex(ctx, "i", pgmo.DropIndexOptions{Concurrently: true})
		},
		"create tablespace": func(ctx context.Context) error {
			_, err := s.CreateTablespace(ctx, pgmo.CreateTablespaceRequest{Name: "t"})
			return err
		},
		"drop tablespace":  func(ctx context.Context) error { return s.TablespaceRef("t").Drop(ctx) },
		"another database": func(ctx context.Context) error { return other.Drop(ctx, pgmo.DropOptions{}) },
	}
	for name, call := range refused {
		for _, scripting := range []bool{false, true} {
			ctx := t.Context()
			if scripting {
				ctx, _ = pgmo.WithScript(ctx)
			}
			before := len(f.Queries())
			var inner error
			_ = s.InTransaction(ctx, func(ctx context.Context) error {
				inner = call(ctx)
				return inner
			})
			if !errors.Is(inner, pgmo.ErrUnsupported) {
				t.Errorf("%s (scripting %v): %v, want ErrUnsupported", name, scripting, inner)
			}
			for _, q := range f.Queries()[before:] {
				if q.Kind == pgmotest.KindExec && q.Text != "begin" && q.Text != "rollback" {
					t.Errorf("%s: sent %q", name, q.Text)
				}
			}
		}
	}

	before := len(f.Queries())
	err := s.InTransaction(t.Context(), func(ctx context.Context) error {
		_, err := own.CreateSchema(ctx, pgmo.CreateSchemaRequest{Name: "app"})
		return errors.Join(err,
			tb.AddColumn(ctx, pgmo.ColumnSpec{Name: "a", Type: "int"}),
			tb.CreateIndex(ctx, pgmo.CreateIndexRequest{Name: "i", Columns: []pgmo.IndexColumn{{Column: "a"}}}),
			tb.DropIndex(ctx, "i", pgmo.DropIndexOptions{}),
			own.MaterializedViewRef("app", "m").Refresh(ctx, pgmo.RefreshMaterializedViewOptions{Concurrently: true}),
			own.TypeRef("app", "e").AddEnumValue(ctx, "x", pgmo.EnumValuePosition{}),
			s.TablespaceRef("t").Rename(ctx, "u"),
			own.SetComment(ctx, "x"),
			tb.Drop(ctx, pgmo.DropOptions{Cascade: true}),
		)
	})
	if err != nil {
		t.Fatal(err)
	}
	var stmts int
	for _, q := range f.Queries()[before:] {
		if q.Tx == 0 {
			t.Errorf("%q ran outside the transaction", q.Text)
		}
		if q.Kind == pgmotest.KindExec && q.Text != "begin" && q.Text != "commit" {
			stmts++
		}
	}
	if stmts != 9 {
		t.Errorf("sent %d statements on the transaction, want 9", stmts)
	}
}

// A write that ran is mirrored onto its handle — a name, a schema, an
// owner, a comment, a refresh's Populated, an enum's labels, a sequence's
// options, a trigger's state — and under WithScript it is not.
func TestObjectWritesMirrorOnlyWhenApplied(t *testing.T) {
	f := pgmotest.New(t)
	d := f.Server().DatabaseRef("app")
	type state struct {
		tb  *pgmo.Table
		mv  *pgmo.MaterializedView
		ty  *pgmo.Type
		seq *pgmo.Sequence
		tr  *pgmo.Trigger
		fn  *pgmo.Function
		dom *pgmo.Domain
		ext *pgmo.Extension
	}
	mk := func() state {
		ty := d.TypeRef("app", "e")
		ty.Labels = []string{"a", "c"}
		seq := d.SequenceRef("app", "s")
		seq.Increment, seq.Cycle = 1, false
		mv := d.MaterializedViewRef("app", "m")
		mv.Populated = true
		return state{d.TableRef("app", "t"), mv, ty, seq, d.TriggerRef("app", "t", "tr"),
			d.FunctionRef("app", "f", nil), d.DomainRef("app", "d"), d.ExtensionRef("x")}
	}
	apply := func(ctx context.Context, st state) {
		t.Helper()
		if err := errors.Join(
			st.tb.SetComment(ctx, "c"), st.tb.SetOwner(ctx, "o"), st.tb.SetSchema(ctx, "s2"), st.tb.Rename(ctx, "t2"),
			st.mv.Refresh(ctx, pgmo.RefreshMaterializedViewOptions{NoData: true}),
			st.ty.AddEnumValue(ctx, "b", pgmo.EnumValuePosition{After: "a"}),
			st.ty.AddEnumValue(ctx, "0", pgmo.EnumValuePosition{Before: "a"}),
			st.ty.AddEnumValue(ctx, "z", pgmo.EnumValuePosition{}),
			st.ty.RenameEnumValue(ctx, "c", "C"),
			st.seq.Alter(ctx, pgmo.SequenceChanges{SequenceOptions: pgmo.SequenceOptions{Increment: new(int64(5)), Cycle: new(true),
				OwnedBy: &pgmo.SequenceOwner{Table: pgmo.ObjectName{Schema: "app", Name: "t"}, Column: "id"}}}),
			st.tr.SetEnabled(ctx, pgmo.TriggerDisabled), st.tr.Rename(ctx, "tr2"),
			st.fn.SetOwner(ctx, "o"),
			st.dom.SetDefault(ctx, "1"), st.dom.SetNotNull(ctx, true),
			st.ext.Update(ctx, "2.0"), st.ext.SetSchema(ctx, "s3"),
		); err != nil {
			t.Fatal(err)
		}
	}

	scripted := mk()
	sctx, _ := pgmo.WithScript(t.Context())
	apply(sctx, scripted)
	if st := scripted; st.tb.Name != "t" || st.tb.Schema != "app" || st.tb.Owner != "" || st.tb.Comment != "" ||
		!st.mv.Populated || strings.Join(st.ty.Labels, ",") != "a,c" || st.seq.Increment != 1 || st.seq.Cycle ||
		st.tr.Enabled != "" || st.tr.Name != "tr" || st.fn.Owner != "" || st.dom.Default != "" || st.dom.NotNull ||
		st.ext.Version != "" || st.ext.Schema != "" || st.seq.OwnedByColumn != "" {
		t.Errorf("a scripted write was mirrored")
	}

	ran := mk()
	apply(t.Context(), ran)
	if st := ran; st.tb.Name != "t2" || st.tb.Schema != "s2" || st.tb.Owner != "o" || st.tb.Comment != "c" ||
		st.mv.Populated || strings.Join(st.ty.Labels, ",") != "0,a,b,C,z" || st.seq.Increment != 5 || !st.seq.Cycle ||
		st.seq.OwnedBy != (pgmo.ObjectName{Schema: "app", Name: "t"}) || st.seq.OwnedByColumn != "id" ||
		st.tr.Enabled != pgmo.TriggerDisabled || st.tr.Name != "tr2" || st.fn.Owner != "o" || st.dom.Default != "1" || !st.dom.NotNull ||
		st.ext.Version != "2.0" || st.ext.Schema != "s3" {
		t.Errorf("a write that ran was not mirrored: labels %q, %+v", ran.ty.Labels, ran.tb)
	}
	// The statements after a rename or a move name the new name.
	var sawMoved bool
	for _, q := range f.Database("app").Queries() {
		if q.Text == `ALTER TABLE s2.t RENAME TO t2` {
			sawMoved = true
		}
	}
	if !sawMoved {
		t.Errorf("the rename after the move did not name the moved table: %v", f.Queries())
	}
}

// The cluster-wide comments mirror too, only when applied.
func TestClusterCommentsMirrorOnlyWhenApplied(t *testing.T) {
	f := pgmotest.New(t)
	s := f.Server()
	for _, scripting := range []bool{true, false} {
		ctx := t.Context()
		if scripting {
			ctx, _ = pgmo.WithScript(ctx)
		}
		d, r, ts := s.DatabaseRef("app"), s.RoleRef("r"), s.TablespaceRef("t")
		ev := d.EventTriggerRef("e")
		if err := errors.Join(ev.SetEnabled(ctx, pgmo.TriggerDisabled), ev.SetOwner(ctx, "o"), ev.Rename(ctx, "e2")); err != nil {
			t.Fatal(err)
		}
		if mirrored := ev.Enabled == pgmo.TriggerDisabled && ev.Owner == "o" && ev.Name == "e2"; mirrored == scripting {
			t.Errorf("scripting %v: event trigger %+v", scripting, ev)
		}
		if err := errors.Join(d.SetComment(ctx, "d"), r.SetComment(ctx, "r"), ts.SetComment(ctx, "t")); err != nil {
			t.Fatal(err)
		}
		if mirrored := d.Comment == "d" && r.Comment == "r" && ts.Comment == "t"; mirrored == scripting {
			t.Errorf("scripting %v: comments %q %q %q", scripting, d.Comment, r.Comment, ts.Comment)
		}
	}
}

// An empty ColumnChanges or SequenceChanges sends nothing.
func TestObjectAlterNothingSendsNothing(t *testing.T) {
	f := pgmotest.New(t)
	d := f.Server().DatabaseRef("app")
	if err := errors.Join(
		d.TableRef("app", "t").AlterColumn(t.Context(), "a", pgmo.ColumnChanges{}),
		d.SequenceRef("app", "s").Alter(t.Context(), pgmo.SequenceChanges{}),
	); err != nil {
		t.Fatal(err)
	}
	wantSent(t, f)
}

// CreateX reads what it created back; under WithScript it returns the
// handle, reading nothing; a read-back that finds nothing returns the
// handle, and so does CreateFunction's when overloads share the name; a
// refused create is the error, wrapped once.
func TestCreateObjectsReadBack(t *testing.T) {
	f := pgmotest.New(t)
	app := f.Database("app")
	app.On("FROM pg_catalog.pg_namespace n", pgmotest.Rows())
	app.On("FROM pg_catalog.pg_proc p", pgmotest.Rows())
	d := f.Server().DatabaseRef("app")
	sc, err := d.CreateSchema(t.Context(), pgmo.CreateSchemaRequest{Name: "app"})
	if err != nil || sc.Name != "app" || sc.OID != 0 {
		t.Errorf("create read back as nothing: %+v, %v; want the handle", sc, err)
	}
	if q := app.Queries(); len(q) != 2 || q[0].Text != "CREATE SCHEMA app" || q[1].Arg(0) != "app" {
		t.Errorf("sent %v, want CREATE SCHEMA then the read by its name", q)
	}
	fn, err := d.CreateFunction(t.Context(), pgmo.CreateFunctionRequest{Schema: "app", Name: "f",
		Arguments: []pgmo.RoutineArgumentSpec{{Type: "int"}, {Mode: pgmo.ArgumentOut, Type: "text"}}, Language: "sql", Body: "SELECT 1"})
	if err != nil || fn.Signature() != "app.f(int)" || fn.OID != 0 {
		t.Errorf("function read back as nothing: %+v, %v; want the handle app.f(int)", fn, err)
	}

	sctx, _ := pgmo.WithScript(t.Context())
	n := len(app.Queries())
	tb, err := d.CreateTable(sctx, pgmo.CreateTableRequest{Schema: "app", Name: "t"})
	if err != nil || tb.Name != "t" || tb.Schema != "app" || len(app.Queries()) != n {
		t.Errorf("scripted create: %+v, %v, sent %d", tb, err, len(app.Queries())-n)
	}

	f3 := pgmotest.New(t)
	f3.Database("app").OnExec("CREATE TABLE", pgmotest.Fail(&pgconn.PgError{Code: "42P07"}))
	_, err = f3.Server().DatabaseRef("app").CreateTable(t.Context(), pgmo.CreateTableRequest{Schema: "app", Name: "t"})
	if !pgmo.IsAlreadyExists(err) || strings.Count(err.Error(), "pgmo:") != 1 ||
		!strings.HasPrefix(err.Error(), `pgmo: create table app.t: `) {
		t.Errorf("refused create: %v", err)
	}
}
