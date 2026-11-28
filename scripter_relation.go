package pgmo

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode"

	"github.com/radix29/pgmo/pgsql"
)

// ============================================================
// Scripting relations: tables, views, materialized views, foreign tables,
// and a table's indexes, constraints, triggers and policies
// ============================================================

// tableParts is everything a table's script restates, read in one render.
type tableParts struct {
	table       *Table
	columns     []*Column
	constraints []*Constraint
	indexes     []*Index
	triggers    []*Trigger
	policies    []*Policy
}

func (sc *Scripter) readTable(ctx context.Context, schema, name string) (*tableParts, error) {
	t, err := sc.db.TableByName(ctx, schema, name)
	if err != nil {
		return nil, err
	}
	p := &tableParts{table: t}
	if p.columns, err = t.Columns(ctx); err != nil {
		return nil, err
	}
	if p.constraints, err = t.Constraints(ctx); err != nil {
		return nil, err
	}
	if p.indexes, err = t.Indexes(ctx); err != nil {
		return nil, err
	}
	if p.triggers, err = t.Triggers(ctx); err != nil {
		return nil, err
	}
	if p.policies, err = t.Policies(ctx); err != nil {
		return nil, err
	}
	return p, nil
}

// ScriptTable scripts the table schema.name (ordinary or partitioned, a
// partition included): CREATE TABLE with its columns and its constraints
// but foreign keys, then the foreign keys and any constraint not yet
// validated as ALTER TABLE … ADD CONSTRAINT, column settings, indexes,
// row-level security and its policies, triggers, owner, comments and
// privileges. Its partitions and inheritance children are scripted on
// their own. A table an extension owns is a note saying so.
func (sc *Scripter) ScriptTable(ctx context.Context, schema, name string) (string, error) {
	return scriptOf(ctx, sc.in(schema), func(ctx context.Context) (*tableParts, error) {
		return sc.readTable(ctx, schema, name)
	}, sc.tableScript)
}

func (sc *Scripter) tableScript(p *tableParts) (string, error) {
	o, t := sc.opts, p.table
	name := o.name(t.Schema, t.Name)
	if s, ok := o.member("table "+name, t.Origin); ok {
		return s, nil
	}
	op := "script table " + pgsql.QualifiedName(t.Schema, t.Name)
	return o.script(op, o.drop("TABLE", name), func(s *scriptText) error {
		create, err := sc.createTable(p, name)
		if err != nil {
			return wrapScript(op, err)
		}
		s.add(create)
		s.add(sc.tableTail(p, name)...)
		return nil
	})
}

// wrapScript prefixes a builder's refusal, which has none, with the
// operation.
func wrapScript(op string, err error) error {
	return fmt.Errorf("pgmo: %s: %w", op, err)
}

// createTable is the table's CREATE TABLE statement.
func (sc *Scripter) createTable(p *tableParts, name string) (string, error) {
	o, t := sc.opts, p.table
	var elems []string
	partition := t.IsPartition && len(t.Parents) == 1
	if !partition {
		for _, c := range p.columns {
			if !c.IsLocal {
				continue // inherited: the parent's
			}
			def, err := columnDefinition(c, int(sc.db.server.version))
			if err != nil {
				return "", err
			}
			elems = append(elems, def)
		}
	}
	for _, c := range p.constraints {
		if inlineConstraint(c) {
			elems = append(elems, "CONSTRAINT "+pgsql.QuoteIdent(c.Name)+" "+c.Definition)
		}
	}
	var b strings.Builder
	b.WriteString("CREATE ")
	switch t.Persistence {
	case PersistenceUnlogged:
		b.WriteString("UNLOGGED ")
	case PersistenceTemporary:
		b.WriteString("TEMPORARY ")
		name = pgsql.QuoteIdent(t.Name) // a temporary table is created in the session's own schema
	}
	b.WriteString("TABLE" + o.ifNotExists() + " " + name)
	if partition {
		b.WriteString(" PARTITION OF " + t.Parents[0].QualifiedName())
		if len(elems) > 0 {
			b.WriteString(" (\n    " + strings.Join(elems, ",\n    ") + "\n)")
		}
		b.WriteString("\n" + t.PartitionBound)
	} else {
		b.WriteString(" (")
		if len(elems) > 0 {
			b.WriteString("\n    " + strings.Join(elems, ",\n    ") + "\n")
		}
		b.WriteString(")")
		if len(t.Parents) > 0 {
			parents := make([]string, len(t.Parents))
			for i, par := range t.Parents {
				parents[i] = par.QualifiedName()
			}
			b.WriteString("\nINHERITS (" + strings.Join(parents, ", ") + ")")
		}
	}
	if t.PartitionKey != "" {
		b.WriteString("\nPARTITION BY " + t.PartitionKey)
	}
	opts, err := storageParameters(t.Options)
	if err != nil {
		return "", err
	}
	if opts != "" {
		b.WriteString("\nWITH " + opts)
	}
	if t.Tablespace != "" {
		b.WriteString("\nTABLESPACE " + pgsql.QuoteIdent(t.Tablespace))
	}
	return b.String(), nil
}

// columnDefinition is a column as CREATE TABLE writes it, through the
// write path's ColumnSpec.
func columnDefinition(c *Column, num int) (string, error) {
	spec := ColumnSpec{Name: c.Name, Type: c.Type, Collation: c.Collation, NotNull: c.NotNull,
		Default: c.Default, Identity: c.Identity, Generated: c.Generated, GenerationExpression: c.GenerationExpression}
	if len(c.FDWOptions) > 0 {
		spec.Type += " OPTIONS " + genericOptions(c.FDWOptions)
	}
	return spec.definition(num)
}

// inlineConstraint reports a constraint written inside CREATE TABLE: the
// table's own (not inherited), validated, and not a foreign key — those
// follow the CREATE, so a script of several tables can create them all
// first — nor a NOT NULL constraint (18), which the column's NOT NULL
// writes.
func inlineConstraint(c *Constraint) bool {
	return c.IsLocal && c.Validated && c.Kind != ConstraintForeignKey && c.Kind != ConstraintNotNull
}

// alterTable is ALTER TABLE for a statement on the table alone: ONLY for an
// ordinary table, so an inheritance child is not changed with it; a
// partitioned table takes the statements for its partitions too, as
// CREATE TABLE … PARTITION OF would give them.
func alterTable(t *Table, name string) string {
	if t.Kind == RelationPartitionedTable {
		return "ALTER TABLE " + name
	}
	return "ALTER TABLE ONLY " + name
}

// tableTail is every statement after the CREATE TABLE.
func (sc *Scripter) tableTail(p *tableParts, name string) []string {
	o, t := sc.opts, p.table
	alter := alterTable(t, name)
	var stmts []string
	for _, c := range p.constraints {
		if c.IsLocal && c.Kind != ConstraintNotNull && !inlineConstraint(c) {
			stmts = append(stmts, alter+" ADD CONSTRAINT "+pgsql.QuoteIdent(c.Name)+" "+c.Definition)
		}
	}
	for _, c := range p.columns {
		if c.StatisticsTarget >= 0 {
			stmts = append(stmts, alter+" ALTER COLUMN "+pgsql.QuoteIdent(c.Name)+" SET STATISTICS "+strconv.Itoa(c.StatisticsTarget))
		}
	}
	stmts = append(stmts, sc.indexStatements(p.indexes, name, t.Kind == RelationPartitionedTable)...)
	if t.RowSecurity {
		stmts = append(stmts, "ALTER TABLE "+name+" ENABLE ROW LEVEL SECURITY")
	}
	if t.ForceRowSecurity {
		stmts = append(stmts, "ALTER TABLE "+name+" FORCE ROW LEVEL SECURITY")
	}
	for _, pol := range p.policies {
		stmts = append(stmts, policyCreate(pol, name))
	}
	for _, tr := range p.triggers {
		if !tr.Inherited {
			stmts = append(stmts, sc.triggerStatements(tr, name, false)...)
		}
	}
	stmts = append(stmts, o.owner("TABLE", name, t.Owner))
	stmts = append(stmts, o.comment("TABLE "+name, t.Comment))
	for _, c := range p.columns {
		stmts = append(stmts, o.comment("COLUMN "+name+"."+pgsql.QuoteIdent(c.Name), c.Comment))
	}
	for _, c := range p.constraints {
		if c.IsLocal {
			stmts = append(stmts, o.comment("CONSTRAINT "+pgsql.QuoteIdent(c.Name)+" ON "+name, c.Comment))
		}
	}
	for _, i := range p.indexes {
		if i.Constraint == "" && i.Parent.Name == "" {
			stmts = append(stmts, o.comment("INDEX "+o.name(t.Schema, i.Name), i.Comment))
		}
	}
	for _, pol := range p.policies {
		stmts = append(stmts, o.comment("POLICY "+pgsql.QuoteIdent(pol.Name)+" ON "+name, pol.Comment))
	}
	for _, tr := range p.triggers {
		if !tr.Inherited {
			stmts = append(stmts, o.comment("TRIGGER "+pgsql.QuoteIdent(tr.Name)+" ON "+name, tr.Comment))
		}
	}
	stmts = append(stmts, o.privileges("TABLE "+name, SecurableTable, t.Owner, t.Privileges, t.PrivilegesDefault, sc.db.server.version)...)
	if o.IncludePrivileges {
		stmts = append(stmts, columnACLStatements(name, p.columns)...)
	}
	return stmts
}

// ------------------------------------------------------------
// Indexes
// ------------------------------------------------------------

// indexStatements are the CREATE INDEX statements of a relation's indexes
// that no constraint creates, then CLUSTER ON for the clustered one. rel is
// the relation's name as the script writes it; partitioned says it is a
// partitioned table, whose indexes the server renders ON ONLY — created on
// the parent alone and invalid until each partition's is attached. The
// script writes them without ONLY: run after the CREATE TABLE, before any
// partition exists, that is the same index, and a partition created later
// gets its own.
func (sc *Scripter) indexStatements(indexes []*Index, rel string, partitioned bool) []string {
	var stmts []string
	for _, i := range indexes {
		if i.Constraint != "" || i.Parent.Name != "" {
			continue // the constraint's, or the parent's index's: created with it
		}
		stmts = append(stmts, sc.indexCreate(i, partitioned))
		if i.Clustered {
			stmts = append(stmts, "ALTER TABLE "+rel+" CLUSTER ON "+pgsql.QuoteIdent(i.Name))
		}
	}
	return stmts
}

// indexCreate is the index's server-rendered definition with IF NOT EXISTS
// and its tablespace added, and ON ONLY dropped for a partitioned table's.
func (sc *Scripter) indexCreate(i *Index, partitioned bool) string {
	def := i.Definition
	if sc.opts.IfNotExists {
		for _, head := range []string{"CREATE UNIQUE INDEX ", "CREATE INDEX "} {
			if rest, ok := strings.CutPrefix(def, head); ok {
				def = head + "IF NOT EXISTS " + rest
				break
			}
		}
	}
	if partitioned {
		if at := topLevelKeyword(def, "ON ONLY"); at >= 0 {
			def = def[:at] + "ON" + def[at+len("ON ONLY"):]
		}
	}
	if i.Tablespace != "" {
		ts := " TABLESPACE " + pgsql.QuoteIdent(i.Tablespace)
		if at := topLevelKeyword(def, "WHERE"); at >= 0 {
			def = strings.TrimRight(def[:at], " ") + ts + " " + def[at:]
		} else {
			def += ts
		}
	}
	return def
}

// topLevelKeyword is the byte offset of the keyword phrase kw (words
// separated by single spaces, upper case) in sql's code at parenthesis depth
// zero, as a whole word, or -1: never inside a literal, a quoted name, a
// comment or a parenthesised expression.
func topLevelKeyword(sql, kw string) int {
	depth := 0
	for sp := range pgsql.Spans(sql) {
		if sp.Kind != pgsql.SpanCode {
			continue
		}
		code := sql[sp.Start:sp.End]
		for i := 0; i < len(code); i++ {
			switch code[i] {
			case '(':
				depth++
				continue
			case ')':
				depth--
				continue
			}
			if depth != 0 || !strings.HasPrefix(strings.ToUpper(code[i:min(len(code), i+len(kw))]), kw) {
				continue
			}
			before := i == 0 || !isIdentByte(code[i-1])
			after := i+len(kw) >= len(code) || !isIdentByte(code[i+len(kw)])
			if before && after {
				return sp.Start + i
			}
		}
	}
	return -1
}

// isIdentByte reports a byte that continues an unquoted identifier.
func isIdentByte(c byte) bool {
	return c == '_' || c == '$' || c >= 0x80 || unicode.IsLetter(rune(c)) || unicode.IsDigit(rune(c))
}

// ScriptIndex scripts the index name of the table or materialized view
// schema.table: its CREATE INDEX (the server's definition), CLUSTER ON
// when the relation is clustered on it, and its comment. An index a
// constraint creates is scripted with the constraint (ScriptConstraint):
// ErrInvalidRequest here.
func (sc *Scripter) ScriptIndex(ctx context.Context, schema, table, name string) (string, error) {
	type found struct {
		index       *Index
		partitioned bool
	}
	op := "script index " + pgsql.QualifiedName(schema, name) + " on " + pgsql.QualifiedName(schema, table)
	return scriptOf(ctx, sc.in(schema), func(ctx context.Context) (found, error) {
		var indexes []*Index
		partitioned := false
		t, err := sc.db.TableByName(ctx, schema, table)
		switch {
		case err == nil:
			partitioned = t.Kind == RelationPartitionedTable
			indexes, err = t.Indexes(ctx)
		case errors.Is(err, ErrNotFound):
			var m *MaterializedView
			if m, err = sc.db.MaterializedViewByName(ctx, schema, table); err == nil {
				indexes, err = m.Indexes(ctx)
			}
		}
		if err != nil {
			return found{}, err
		}
		for _, i := range indexes {
			if i.Name == name {
				return found{i, partitioned}, nil
			}
		}
		return found{}, notFoundf("pgmo: %s: no such index", op)
	}, func(f found) (string, error) {
		o, i := sc.opts, f.index
		switch {
		case i.Constraint != "":
			return "", invalidf("pgmo: %s: the index is constraint %s's; script the constraint", op, pgsql.QuoteIdent(i.Constraint))
		case i.Parent.Name != "":
			return "", invalidf("pgmo: %s: the index is attached to %s, created with it; script that one", op, i.Parent.QualifiedName())
		}
		iname := o.name(schema, i.Name)
		return o.script(op, o.drop("INDEX", iname), func(s *scriptText) error {
			s.add(sc.indexStatements([]*Index{i}, o.name(schema, table), f.partitioned)...)
			s.add(o.comment("INDEX "+iname, i.Comment))
			return nil
		})
	})
}

// ------------------------------------------------------------
// Constraints
// ------------------------------------------------------------

// ScriptConstraint scripts the constraint name of the table schema.table as
// ALTER TABLE … ADD CONSTRAINT with the server's definition (an index it
// creates comes with it), and its comment; DROP is ALTER TABLE … DROP
// CONSTRAINT. A constraint the table inherits is its parent's:
// ErrInvalidRequest.
func (sc *Scripter) ScriptConstraint(ctx context.Context, schema, table, name string) (string, error) {
	type found struct {
		table      *Table
		constraint *Constraint
	}
	op := "script constraint " + pgsql.QuoteIdent(name) + " on " + pgsql.QualifiedName(schema, table)
	return scriptOf(ctx, sc.in(schema), func(ctx context.Context) (found, error) {
		t, err := sc.db.TableByName(ctx, schema, table)
		if err != nil {
			return found{}, err
		}
		cs, err := t.Constraints(ctx)
		if err != nil {
			return found{}, err
		}
		for _, c := range cs {
			if c.Name == name {
				return found{t, c}, nil
			}
		}
		return found{}, notFoundf("pgmo: %s: no such constraint", op)
	}, func(f found) (string, error) {
		o, c := sc.opts, f.constraint
		if !c.IsLocal {
			return "", invalidf("pgmo: %s: the constraint is inherited; script the parent's", op)
		}
		tname := o.name(schema, table)
		alter := alterTable(f.table, tname)
		cname := pgsql.QuoteIdent(c.Name)
		drop := alter + " DROP CONSTRAINT "
		if o.IfExists {
			drop += "IF EXISTS "
		}
		drop += cname
		if o.Cascade {
			drop += " CASCADE"
		}
		return o.script(op, drop, func(s *scriptText) error {
			s.add(alter + " ADD CONSTRAINT " + cname + " " + c.Definition)
			s.add(o.comment("CONSTRAINT "+cname+" ON "+tname, c.Comment))
			return nil
		})
	})
}

// ------------------------------------------------------------
// Triggers
// ------------------------------------------------------------

// triggerStatements are the trigger's CREATE TRIGGER — the server's
// definition, OR REPLACE when orReplace; a constraint trigger, which has no
// such form, reads CREATE CONSTRAINT TRIGGER and is left as it is — and the
// ENABLE or DISABLE restating a state other than the default. rel is its
// relation's name as the script writes it.
func (sc *Scripter) triggerStatements(t *Trigger, rel string, orReplace bool) []string {
	def := t.Definition
	if orReplace {
		if rest, ok := strings.CutPrefix(def, "CREATE TRIGGER "); ok {
			def = "CREATE OR REPLACE TRIGGER " + rest
		}
	}
	stmts := []string{def}
	alter := "ALTER TABLE "
	if t.TableKind == RelationForeignTable {
		alter = "ALTER FOREIGN TABLE "
	}
	tname := pgsql.QuoteIdent(t.Name)
	switch t.Enabled {
	case TriggerDisabled:
		stmts = append(stmts, alter+rel+" DISABLE TRIGGER "+tname)
	case TriggerEnabledReplica:
		stmts = append(stmts, alter+rel+" ENABLE REPLICA TRIGGER "+tname)
	case TriggerEnabledAlways:
		stmts = append(stmts, alter+rel+" ENABLE ALWAYS TRIGGER "+tname)
	}
	return stmts
}

// ScriptTrigger scripts the trigger name on the table, view or foreign
// table schema.table: the server's CREATE TRIGGER (OR REPLACE under
// ScriptAlter, but for a constraint trigger), its enabled state and its
// comment. A partition's copy of its parent's trigger is the parent's:
// ErrInvalidRequest.
func (sc *Scripter) ScriptTrigger(ctx context.Context, schema, table, name string) (string, error) {
	op := "script trigger " + pgsql.QuoteIdent(name) + " on " + pgsql.QualifiedName(schema, table)
	return scriptOf(ctx, sc.in(schema), func(ctx context.Context) (*Trigger, error) {
		return sc.db.TriggerByName(ctx, schema, table, name)
	}, func(t *Trigger) (string, error) {
		o := sc.opts
		rel := o.name(t.Schema, t.Table)
		if s, ok := o.member("trigger "+pgsql.QuoteIdent(t.Name)+" on "+rel, t.Origin); ok {
			return s, nil
		}
		if t.Inherited {
			return "", invalidf("pgmo: %s: the trigger is a partition's copy of its parent's; script the parent's", op)
		}
		return o.script(op, o.dropOn("TRIGGER", pgsql.QuoteIdent(t.Name), rel), func(s *scriptText) error {
			s.add(sc.triggerStatements(t, rel, o.Verb == ScriptAlter)...)
			s.add(o.comment("TRIGGER "+pgsql.QuoteIdent(t.Name)+" ON "+rel, t.Comment))
			return nil
		})
	})
}

// ------------------------------------------------------------
// Policies
// ------------------------------------------------------------

// policyCreate is the policy's CREATE POLICY on the table rel, from its
// fields: PostgreSQL has no function rendering one.
func policyCreate(p *Policy, rel string) string {
	var b strings.Builder
	b.WriteString("CREATE POLICY " + pgsql.QuoteIdent(p.Name) + " ON " + rel)
	if !p.Permissive {
		b.WriteString(" AS RESTRICTIVE")
	}
	if p.Command != PolicyAll && p.Command != "" {
		b.WriteString(" FOR " + p.Command.String())
	}
	if len(p.Roles) > 0 && !(len(p.Roles) == 1 && p.Roles[0] == "") {
		b.WriteString(" TO " + roleSpecs(p.Roles))
	}
	if p.Using != "" {
		b.WriteString(" USING (" + p.Using + ")")
	}
	if p.WithCheck != "" {
		b.WriteString(" WITH CHECK (" + p.WithCheck + ")")
	}
	return b.String()
}

// ScriptPolicy scripts the row-level security policy name on the table
// schema.table: CREATE POLICY and its comment. Whether the table enforces
// its policies (ENABLE ROW LEVEL SECURITY) is the table's script.
func (sc *Scripter) ScriptPolicy(ctx context.Context, schema, table, name string) (string, error) {
	op := "script policy " + pgsql.QuoteIdent(name) + " on " + pgsql.QualifiedName(schema, table)
	return scriptOf(ctx, sc.in(schema), func(ctx context.Context) (*Policy, error) {
		return sc.db.PolicyByName(ctx, schema, table, name)
	}, func(p *Policy) (string, error) {
		o := sc.opts
		rel := o.name(p.Schema, p.Table)
		pname := pgsql.QuoteIdent(p.Name)
		return o.script(op, o.dropOn("POLICY", pname, rel), func(s *scriptText) error {
			s.add(policyCreate(p, rel))
			s.add(o.comment("POLICY "+pname+" ON "+rel, p.Comment))
			return nil
		})
	})
}

// ------------------------------------------------------------
// Views and materialized views
// ------------------------------------------------------------

// viewQuery is a view's server-rendered query as AS takes it: pg_get_viewdef
// gives it with a leading space and a closing semicolon.
func viewQuery(def string) string {
	return strings.TrimSuffix(strings.TrimSpace(def), ";")
}

type viewParts struct {
	view     *View
	columns  []*Column
	triggers []*Trigger
}

// ScriptView scripts the view schema.name: CREATE VIEW (OR REPLACE under
// ScriptAlter) with its options and the server's rendering of its query,
// column defaults, triggers, owner, comments and privileges.
func (sc *Scripter) ScriptView(ctx context.Context, schema, name string) (string, error) {
	op := "script view " + pgsql.QualifiedName(schema, name)
	return scriptOf(ctx, sc.in(schema), func(ctx context.Context) (*viewParts, error) {
		v, err := sc.db.ViewByName(ctx, schema, name)
		if err != nil {
			return nil, err
		}
		p := &viewParts{view: v}
		if p.columns, err = v.Columns(ctx); err != nil {
			return nil, err
		}
		if p.triggers, err = v.Triggers(ctx); err != nil {
			return nil, err
		}
		return p, nil
	}, func(p *viewParts) (string, error) {
		o, v := sc.opts, p.view
		vname := o.name(v.Schema, v.Name)
		if s, ok := o.member("view "+vname, v.Origin); ok {
			return s, nil
		}
		return o.script(op, o.drop("VIEW", vname), func(s *scriptText) error {
			opts, err := storageParameters(v.Options)
			if err != nil {
				return wrapScript(op, err)
			}
			stmt := "CREATE " + o.orReplace() + "VIEW " + vname
			if opts != "" {
				stmt += " WITH " + opts
			}
			s.add(stmt + " AS\n" + viewQuery(v.Definition))
			for _, c := range p.columns {
				if c.Default != "" {
					s.add("ALTER VIEW " + vname + " ALTER COLUMN " + pgsql.QuoteIdent(c.Name) + " SET DEFAULT " + c.Default)
				}
			}
			for _, t := range p.triggers {
				s.add(sc.triggerStatements(t, vname, false)...)
			}
			s.add(o.owner("VIEW", vname, v.Owner))
			s.add(o.comment("VIEW "+vname, v.Comment))
			for _, c := range p.columns {
				s.add(o.comment("COLUMN "+vname+"."+pgsql.QuoteIdent(c.Name), c.Comment))
			}
			for _, t := range p.triggers {
				s.add(o.comment("TRIGGER "+pgsql.QuoteIdent(t.Name)+" ON "+vname, t.Comment))
			}
			s.add(o.privileges("TABLE "+vname, SecurableTable, v.Owner, v.Privileges, v.PrivilegesDefault, sc.db.server.version)...)
			if o.IncludePrivileges {
				s.add(columnACLStatements(vname, p.columns)...)
			}
			return nil
		})
	})
}

type materializedViewParts struct {
	view    *MaterializedView
	columns []*Column
	indexes []*Index
}

// ScriptMaterializedView scripts the materialized view schema.name: CREATE
// MATERIALIZED VIEW with the server's rendering of its query, WITH DATA when
// it is populated and WITH NO DATA when not, its indexes, owner, comments
// and privileges. ScriptAlter falls back to CREATE: PostgreSQL has no CREATE
// OR REPLACE MATERIALIZED VIEW.
func (sc *Scripter) ScriptMaterializedView(ctx context.Context, schema, name string) (string, error) {
	op := "script materialized view " + pgsql.QualifiedName(schema, name)
	return scriptOf(ctx, sc.in(schema), func(ctx context.Context) (*materializedViewParts, error) {
		m, err := sc.db.MaterializedViewByName(ctx, schema, name)
		if err != nil {
			return nil, err
		}
		p := &materializedViewParts{view: m}
		if p.columns, err = m.Columns(ctx); err != nil {
			return nil, err
		}
		if p.indexes, err = m.Indexes(ctx); err != nil {
			return nil, err
		}
		return p, nil
	}, func(p *materializedViewParts) (string, error) {
		o, m := sc.opts, p.view
		mname := o.name(m.Schema, m.Name)
		if s, ok := o.member("materialized view "+mname, m.Origin); ok {
			return s, nil
		}
		return o.script(op, o.drop("MATERIALIZED VIEW", mname), func(s *scriptText) error {
			opts, err := storageParameters(m.Options)
			if err != nil {
				return wrapScript(op, err)
			}
			stmt := "CREATE MATERIALIZED VIEW" + o.ifNotExists() + " " + mname
			if opts != "" {
				stmt += " WITH " + opts
			}
			if m.Tablespace != "" {
				stmt += " TABLESPACE " + pgsql.QuoteIdent(m.Tablespace)
			}
			stmt += " AS\n" + viewQuery(m.Definition)
			if m.Populated {
				stmt += "\nWITH DATA"
			} else {
				stmt += "\nWITH NO DATA"
			}
			s.add(stmt)
			s.add(sc.indexStatements(p.indexes, mname, false)...)
			s.add(o.owner("MATERIALIZED VIEW", mname, m.Owner))
			s.add(o.comment("MATERIALIZED VIEW "+mname, m.Comment))
			for _, c := range p.columns {
				s.add(o.comment("COLUMN "+mname+"."+pgsql.QuoteIdent(c.Name), c.Comment))
			}
			for _, i := range p.indexes {
				s.add(o.comment("INDEX "+o.name(m.Schema, i.Name), i.Comment))
			}
			s.add(o.privileges("TABLE "+mname, SecurableTable, m.Owner, m.Privileges, m.PrivilegesDefault, sc.db.server.version)...)
			if o.IncludePrivileges {
				s.add(columnACLStatements(mname, p.columns)...)
			}
			return nil
		})
	})
}

// ------------------------------------------------------------
// Foreign tables
// ------------------------------------------------------------

// genericOptions is an FDW OPTIONS list's body in parentheses: each name an
// identifier, each value a literal.
func genericOptions(params []ParameterSetting) string {
	parts := make([]string, len(params))
	for i, p := range params {
		parts[i] = pgsql.QuoteIdent(p.Name) + " " + pgsql.QuoteLiteral(p.Value)
	}
	return "(" + strings.Join(parts, ", ") + ")"
}

type foreignTableParts struct {
	table       *ForeignTable
	columns     []*Column
	constraints []*Constraint
	triggers    []*Trigger
}

// ScriptForeignTable scripts the foreign table schema.name: CREATE FOREIGN
// TABLE with its columns (and their OPTIONS), constraints, SERVER and
// OPTIONS — or PARTITION OF its parent — then triggers, owner, comments and
// privileges.
func (sc *Scripter) ScriptForeignTable(ctx context.Context, schema, name string) (string, error) {
	op := "script foreign table " + pgsql.QualifiedName(schema, name)
	return scriptOf(ctx, sc.in(schema), func(ctx context.Context) (*foreignTableParts, error) {
		f, err := sc.db.ForeignTableByName(ctx, schema, name)
		if err != nil {
			return nil, err
		}
		p := &foreignTableParts{table: f}
		if p.columns, err = f.Columns(ctx); err != nil {
			return nil, err
		}
		if p.constraints, err = f.Constraints(ctx); err != nil {
			return nil, err
		}
		if p.triggers, err = f.Triggers(ctx); err != nil {
			return nil, err
		}
		return p, nil
	}, func(p *foreignTableParts) (string, error) {
		o, f := sc.opts, p.table
		fname := o.name(f.Schema, f.Name)
		if s, ok := o.member("foreign table "+fname, f.Origin); ok {
			return s, nil
		}
		return o.script(op, o.drop("FOREIGN TABLE", fname), func(s *scriptText) error {
			partition := f.IsPartition && len(f.Parents) == 1
			var elems []string
			if !partition {
				for _, c := range p.columns {
					if !c.IsLocal {
						continue
					}
					def, err := columnDefinition(c, int(sc.db.server.version))
					if err != nil {
						return wrapScript(op, err)
					}
					elems = append(elems, def)
				}
			}
			var later []string
			for _, c := range p.constraints {
				switch {
				case inlineConstraint(c):
					elems = append(elems, "CONSTRAINT "+pgsql.QuoteIdent(c.Name)+" "+c.Definition)
				case c.IsLocal && c.Kind != ConstraintNotNull:
					later = append(later, "ALTER FOREIGN TABLE "+fname+" ADD CONSTRAINT "+pgsql.QuoteIdent(c.Name)+" "+c.Definition)
				}
			}
			var b strings.Builder
			b.WriteString("CREATE FOREIGN TABLE" + o.ifNotExists() + " " + fname)
			body := ""
			if len(elems) > 0 {
				body = "\n    " + strings.Join(elems, ",\n    ") + "\n"
			}
			if partition {
				b.WriteString(" PARTITION OF " + f.Parents[0].QualifiedName())
				if body != "" {
					b.WriteString(" (" + body + ")")
				}
				b.WriteString("\n" + f.PartitionBound)
			} else {
				b.WriteString(" (" + body + ")")
				if len(f.Parents) > 0 {
					parents := make([]string, len(f.Parents))
					for i, par := range f.Parents {
						parents[i] = par.QualifiedName()
					}
					b.WriteString("\nINHERITS (" + strings.Join(parents, ", ") + ")")
				}
			}
			b.WriteString("\nSERVER " + pgsql.QuoteIdent(f.Server))
			if len(f.Options) > 0 {
				b.WriteString("\nOPTIONS " + genericOptions(f.Options))
			}
			s.add(b.String())
			s.add(later...)
			for _, t := range p.triggers {
				if !t.Inherited {
					s.add(sc.triggerStatements(t, fname, false)...)
				}
			}
			s.add(o.owner("FOREIGN TABLE", fname, f.Owner))
			s.add(o.comment("FOREIGN TABLE "+fname, f.Comment))
			for _, c := range p.columns {
				s.add(o.comment("COLUMN "+fname+"."+pgsql.QuoteIdent(c.Name), c.Comment))
			}
			for _, c := range p.constraints {
				if c.IsLocal {
					s.add(o.comment("CONSTRAINT "+pgsql.QuoteIdent(c.Name)+" ON "+fname, c.Comment))
				}
			}
			for _, t := range p.triggers {
				if !t.Inherited {
					s.add(o.comment("TRIGGER "+pgsql.QuoteIdent(t.Name)+" ON "+fname, t.Comment))
				}
			}
			s.add(o.privileges("TABLE "+fname, SecurableTable, f.Owner, f.Privileges, f.PrivilegesDefault, sc.db.server.version)...)
			if o.IncludePrivileges {
				s.add(columnACLStatements(fname, p.columns)...)
			}
			return nil
		})
	})
}
