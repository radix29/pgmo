package pgmo

import (
	"context"
	"strconv"
	"strings"

	"github.com/radix29/pgmo/pgsql"
)

// ============================================================
// Scripting sequences, types and domains
// ============================================================
//
// None has a server function rendering its CREATE: each is built from the
// catalog fields. A domain's default and constraints are the server's text
// (pg_get_expr, pg_get_constraintdef).

// ScriptSequence scripts the sequence schema.name: CREATE SEQUENCE with
// every option stated, OWNED BY (as ALTER SEQUENCE, since the owning
// table's script may come after it), owner, comment and privileges. Its
// current value is data, not DDL, and is not restated. An identity
// column's sequence is its column's: a note saying so.
func (sc *Scripter) ScriptSequence(ctx context.Context, schema, name string) (string, error) {
	op := "script sequence " + pgsql.QualifiedName(schema, name)
	return scriptOf(ctx, sc.in(schema), func(ctx context.Context) (*Sequence, error) {
		return sc.db.SequenceByName(ctx, schema, name)
	}, func(q *Sequence) (string, error) {
		o := sc.opts
		qname := o.name(q.Schema, q.Name)
		if s, ok := o.member("sequence "+qname, q.Origin); ok {
			return s, nil
		}
		if q.Identity {
			var s scriptText
			s.notef("sequence %s is the identity sequence of %s.%s: created and dropped with its column",
				qname, q.OwnedBy.QualifiedName(), pgsql.QuoteIdent(q.OwnedByColumn))
			return s.String(), nil
		}
		return o.script(op, o.drop("SEQUENCE", qname), func(s *scriptText) error {
			var b strings.Builder
			b.WriteString("CREATE ")
			switch q.Persistence {
			case PersistenceUnlogged:
				b.WriteString("UNLOGGED ")
			case PersistenceTemporary:
				b.WriteString("TEMPORARY ")
				qname = pgsql.QuoteIdent(q.Name)
			}
			b.WriteString("SEQUENCE" + o.ifNotExists() + " " + qname)
			if q.DataType != "" {
				b.WriteString("\n    AS " + q.DataType)
			}
			b.WriteString("\n    START WITH " + strconv.FormatInt(q.Start, 10))
			b.WriteString("\n    INCREMENT BY " + strconv.FormatInt(q.Increment, 10))
			b.WriteString("\n    MINVALUE " + strconv.FormatInt(q.MinValue, 10))
			b.WriteString("\n    MAXVALUE " + strconv.FormatInt(q.MaxValue, 10))
			b.WriteString("\n    CACHE " + strconv.FormatInt(q.Cache, 10))
			if q.Cycle {
				b.WriteString("\n    CYCLE")
			} else {
				b.WriteString("\n    NO CYCLE")
			}
			s.add(b.String())
			if q.OwnedBy.Name != "" {
				s.add("ALTER SEQUENCE " + qname + " OWNED BY " + q.OwnedBy.QualifiedName() + "." + pgsql.QuoteIdent(q.OwnedByColumn))
			}
			s.add(o.owner("SEQUENCE", qname, q.Owner))
			s.add(o.comment("SEQUENCE "+qname, q.Comment))
			s.add(o.privileges("SEQUENCE "+qname, SecurableSequence, q.Owner, q.Privileges, q.PrivilegesDefault, sc.db.server.version)...)
			return nil
		})
	})
}

// ScriptType scripts the type schema.name — an enum, composite, range or
// base type, or a shell — as CREATE TYPE from its fields, then owner,
// comment and privileges. A multirange is its range's: a note saying so.
// A pseudo-type (pg_catalog's own) has no CREATE: ErrInvalidRequest.
func (sc *Scripter) ScriptType(ctx context.Context, schema, name string) (string, error) {
	op := "script type " + pgsql.QualifiedName(schema, name)
	return scriptOf(ctx, sc.in(schema), func(ctx context.Context) (*Type, error) {
		return sc.db.TypeByName(ctx, schema, name)
	}, func(t *Type) (string, error) {
		o := sc.opts
		tname := o.name(t.Schema, t.Name)
		if s, ok := o.member("type "+tname, t.Origin); ok {
			return s, nil
		}
		switch {
		case t.Kind == TypeMultirange:
			var s scriptText
			s.notef("type %s is the multirange of range %s: created and dropped with it", tname, t.Range.QualifiedName())
			return s.String(), nil
		case t.Kind == TypePseudo && !t.Shell:
			return "", invalidf("pgmo: %s: a pseudo-type has no CREATE TYPE", op)
		}
		return o.script(op, o.drop("TYPE", tname), func(s *scriptText) error {
			create, err := typeCreate(t, tname)
			if err != nil {
				return wrapScript(op, err)
			}
			s.add(create)
			s.add(o.owner("TYPE", tname, t.Owner))
			s.add(o.comment("TYPE "+tname, t.Comment))
			s.add(o.privileges("TYPE "+tname, SecurableType, t.Owner, t.Privileges, t.PrivilegesDefault, sc.db.server.version)...)
			return nil
		})
	})
}

// typeCreate is the type's CREATE TYPE under the name tname.
func typeCreate(t *Type, tname string) (string, error) {
	switch {
	case t.Shell:
		return "CREATE TYPE " + tname, nil
	case t.Kind == TypeEnum:
		labels := make([]string, len(t.Labels))
		for i, l := range t.Labels {
			labels[i] = pgsql.QuoteLiteral(l)
		}
		if len(labels) == 0 {
			return "CREATE TYPE " + tname + " AS ENUM ()", nil
		}
		return "CREATE TYPE " + tname + " AS ENUM (\n    " + strings.Join(labels, ",\n    ") + "\n)", nil
	case t.Kind == TypeComposite:
		attrs := make([]string, len(t.Attributes))
		for i, a := range t.Attributes {
			attrs[i] = pgsql.QuoteIdent(a.Name) + " " + a.Type
			if a.Collation.Name != "" {
				attrs[i] += " COLLATE " + a.Collation.QualifiedName()
			}
		}
		if len(attrs) == 0 {
			return "CREATE TYPE " + tname + " AS ()", nil
		}
		return "CREATE TYPE " + tname + " AS (\n    " + strings.Join(attrs, ",\n    ") + "\n)", nil
	case t.Kind == TypeRange:
		opts := []string{"SUBTYPE = " + t.Subtype}
		if t.SubtypeOperatorClass.Name != "" {
			opts = append(opts, "SUBTYPE_OPCLASS = "+t.SubtypeOperatorClass.QualifiedName())
		}
		if t.Collation.Name != "" {
			opts = append(opts, "COLLATION = "+t.Collation.QualifiedName())
		}
		if t.Canonical.Name != "" {
			opts = append(opts, "CANONICAL = "+t.Canonical.QualifiedName())
		}
		if t.SubtypeDiff.Name != "" {
			opts = append(opts, "SUBTYPE_DIFF = "+t.SubtypeDiff.QualifiedName())
		}
		if t.Multirange.Name != "" {
			opts = append(opts, "MULTIRANGE_TYPE_NAME = "+t.Multirange.QualifiedName())
		}
		return "CREATE TYPE " + tname + " AS RANGE (\n    " + strings.Join(opts, ",\n    ") + "\n)", nil
	case t.Kind == TypeBase:
		return baseTypeCreate(t, tname), nil
	}
	return "", invalidf("unknown type kind %q", string(t.Kind))
}

// baseTypeCreate is a base type's CREATE TYPE (INPUT = …, OUTPUT = …): every
// property stated, as pg_dump writes it.
func baseTypeCreate(t *Type, tname string) string {
	opts := []string{"INPUT = " + t.Input.QualifiedName(), "OUTPUT = " + t.Output.QualifiedName()}
	for _, f := range []struct {
		key string
		fn  ObjectName
	}{{"RECEIVE", t.Receive}, {"SEND", t.Send}, {"TYPMOD_IN", t.TypmodInput}, {"TYPMOD_OUT", t.TypmodOutput},
		{"ANALYZE", t.Analyze}, {"SUBSCRIPT", t.Subscript}} {
		if f.fn.Name != "" {
			opts = append(opts, f.key+" = "+f.fn.QualifiedName())
		}
	}
	switch {
	case t.InternalLength == -1:
		opts = append(opts, "INTERNALLENGTH = VARIABLE")
	case t.InternalLength > 0:
		opts = append(opts, "INTERNALLENGTH = "+strconv.Itoa(t.InternalLength))
	}
	if t.PassedByValue {
		opts = append(opts, "PASSEDBYVALUE")
	}
	if t.Alignment != "" {
		opts = append(opts, "ALIGNMENT = "+t.Alignment.String())
	}
	if t.Storage != "" {
		opts = append(opts, "STORAGE = "+t.Storage.String())
	}
	if t.Category != "" {
		opts = append(opts, "CATEGORY = "+pgsql.QuoteLiteral(string(t.Category)))
	}
	if t.Preferred {
		opts = append(opts, "PREFERRED = true")
	}
	if t.HasDefault {
		opts = append(opts, "DEFAULT = "+pgsql.QuoteLiteral(t.Default))
	}
	if t.Element != "" {
		opts = append(opts, "ELEMENT = "+t.Element)
	}
	if t.Delimiter != "" && t.Delimiter != "," {
		opts = append(opts, "DELIMITER = "+pgsql.QuoteLiteral(t.Delimiter))
	}
	if t.Collatable {
		opts = append(opts, "COLLATABLE = true")
	}
	return "CREATE TYPE " + tname + " (\n    " + strings.Join(opts, ",\n    ") + "\n)"
}

// ScriptDomain scripts the domain schema.name: CREATE DOMAIN with its base
// type, collation, default, NOT NULL and validated constraints (the
// server's definitions), any constraint not yet validated as ALTER DOMAIN
// … ADD CONSTRAINT … NOT VALID, owner, comment and privileges.
func (sc *Scripter) ScriptDomain(ctx context.Context, schema, name string) (string, error) {
	op := "script domain " + pgsql.QualifiedName(schema, name)
	return scriptOf(ctx, sc.in(schema), func(ctx context.Context) (*Domain, error) {
		return sc.db.DomainByName(ctx, schema, name)
	}, func(m *Domain) (string, error) {
		o := sc.opts
		dname := o.name(m.Schema, m.Name)
		if s, ok := o.member("domain "+dname, m.Origin); ok {
			return s, nil
		}
		return o.script(op, o.drop("DOMAIN", dname), func(s *scriptText) error {
			var b strings.Builder
			b.WriteString("CREATE DOMAIN " + dname + " AS " + m.BaseType)
			if m.Collation.Name != "" {
				b.WriteString("\n    COLLATE " + m.Collation.QualifiedName())
			}
			if m.Default != "" {
				b.WriteString("\n    DEFAULT " + m.Default)
			}
			if m.NotNull {
				b.WriteString("\n    NOT NULL")
			}
			var later []string
			for _, c := range m.Constraints {
				switch {
				case c.Kind == ConstraintNotNull:
					// 17+ keeps NOT NULL as a constraint row too; NotNull writes it.
				case c.Validated:
					b.WriteString("\n    CONSTRAINT " + pgsql.QuoteIdent(c.Name) + " " + c.Definition)
				default:
					later = append(later, "ALTER DOMAIN "+dname+" ADD CONSTRAINT "+pgsql.QuoteIdent(c.Name)+" "+c.Definition)
				}
			}
			s.add(b.String())
			s.add(later...)
			s.add(o.owner("DOMAIN", dname, m.Owner))
			s.add(o.comment("DOMAIN "+dname, m.Comment))
			s.add(o.privileges("DOMAIN "+dname, SecurableDomain, m.Owner, m.Privileges, m.PrivilegesDefault, sc.db.server.version)...)
			return nil
		})
	})
}
