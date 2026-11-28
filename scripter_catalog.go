package pgmo

import (
	"context"
	"strings"

	"github.com/radix29/pgmo/pgsql"
)

// ============================================================
// Scripting schemas, extensions, event triggers, collations, text search
// objects, languages, casts, operators and foreign data
// ============================================================
//
// None has a server function rendering its CREATE: each is built from the
// catalog fields.

// ScriptSchema scripts the schema name: CREATE SCHEMA, owner, comment,
// privileges, and the default privileges set in it (ALTER DEFAULT
// PRIVILEGES … IN SCHEMA). The objects in it are scripted on their own.
func (sc *Scripter) ScriptSchema(ctx context.Context, name string) (string, error) {
	op := "script schema " + pgsql.QuoteIdent(name)
	return scriptOf(ctx, sc.in(""), func(ctx context.Context) (*Schema, error) {
		return sc.db.SchemaByName(ctx, name)
	}, func(s *Schema) (string, error) {
		o := sc.opts
		sname := pgsql.QuoteIdent(s.Name)
		if out, ok := o.member("schema "+sname, s.Origin); ok {
			return out, nil
		}
		return o.script(op, o.drop("SCHEMA", sname), func(t *scriptText) error {
			t.add("CREATE SCHEMA" + o.ifNotExists() + " " + sname)
			t.add(o.owner("SCHEMA", sname, s.Owner))
			t.add(o.comment("SCHEMA "+sname, s.Comment))
			t.add(o.privileges("SCHEMA "+sname, SecurableSchema, s.Owner, s.Privileges, s.PrivilegesDefault, sc.db.server.version)...)
			if o.IncludePrivileges {
				stmts, err := defaultPrivilegeStatements(s.DefaultPrivileges, sc.db.server.version)
				if err != nil {
					return wrapScript(op, err)
				}
				t.add(stmts...)
			}
			return nil
		})
	})
}

// ScriptExtension scripts the extension name: CREATE EXTENSION with the
// schema and version it is installed at, and its comment. Its members are
// the extension's script's, and their DDL is never written. An extension
// has no OWNER TO: the role running CREATE EXTENSION owns it.
func (sc *Scripter) ScriptExtension(ctx context.Context, name string) (string, error) {
	op := "script extension " + pgsql.QuoteIdent(name)
	return scriptOf(ctx, sc.in(""), func(ctx context.Context) (*Extension, error) {
		return sc.db.ExtensionByName(ctx, name)
	}, func(e *Extension) (string, error) {
		o := sc.opts
		ename := pgsql.QuoteIdent(e.Name)
		return o.script(op, o.drop("EXTENSION", ename), func(s *scriptText) error {
			stmt := "CREATE EXTENSION" + o.ifNotExists() + " " + ename
			if e.Schema != "" {
				stmt += " WITH SCHEMA " + pgsql.QuoteIdent(e.Schema)
			}
			if e.Version != "" {
				stmt += " VERSION " + pgsql.QuoteLiteral(e.Version)
			}
			s.add(stmt)
			s.add(o.comment("EXTENSION "+ename, e.Comment))
			return nil
		})
	})
}

// enableStatement is the ENABLE or DISABLE restating a trigger state other
// than the default, on the statement prefix alter ("ALTER EVENT TRIGGER
// x"); "" for the default.
func enableStatement(alter string, e TriggerEnabled) string {
	switch e {
	case TriggerDisabled:
		return alter + " DISABLE"
	case TriggerEnabledReplica:
		return alter + " ENABLE REPLICA"
	case TriggerEnabledAlways:
		return alter + " ENABLE ALWAYS"
	}
	return ""
}

// ScriptEventTrigger scripts the event trigger name: CREATE EVENT TRIGGER
// with its event, tag filter and function, its enabled state, owner and
// comment.
func (sc *Scripter) ScriptEventTrigger(ctx context.Context, name string) (string, error) {
	op := "script event trigger " + pgsql.QuoteIdent(name)
	return scriptOf(ctx, sc.in(""), func(ctx context.Context) (*EventTrigger, error) {
		return sc.db.EventTriggerByName(ctx, name)
	}, func(e *EventTrigger) (string, error) {
		o := sc.opts
		ename := pgsql.QuoteIdent(e.Name)
		if s, ok := o.member("event trigger "+ename, e.Origin); ok {
			return s, nil
		}
		return o.script(op, o.drop("EVENT TRIGGER", ename), func(s *scriptText) error {
			stmt := "CREATE EVENT TRIGGER " + ename + " ON " + pgsql.QuoteIdent(string(e.Event))
			if len(e.Tags) > 0 {
				tags := make([]string, len(e.Tags))
				for i, t := range e.Tags {
					tags[i] = pgsql.QuoteLiteral(t)
				}
				stmt += "\n    WHEN TAG IN (" + strings.Join(tags, ", ") + ")"
			}
			s.add(stmt + "\n    EXECUTE FUNCTION " + e.Function.QualifiedName() + "()")
			s.add(enableStatement("ALTER EVENT TRIGGER "+ename, e.Enabled))
			s.add(o.owner("EVENT TRIGGER", ename, e.Owner))
			s.add(o.comment("EVENT TRIGGER "+ename, e.Comment))
			return nil
		})
	})
}

// collationOptions are CREATE COLLATION's options for c: its provider and
// locales, determinism and ICU rules. No prefix: the caller names the
// operation.
func collationOptions(c *Collation) ([]string, error) {
	var opts []string
	switch c.Provider {
	case LocaleProviderLibc:
		opts = append(opts, "provider = libc")
		if c.Collate == c.CType {
			opts = append(opts, "locale = "+pgsql.QuoteLiteral(c.Collate))
		} else {
			opts = append(opts, "lc_collate = "+pgsql.QuoteLiteral(c.Collate), "lc_ctype = "+pgsql.QuoteLiteral(c.CType))
		}
	case LocaleProviderICU, LocaleProviderBuiltin:
		opts = append(opts, "provider = "+c.Provider.String(), "locale = "+pgsql.QuoteLiteral(c.Locale))
	default:
		return nil, invalidf("the %s provider is the database's; it has no CREATE COLLATION", c.Provider)
	}
	if !c.Deterministic {
		opts = append(opts, "deterministic = false")
	}
	if c.ICURules != "" {
		opts = append(opts, "rules = "+pgsql.QuoteLiteral(c.ICURules))
	}
	return opts, nil
}

// ScriptCollation scripts the collation schema.name: CREATE COLLATION with
// its provider, locales, determinism and ICU rules (its recorded version is
// the server's to set), owner and comment. The database's default
// collation, whose provider is the database's, has no CREATE:
// ErrInvalidRequest.
func (sc *Scripter) ScriptCollation(ctx context.Context, schema, name string) (string, error) {
	op := "script collation " + pgsql.QualifiedName(schema, name)
	return scriptOf(ctx, sc.in(schema), func(ctx context.Context) (*Collation, error) {
		return sc.db.CollationByName(ctx, schema, name)
	}, func(c *Collation) (string, error) {
		o := sc.opts
		cname := o.name(c.Schema, c.Name)
		if s, ok := o.member("collation "+cname, c.Origin); ok {
			return s, nil
		}
		opts, err := collationOptions(c)
		if err != nil {
			return "", wrapScript(op, err)
		}
		return o.script(op, o.drop("COLLATION", cname), func(s *scriptText) error {
			s.add("CREATE COLLATION" + o.ifNotExists() + " " + cname + " (" + strings.Join(opts, ", ") + ")")
			s.add(o.owner("COLLATION", cname, c.Owner))
			s.add(o.comment("COLLATION "+cname, c.Comment))
			return nil
		})
	})
}

// ------------------------------------------------------------
// Text search
// ------------------------------------------------------------

// definitionList is a CREATE's parenthesised KEY = value list, one per line.
func definitionList(opts []string) string {
	return " (\n    " + strings.Join(opts, ",\n    ") + "\n)"
}

// ScriptTextSearchParser scripts the text search parser schema.name:
// CREATE TEXT SEARCH PARSER with its functions, and its comment. A parser
// has no owner.
func (sc *Scripter) ScriptTextSearchParser(ctx context.Context, schema, name string) (string, error) {
	op := "script text search parser " + pgsql.QualifiedName(schema, name)
	return scriptOf(ctx, sc.in(schema), func(ctx context.Context) (*TextSearchParser, error) {
		return sc.db.TextSearchParserByName(ctx, schema, name)
	}, func(p *TextSearchParser) (string, error) {
		o := sc.opts
		pname := o.name(p.Schema, p.Name)
		if s, ok := o.member("text search parser "+pname, p.Origin); ok {
			return s, nil
		}
		return o.script(op, o.drop("TEXT SEARCH PARSER", pname), func(s *scriptText) error {
			opts := []string{"START = " + p.Start.QualifiedName(), "GETTOKEN = " + p.Token.QualifiedName(),
				"END = " + p.End.QualifiedName(), "LEXTYPES = " + p.LexTypes.QualifiedName()}
			if p.Headline.Name != "" {
				opts = append(opts, "HEADLINE = "+p.Headline.QualifiedName())
			}
			s.add("CREATE TEXT SEARCH PARSER " + pname + definitionList(opts))
			s.add(o.comment("TEXT SEARCH PARSER "+pname, p.Comment))
			return nil
		})
	})
}

// ScriptTextSearchTemplate scripts the text search template schema.name:
// CREATE TEXT SEARCH TEMPLATE with its functions, and its comment. A
// template has no owner.
func (sc *Scripter) ScriptTextSearchTemplate(ctx context.Context, schema, name string) (string, error) {
	op := "script text search template " + pgsql.QualifiedName(schema, name)
	return scriptOf(ctx, sc.in(schema), func(ctx context.Context) (*TextSearchTemplate, error) {
		return sc.db.TextSearchTemplateByName(ctx, schema, name)
	}, func(t *TextSearchTemplate) (string, error) {
		o := sc.opts
		tname := o.name(t.Schema, t.Name)
		if s, ok := o.member("text search template "+tname, t.Origin); ok {
			return s, nil
		}
		return o.script(op, o.drop("TEXT SEARCH TEMPLATE", tname), func(s *scriptText) error {
			var opts []string
			if t.Init.Name != "" {
				opts = append(opts, "INIT = "+t.Init.QualifiedName())
			}
			opts = append(opts, "LEXIZE = "+t.Lexize.QualifiedName())
			s.add("CREATE TEXT SEARCH TEMPLATE " + tname + definitionList(opts))
			s.add(o.comment("TEXT SEARCH TEMPLATE "+tname, t.Comment))
			return nil
		})
	})
}

// ScriptTextSearchDictionary scripts the text search dictionary
// schema.name: CREATE TEXT SEARCH DICTIONARY with its template and options,
// owner and comment.
func (sc *Scripter) ScriptTextSearchDictionary(ctx context.Context, schema, name string) (string, error) {
	op := "script text search dictionary " + pgsql.QualifiedName(schema, name)
	return scriptOf(ctx, sc.in(schema), func(ctx context.Context) (*TextSearchDictionary, error) {
		return sc.db.TextSearchDictionaryByName(ctx, schema, name)
	}, func(d *TextSearchDictionary) (string, error) {
		o := sc.opts
		dname := o.name(d.Schema, d.Name)
		if s, ok := o.member("text search dictionary "+dname, d.Origin); ok {
			return s, nil
		}
		return o.script(op, o.drop("TEXT SEARCH DICTIONARY", dname), func(s *scriptText) error {
			opts := []string{"TEMPLATE = " + d.Template.QualifiedName()}
			if d.Options != "" {
				opts = append(opts, d.Options)
			}
			s.add("CREATE TEXT SEARCH DICTIONARY " + dname + definitionList(opts))
			s.add(o.owner("TEXT SEARCH DICTIONARY", dname, d.Owner))
			s.add(o.comment("TEXT SEARCH DICTIONARY "+dname, d.Comment))
			return nil
		})
	})
}

// ScriptTextSearchConfiguration scripts the text search configuration
// schema.name: CREATE TEXT SEARCH CONFIGURATION on its parser, an ALTER …
// ADD MAPPING per token type, owner and comment.
func (sc *Scripter) ScriptTextSearchConfiguration(ctx context.Context, schema, name string) (string, error) {
	op := "script text search configuration " + pgsql.QualifiedName(schema, name)
	return scriptOf(ctx, sc.in(schema), func(ctx context.Context) (*TextSearchConfiguration, error) {
		return sc.db.TextSearchConfigurationByName(ctx, schema, name)
	}, func(c *TextSearchConfiguration) (string, error) {
		o := sc.opts
		cname := o.name(c.Schema, c.Name)
		if s, ok := o.member("text search configuration "+cname, c.Origin); ok {
			return s, nil
		}
		return o.script(op, o.drop("TEXT SEARCH CONFIGURATION", cname), func(s *scriptText) error {
			s.add("CREATE TEXT SEARCH CONFIGURATION " + cname + " (\n    PARSER = " + c.Parser.QualifiedName() + "\n)")
			for _, m := range c.Mappings {
				dicts := make([]string, len(m.Dictionaries))
				for i, d := range m.Dictionaries {
					dicts[i] = d.QualifiedName()
				}
				s.add("ALTER TEXT SEARCH CONFIGURATION " + cname + "\n    ADD MAPPING FOR " + pgsql.QuoteIdent(m.TokenType) + " WITH " + strings.Join(dicts, ", "))
			}
			s.add(o.owner("TEXT SEARCH CONFIGURATION", cname, c.Owner))
			s.add(o.comment("TEXT SEARCH CONFIGURATION "+cname, c.Comment))
			return nil
		})
	})
}

// ------------------------------------------------------------
// Languages, casts, operators
// ------------------------------------------------------------

// ScriptLanguage scripts the procedural language name: CREATE [OR REPLACE]
// [TRUSTED] LANGUAGE with its handler, inline handler and validator, owner,
// comment and privileges. A language the server is built with (internal,
// c, sql) has no CREATE: ErrInvalidRequest.
func (sc *Scripter) ScriptLanguage(ctx context.Context, name string) (string, error) {
	op := "script language " + pgsql.QuoteIdent(name)
	return scriptOf(ctx, sc.in(""), func(ctx context.Context) (*Language, error) {
		return sc.db.LanguageByName(ctx, name)
	}, func(l *Language) (string, error) {
		o := sc.opts
		lname := pgsql.QuoteIdent(l.Name)
		if s, ok := o.member("language "+lname, l.Origin); ok {
			return s, nil
		}
		if !l.Procedural {
			return "", invalidf("pgmo: %s: a built-in language has no CREATE LANGUAGE", op)
		}
		return o.script(op, o.drop("LANGUAGE", lname), func(s *scriptText) error {
			stmt := "CREATE " + o.orReplace()
			if l.Trusted {
				stmt += "TRUSTED "
			}
			stmt += "LANGUAGE " + lname + "\n    HANDLER " + l.Handler.QualifiedName()
			if l.Inline.Name != "" {
				stmt += "\n    INLINE " + l.Inline.QualifiedName()
			}
			if l.Validator.Name != "" {
				stmt += "\n    VALIDATOR " + l.Validator.QualifiedName()
			}
			s.add(stmt)
			s.add(o.owner("LANGUAGE", lname, l.Owner))
			s.add(o.comment("LANGUAGE "+lname, l.Comment))
			s.add(o.privileges("LANGUAGE "+lname, SecurableLanguage, l.Owner, l.Privileges, l.PrivilegesDefault, sc.db.server.version)...)
			return nil
		})
	})
}

// castClause is what CREATE CAST writes after the signature: how the cast
// converts, and where the server applies it unasked. No prefix.
func castClause(c *Cast) (string, error) {
	var how string
	switch c.Method {
	case CastByFunction:
		how = "WITH FUNCTION " + c.FunctionSignature()
	case CastByInOut:
		how = "WITH INOUT"
	case CastBinary:
		how = "WITHOUT FUNCTION"
	default:
		return "", invalidf("unknown cast method %q", string(c.Method))
	}
	switch c.Context {
	case CastAssignment:
		how += " AS ASSIGNMENT"
	case CastImplicit:
		how += " AS IMPLICIT"
	}
	return how, nil
}

// ScriptCast scripts the cast from the type source to the type target, by
// OID: CREATE CAST WITH FUNCTION, WITHOUT FUNCTION or WITH INOUT, AS
// ASSIGNMENT or AS IMPLICIT, and its comment. A cast has no owner.
func (sc *Scripter) ScriptCast(ctx context.Context, source, target OID) (string, error) {
	op := "script cast"
	return scriptOf(ctx, sc.in(""), func(ctx context.Context) (*Cast, error) {
		return sc.db.CastByTypes(ctx, source, target)
	}, func(c *Cast) (string, error) {
		o := sc.opts
		sig := c.Signature()
		if s, ok := o.member("cast "+sig, c.Origin); ok {
			return s, nil
		}
		op += " " + sig
		how, err := castClause(c)
		if err != nil {
			return "", wrapScript(op, err)
		}
		return o.script(op, o.drop("CAST", sig), func(s *scriptText) error {
			s.add("CREATE CAST " + sig + "\n    " + how)
			s.add(o.comment("CAST "+sig, c.Comment))
			return nil
		})
	})
}

// operatorName is an operator's name as the script writes it: the schema
// quoted with SchemaQualify, the name as it is (an operator name is no
// identifier).
func (o ScriptOptions) operatorName(schema, name string) string {
	if o.SchemaQualify {
		return pgsql.QuoteIdent(schema) + "." + name
	}
	return name
}

// ScriptOperator scripts the operator schema.name on the operand types left
// (0 for a prefix operator) and right: CREATE OPERATOR with its function,
// operand types, commutator, negator, estimators and HASHES/MERGES, owner
// and comment.
func (sc *Scripter) ScriptOperator(ctx context.Context, schema, name string, left, right OID) (string, error) {
	op := "script operator " + pgsql.QuoteIdent(schema) + "." + name
	return scriptOf(ctx, sc.in(schema), func(ctx context.Context) (*Operator, error) {
		return sc.db.OperatorByName(ctx, schema, name, left, right)
	}, func(p *Operator) (string, error) {
		o := sc.opts
		leftArg := p.Left
		if leftArg == "" {
			leftArg = "NONE"
		}
		sig := o.operatorName(p.Schema, p.Name) + " (" + leftArg + ", " + p.Right + ")"
		if s, ok := o.member("operator "+sig, p.Origin); ok {
			return s, nil
		}
		return o.script(op, o.drop("OPERATOR", sig), func(s *scriptText) error {
			opts := []string{"FUNCTION = " + p.Function.QualifiedName()}
			if p.Left != "" {
				opts = append(opts, "LEFTARG = "+p.Left)
			}
			opts = append(opts, "RIGHTARG = "+p.Right)
			operator := func(n ObjectName) string { return "OPERATOR(" + pgsql.QuoteIdent(n.Schema) + "." + n.Name + ")" }
			if p.Commutator.Name != "" {
				opts = append(opts, "COMMUTATOR = "+operator(p.Commutator))
			}
			if p.Negator.Name != "" {
				opts = append(opts, "NEGATOR = "+operator(p.Negator))
			}
			if p.Restrict.Name != "" {
				opts = append(opts, "RESTRICT = "+p.Restrict.QualifiedName())
			}
			if p.Join.Name != "" {
				opts = append(opts, "JOIN = "+p.Join.QualifiedName())
			}
			if p.Hashes {
				opts = append(opts, "HASHES")
			}
			if p.Merges {
				opts = append(opts, "MERGES")
			}
			s.add("CREATE OPERATOR " + o.operatorName(p.Schema, p.Name) + definitionList(opts))
			s.add(o.owner("OPERATOR", sig, p.Owner))
			s.add(o.comment("OPERATOR "+sig, p.Comment))
			return nil
		})
	})
}

// ------------------------------------------------------------
// Foreign data
// ------------------------------------------------------------

// ScriptForeignDataWrapper scripts the foreign-data wrapper name: CREATE
// FOREIGN DATA WRAPPER with its handler, validator and options, owner,
// comment and privileges.
func (sc *Scripter) ScriptForeignDataWrapper(ctx context.Context, name string) (string, error) {
	op := "script foreign data wrapper " + pgsql.QuoteIdent(name)
	return scriptOf(ctx, sc.in(""), func(ctx context.Context) (*ForeignDataWrapper, error) {
		return sc.db.ForeignDataWrapperByName(ctx, name)
	}, func(w *ForeignDataWrapper) (string, error) {
		o := sc.opts
		wname := pgsql.QuoteIdent(w.Name)
		if s, ok := o.member("foreign data wrapper "+wname, w.Origin); ok {
			return s, nil
		}
		return o.script(op, o.drop("FOREIGN DATA WRAPPER", wname), func(s *scriptText) error {
			stmt := "CREATE FOREIGN DATA WRAPPER " + wname
			if w.Handler.Name != "" {
				stmt += "\n    HANDLER " + w.Handler.QualifiedName()
			}
			if w.Validator.Name != "" {
				stmt += "\n    VALIDATOR " + w.Validator.QualifiedName()
			}
			if len(w.Options) > 0 {
				stmt += "\n    OPTIONS " + genericOptions(w.Options)
			}
			s.add(stmt)
			s.add(o.owner("FOREIGN DATA WRAPPER", wname, w.Owner))
			s.add(o.comment("FOREIGN DATA WRAPPER "+wname, w.Comment))
			s.add(o.privileges("FOREIGN DATA WRAPPER "+wname, SecurableForeignDataWrapper, w.Owner, w.Privileges, w.PrivilegesDefault, sc.db.server.version)...)
			return nil
		})
	})
}

// ScriptForeignServer scripts the foreign server name: CREATE SERVER with
// its type, version, wrapper and options, owner, comment and privileges.
// Its user mappings are scripted on their own.
func (sc *Scripter) ScriptForeignServer(ctx context.Context, name string) (string, error) {
	op := "script foreign server " + pgsql.QuoteIdent(name)
	return scriptOf(ctx, sc.in(""), func(ctx context.Context) (*ForeignServer, error) {
		return sc.db.ForeignServerByName(ctx, name)
	}, func(f *ForeignServer) (string, error) {
		o := sc.opts
		fname := pgsql.QuoteIdent(f.Name)
		if s, ok := o.member("foreign server "+fname, f.Origin); ok {
			return s, nil
		}
		return o.script(op, o.drop("SERVER", fname), func(s *scriptText) error {
			stmt := "CREATE SERVER" + o.ifNotExists() + " " + fname
			if f.Type != "" {
				stmt += " TYPE " + pgsql.QuoteLiteral(f.Type)
			}
			if f.Version != "" {
				stmt += " VERSION " + pgsql.QuoteLiteral(f.Version)
			}
			stmt += "\n    FOREIGN DATA WRAPPER " + pgsql.QuoteIdent(f.Wrapper)
			if len(f.Options) > 0 {
				stmt += "\n    OPTIONS " + genericOptions(f.Options)
			}
			s.add(stmt)
			s.add(o.owner("SERVER", fname, f.Owner))
			s.add(o.comment("SERVER "+fname, f.Comment))
			s.add(o.privileges("FOREIGN SERVER "+fname, SecurableForeignServer, f.Owner, f.Privileges, f.PrivilegesDefault, sc.db.server.version)...)
			return nil
		})
	})
}

// userMappingOptions is genericOptions with a password option's value, a
// secret, replaced by PasswordPlaceholder, unquoted: the statement does not
// parse until a password is put in its place.
func userMappingOptions(params []ParameterSetting) string {
	parts := make([]string, len(params))
	for i, p := range params {
		v := pgsql.QuoteLiteral(p.Value)
		if p.Name == "password" {
			v = PasswordPlaceholder
		}
		parts[i] = pgsql.QuoteIdent(p.Name) + " " + v
	}
	return "(" + strings.Join(parts, ", ") + ")"
}

// ScriptUserMapping scripts the user mapping of the role user ("" for
// PUBLIC) on the foreign server server: CREATE USER MAPPING with its
// options. A password option's value is a secret: the script writes
// PasswordPlaceholder, unquoted, in its place, so the script does not parse
// until it is replaced. Options the connected role may not read
// (OptionsHidden) are left out, with a note saying so.
func (sc *Scripter) ScriptUserMapping(ctx context.Context, server, user string) (string, error) {
	op := "script user mapping for " + roleSpec(user) + " server " + pgsql.QuoteIdent(server)
	return scriptOf(ctx, sc.in(""), func(ctx context.Context) (*UserMapping, error) {
		return sc.db.UserMappingByName(ctx, server, user)
	}, func(m *UserMapping) (string, error) {
		o := sc.opts
		target := "FOR " + roleSpec(m.User) + " SERVER " + pgsql.QuoteIdent(m.Server)
		if s, ok := o.member("user mapping "+target, m.Origin); ok {
			return s, nil
		}
		return o.script(op, o.dropPlain("USER MAPPING", target), func(s *scriptText) error {
			if m.OptionsHidden {
				s.notef("the mapping's options are hidden from the connected role and are not scripted")
			}
			stmt := "CREATE USER MAPPING" + o.ifNotExists() + " " + target
			if len(m.Options) > 0 {
				stmt += "\n    OPTIONS " + userMappingOptions(m.Options)
			}
			s.add(stmt)
			return nil
		})
	})
}
