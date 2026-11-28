package pgmo

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/radix29/pgmo/pgsql"
)

// ============================================================
// Type and domain writes
// ============================================================
//
// type_write.go is P4.13.3's types and domains: CREATE TYPE … AS ENUM, AS
// (…) and AS RANGE, ALTER TYPE … ADD VALUE / RENAME VALUE, CREATE DOMAIN,
// ALTER DOMAIN's default, NOT NULL and constraints, and for both DROP
// (CASCADE opt-in), RENAME TO, SET SCHEMA, OWNER TO and COMMENT ON. A base
// type (CREATE TYPE … (INPUT = …), C functions) is not written: it is an
// extension's business.

// CreateEnumTypeRequest describes a new enum type.
type CreateEnumTypeRequest struct {
	Schema, Name string
	// Labels are its values, in sort order; none is an enum with no values,
	// which PostgreSQL allows.
	Labels []string
}

// CreateEnumType creates the enum type req describes (CREATE TYPE … AS
// ENUM), and returns it read back from the catalog — or, under
// Scripting(ctx), the TypeRef handle.
func (d *Database) CreateEnumType(ctx context.Context, req CreateEnumTypeRequest) (*Type, error) {
	labels := make([]string, len(req.Labels))
	for i, l := range req.Labels {
		labels[i] = pgsql.QuoteLiteral(l)
	}
	return d.createType(ctx, req.Schema, req.Name, func() (string, error) {
		return "ENUM (" + strings.Join(labels, ", ") + ")", nil
	})
}

// TypeAttributeSpec is one attribute of a composite type.
type TypeAttributeSpec struct {
	Name string
	// Type is the attribute's type as SQL, written as given.
	Type string
	// Collation is COLLATE; zero for the type's.
	Collation ObjectName
}

// CreateCompositeTypeRequest describes a new composite type.
type CreateCompositeTypeRequest struct {
	Schema, Name string
	Attributes   []TypeAttributeSpec
}

// CreateCompositeType creates the composite type req describes (CREATE
// TYPE … AS (…)), and returns it read back from the catalog — or, under
// Scripting(ctx), the TypeRef handle.
func (d *Database) CreateCompositeType(ctx context.Context, req CreateCompositeTypeRequest) (*Type, error) {
	return d.createType(ctx, req.Schema, req.Name, func() (string, error) {
		attrs := make([]string, len(req.Attributes))
		for i, a := range req.Attributes {
			if a.Name == "" || a.Type == "" {
				return "", invalidf("attribute %d: a name and a type are required", i+1)
			}
			attrs[i] = pgsql.QuoteIdent(a.Name) + " " + a.Type
			if a.Collation.Name != "" {
				attrs[i] += " COLLATE " + a.Collation.QualifiedName()
			}
		}
		return "(" + strings.Join(attrs, ", ") + ")", nil
	})
}

// CreateRangeTypeRequest describes a new range type; the server creates its
// multirange type with it.
type CreateRangeTypeRequest struct {
	Schema, Name string

	// Subtype is SUBTYPE: the type of the bounds, as SQL, written as given.
	Subtype string

	// SubtypeOperatorClass is SUBTYPE_OPCLASS: the btree operator class
	// ordering the bounds; zero for the subtype's default.
	SubtypeOperatorClass ObjectName

	// Collation is COLLATION, for a collatable subtype; zero for its
	// default.
	Collation ObjectName

	// Canonical and SubtypeDiff are CANONICAL and SUBTYPE_DIFF functions;
	// zero for none.
	Canonical   ObjectName
	SubtypeDiff ObjectName

	// Multirange is MULTIRANGE_TYPE_NAME; zero lets the server name it
	// (the range's name with "multirange" for "range", or "_multirange"
	// appended).
	Multirange ObjectName
}

// CreateRangeType creates the range type req describes (CREATE TYPE … AS
// RANGE), and returns it read back from the catalog — or, under
// Scripting(ctx), the TypeRef handle.
func (d *Database) CreateRangeType(ctx context.Context, req CreateRangeTypeRequest) (*Type, error) {
	return d.createType(ctx, req.Schema, req.Name, func() (string, error) {
		if req.Subtype == "" {
			return "", invalidf("no subtype")
		}
		opts := []string{"SUBTYPE = " + req.Subtype}
		named := func(option string, o ObjectName) {
			if o.Name != "" {
				opts = append(opts, option+" = "+o.QualifiedName())
			}
		}
		named("SUBTYPE_OPCLASS", req.SubtypeOperatorClass)
		named("COLLATION", req.Collation)
		named("CANONICAL", req.Canonical)
		named("SUBTYPE_DIFF", req.SubtypeDiff)
		named("MULTIRANGE_TYPE_NAME", req.Multirange)
		return "RANGE (" + strings.Join(opts, ", ") + ")", nil
	})
}

// createType runs CREATE TYPE schema.name AS <body()>, and reads the type
// back.
func (d *Database) createType(ctx context.Context, schema, name string, body func() (string, error)) (*Type, error) {
	if err := requireQualified("create type", schema, name); err != nil {
		return nil, err
	}
	op := "create type " + pgsql.QualifiedName(schema, name)
	b, err := body()
	if err != nil {
		return nil, fmt.Errorf("pgmo: %s: %w", op, err)
	}
	if err := d.exec(ctx, "CREATE TYPE "+pgsql.QualifiedName(schema, name)+" AS "+b); err != nil {
		return nil, fmt.Errorf("pgmo: %s: %w", op, err)
	}
	return createdObject(ctx, d.TypeRef(schema, name), func() (*Type, error) {
		return d.TypeByName(ctx, schema, name)
	})
}

func (t *Type) ddl(verb string) (ddlObject, error) {
	return t.db.qualified(verb, "TYPE", t.Schema, t.Name)
}

// EnumValuePosition places a new enum label: before or after an existing
// one; the zero EnumValuePosition is last.
type EnumValuePosition struct {
	Before string
	After  string
}

// AddEnumValue adds label to the enum type (ALTER TYPE … ADD VALUE), last
// or where pos puts it, mirrored onto Labels (not under WithScript). It may
// run inside InTransaction, but the new label cannot be used there until the
// transaction commits (55P04).
func (t *Type) AddEnumValue(ctx context.Context, label string, pos EnumValuePosition) error {
	o, err := t.ddl("add value to")
	if err != nil {
		return err
	}
	stmt := "ALTER TYPE " + o.name + " ADD VALUE " + pgsql.QuoteLiteral(label)
	switch {
	case pos.Before != "" && pos.After != "":
		return invalidf("pgmo: %s: both BEFORE and AFTER", o.op("add value to"))
	case pos.Before != "":
		stmt += " BEFORE " + pgsql.QuoteLiteral(pos.Before)
	case pos.After != "":
		stmt += " AFTER " + pgsql.QuoteLiteral(pos.After)
	}
	if err := o.do(ctx, "add value to", stmt); err != nil {
		return err
	}
	if Scripting(ctx) {
		return nil
	}
	at := len(t.Labels)
	if i := slices.Index(t.Labels, pos.Before); pos.Before != "" && i >= 0 {
		at = i
	} else if i := slices.Index(t.Labels, pos.After); pos.After != "" && i >= 0 {
		at = i + 1
	}
	t.Labels = slices.Insert(t.Labels, at, label)
	return nil
}

// RenameEnumValue renames the enum type's label to newLabel (ALTER TYPE …
// RENAME VALUE), mirrored onto Labels (not under WithScript).
func (t *Type) RenameEnumValue(ctx context.Context, label, newLabel string) error {
	o, err := t.ddl("rename value of")
	if err != nil {
		return err
	}
	if err := o.do(ctx, "rename value of", "ALTER TYPE "+o.name+" RENAME VALUE "+pgsql.QuoteLiteral(label)+
		" TO "+pgsql.QuoteLiteral(newLabel)); err != nil {
		return err
	}
	if i := slices.Index(t.Labels, label); i >= 0 && !Scripting(ctx) {
		t.Labels[i] = newLabel
	}
	return nil
}

// Drop drops the type (DROP TYPE), with CASCADE when opts.Cascade — which
// drops the columns, functions and other types using it. A range's
// multirange goes with it.
func (t *Type) Drop(ctx context.Context, opts DropOptions) error {
	return dropObject(ctx, t.ddl, opts)
}

// Rename renames the type (ALTER TYPE … RENAME TO), mirrored onto Name (not
// under WithScript). Its array type is renamed with it.
func (t *Type) Rename(ctx context.Context, newName string) error {
	return renameObject(ctx, t.ddl, newName, &t.Name)
}

// SetSchema moves the type to schema, mirrored onto Schema (not under
// WithScript).
func (t *Type) SetSchema(ctx context.Context, schema string) error {
	return moveObject(ctx, t.ddl, schema, &t.Schema)
}

// SetOwner makes role the type's owner, mirrored onto Owner (not under
// WithScript).
func (t *Type) SetOwner(ctx context.Context, role string) error {
	return ownObject(ctx, t.ddl, role, &t.Owner)
}

// SetComment sets the type's comment, "" removing it, mirrored onto Comment
// (not under WithScript).
func (t *Type) SetComment(ctx context.Context, text string) error {
	return commentObject(ctx, t.ddl, text, &t.Comment)
}

// ------------------------------------------------------------
// Domains
// ------------------------------------------------------------

// DomainConstraintSpec is one CHECK constraint of a domain.
type DomainConstraintSpec struct {
	// Name is CONSTRAINT's name; "" lets the server pick one.
	Name string
	// Check is the expression as SQL, VALUE standing for the value.
	Check string
	// NotValid is NOT VALID: existing columns of the domain are not
	// checked. AddConstraint only.
	NotValid bool
}

// definition is the constraint as CREATE DOMAIN and ALTER DOMAIN … ADD
// write it.
func (c DomainConstraintSpec) definition() (string, error) {
	if c.Check == "" {
		return "", invalidf("a CHECK constraint with no expression")
	}
	def := "CHECK (" + c.Check + ")"
	if c.Name != "" {
		def = "CONSTRAINT " + pgsql.QuoteIdent(c.Name) + " " + def
	}
	if c.NotValid {
		def += " NOT VALID"
	}
	return def, nil
}

// CreateDomainRequest describes a new domain.
type CreateDomainRequest struct {
	Schema, Name string

	// BaseType is the underlying type as SQL, written as given.
	BaseType string

	// Collation is COLLATE; zero for the base type's.
	Collation ObjectName

	// Default is DEFAULT's expression as SQL; "" for none.
	Default string

	// NotNull is NOT NULL.
	NotNull bool

	// Constraints are its CHECK constraints.
	Constraints []DomainConstraintSpec
}

// CreateDomain creates the domain req describes, and returns it read back
// from the catalog — or, under Scripting(ctx), the DomainRef handle.
func (d *Database) CreateDomain(ctx context.Context, req CreateDomainRequest) (*Domain, error) {
	if err := requireQualified("create domain", req.Schema, req.Name); err != nil {
		return nil, err
	}
	op := "create domain " + pgsql.QualifiedName(req.Schema, req.Name)
	if req.BaseType == "" {
		return nil, invalidf("pgmo: %s: no base type", op)
	}
	stmt := "CREATE DOMAIN " + pgsql.QualifiedName(req.Schema, req.Name) + " AS " + req.BaseType
	if req.Collation.Name != "" {
		stmt += " COLLATE " + req.Collation.QualifiedName()
	}
	if req.Default != "" {
		stmt += " DEFAULT " + req.Default
	}
	if req.NotNull {
		stmt += " NOT NULL"
	}
	for _, c := range req.Constraints {
		if c.NotValid {
			return nil, invalidf("pgmo: %s: NOT VALID in CREATE DOMAIN; add the constraint with AddConstraint", op)
		}
		def, err := c.definition()
		if err != nil {
			return nil, fmt.Errorf("pgmo: %s: %w", op, err)
		}
		stmt += " " + def
	}
	if err := d.exec(ctx, stmt); err != nil {
		return nil, fmt.Errorf("pgmo: %s: %w", op, err)
	}
	return createdObject(ctx, d.DomainRef(req.Schema, req.Name), func() (*Domain, error) {
		return d.DomainByName(ctx, req.Schema, req.Name)
	})
}

func (m *Domain) ddl(verb string) (ddlObject, error) {
	return m.db.qualified(verb, "DOMAIN", m.Schema, m.Name)
}

// alter runs ALTER DOMAIN m action as the operation verb.
func (m *Domain) alter(ctx context.Context, verb, action string) error {
	o, err := m.ddl(verb)
	if err != nil {
		return err
	}
	return o.do(ctx, verb, "ALTER DOMAIN "+o.name+" "+action)
}

// SetDefault sets the domain's default (ALTER DOMAIN … SET DEFAULT), an
// expression as SQL; "" is DROP DEFAULT. Mirrored onto Default (not under
// WithScript) as given — the server stores its own rendering.
func (m *Domain) SetDefault(ctx context.Context, expr string) error {
	action := "DROP DEFAULT"
	if expr != "" {
		action = "SET DEFAULT " + expr
	}
	if err := m.alter(ctx, "set default of", action); err != nil {
		return err
	}
	setIfApplied(ctx, &m.Default, expr)
	return nil
}

// SetNotNull is ALTER DOMAIN … SET NOT NULL (true; every column of the
// domain is checked) or DROP NOT NULL, mirrored onto NotNull (not under
// WithScript).
func (m *Domain) SetNotNull(ctx context.Context, notNull bool) error {
	action := "DROP NOT NULL"
	if notNull {
		action = "SET NOT NULL"
	}
	if err := m.alter(ctx, "set not null of", action); err != nil {
		return err
	}
	setIfApplied(ctx, &m.NotNull, notNull)
	return nil
}

// AddConstraint adds the CHECK constraint c to the domain (ALTER DOMAIN …
// ADD). Constraints is not mirrored: read the domain again.
func (m *Domain) AddConstraint(ctx context.Context, c DomainConstraintSpec) error {
	o, err := m.ddl("add constraint to")
	if err != nil {
		return err
	}
	def, err := c.definition()
	if err != nil {
		return fmt.Errorf("pgmo: %s: %w", o.op("add constraint to"), err)
	}
	return o.do(ctx, "add constraint to", "ALTER DOMAIN "+o.name+" ADD "+def)
}

// DropConstraint drops the domain's constraint name (ALTER DOMAIN … DROP
// CONSTRAINT), with CASCADE when opts.Cascade.
func (m *Domain) DropConstraint(ctx context.Context, name string, opts DropOptions) error {
	if name == "" {
		return invalidf("pgmo: drop domain constraint: no name")
	}
	return m.alter(ctx, "drop constraint "+pgsql.QuoteIdent(name)+" of", "DROP CONSTRAINT "+pgsql.QuoteIdent(name)+opts.clause())
}

// ValidateConstraint checks the domain's columns against its NOT VALID
// constraint name, and marks it validated (ALTER DOMAIN … VALIDATE
// CONSTRAINT).
func (m *Domain) ValidateConstraint(ctx context.Context, name string) error {
	if name == "" {
		return invalidf("pgmo: validate domain constraint: no name")
	}
	return m.alter(ctx, "validate constraint "+pgsql.QuoteIdent(name)+" of", "VALIDATE CONSTRAINT "+pgsql.QuoteIdent(name))
}

// Drop drops the domain (DROP DOMAIN), with CASCADE when opts.Cascade —
// which drops the columns of the domain.
func (m *Domain) Drop(ctx context.Context, opts DropOptions) error {
	return dropObject(ctx, m.ddl, opts)
}

// Rename renames the domain, mirrored onto Name (not under WithScript).
func (m *Domain) Rename(ctx context.Context, newName string) error {
	return renameObject(ctx, m.ddl, newName, &m.Name)
}

// SetSchema moves the domain to schema, mirrored onto Schema (not under
// WithScript).
func (m *Domain) SetSchema(ctx context.Context, schema string) error {
	return moveObject(ctx, m.ddl, schema, &m.Schema)
}

// SetOwner makes role the domain's owner, mirrored onto Owner (not under
// WithScript).
func (m *Domain) SetOwner(ctx context.Context, role string) error {
	return ownObject(ctx, m.ddl, role, &m.Owner)
}

// SetComment sets the domain's comment, "" removing it, mirrored onto
// Comment (not under WithScript).
func (m *Domain) SetComment(ctx context.Context, text string) error {
	return commentObject(ctx, m.ddl, text, &m.Comment)
}
