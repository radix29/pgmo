package pgmo

import (
	"context"
	"fmt"

	"github.com/radix29/pgmo/pgsql"
)

// ============================================================
// Text search
// ============================================================
//
// text_search.go reads a database's full-text search objects (PLAN.md
// P4.11), through the database's own pool: four families, as four
// statements (CREATE TEXT SEARCH CONFIGURATION, DICTIONARY, PARSER,
// TEMPLATE) and four Object Explorer folders, each a schema's object:
//
//   - a parser (pg_ts_parser) splits a document into tokens of the types
//     it knows (asciiword, url, …) — C functions, so creating one takes a
//     superuser; it has no owner;
//   - a template (pg_ts_template) is the C functions behind a kind of
//     dictionary — likewise a superuser's, with no owner;
//   - a dictionary (pg_ts_dict) is a template with options (a stem
//     language, a stop-word file);
//   - a configuration (pg_ts_config) is a parser and, per token type, the
//     dictionaries tried in turn (pg_ts_config_map) — what to_tsvector
//     takes.
//
// None has an ACL, and the catalogs are readable by every role, so every
// listing is the same for all roles. initdb puts the built-in ones in
// pg_catalog: IsSystem is the schema's rule, as for every schema object.

// TextSearchParser is one text search parser of a database. Read it with
// Database.TextSearchParsers or Database.TextSearchParserByName;
// Database.TextSearchParserRef is a handle holding the name alone.
type TextSearchParser struct {
	db *Database

	// Origin: IsSystem for a parser in a system schema (pg_catalog's
	// default); Extension for one an extension owns.
	Origin

	OID    OID
	Schema string
	Name   string

	// Start, Token, End and LexTypes are the parser's functions (START,
	// GETTOKEN, END, LEXTYPES); Headline is HEADLINE's, zero for none.
	Start, Token, End, LexTypes ObjectName
	Headline                    ObjectName

	// Comment is COMMENT ON TEXT SEARCH PARSER's text, empty for none.
	Comment string
}

// TextSearchTemplate is one text search template of a database. Read it
// with Database.TextSearchTemplates or Database.TextSearchTemplateByName;
// Database.TextSearchTemplateRef is a handle holding the name alone.
type TextSearchTemplate struct {
	db *Database

	// Origin: IsSystem for a template in a system schema (pg_catalog's
	// simple, synonym, ispell, thesaurus, snowball); Extension for one an
	// extension owns.
	Origin

	OID    OID
	Schema string
	Name   string

	// Init is INIT's function, zero for none; Lexize is LEXIZE's.
	Init, Lexize ObjectName

	// Comment is COMMENT ON TEXT SEARCH TEMPLATE's text, empty for none.
	Comment string
}

// TextSearchDictionary is one text search dictionary of a database. Read it
// with Database.TextSearchDictionaries or
// Database.TextSearchDictionaryByName; Database.TextSearchDictionaryRef is
// a handle holding the name alone.
type TextSearchDictionary struct {
	db *Database

	// Origin: IsSystem for a dictionary in a system schema (pg_catalog's
	// simple and *_stem); Extension for one an extension owns.
	Origin

	OID    OID
	Schema string
	Name   string

	// Owner is the owning role's name (dictowner).
	Owner string

	// Template is the template the dictionary instantiates.
	Template ObjectName

	// Options are the template's options as the server stores them
	// (dictinitoption), the text CREATE TEXT SEARCH DICTIONARY's list after
	// TEMPLATE reads as — "language = 'english', stopwords = 'english'";
	// empty for none.
	Options string

	// Comment is COMMENT ON TEXT SEARCH DICTIONARY's text, empty for none.
	Comment string
}

// TextSearchMapping is one token type of a configuration and the
// dictionaries consulted for it, in order: the first that recognises the
// token gives its lexemes.
type TextSearchMapping struct {
	// TokenType is the parser's alias for the type (ts_token_type:
	// asciiword, url…), what ALTER TEXT SEARCH CONFIGURATION … MAPPING FOR
	// takes.
	TokenType string

	// Dictionaries are the dictionaries in the order tried.
	Dictionaries []ObjectName
}

// TextSearchConfiguration is one text search configuration of a database.
// Read it with Database.TextSearchConfigurations or
// Database.TextSearchConfigurationByName;
// Database.TextSearchConfigurationRef is a handle holding the name alone.
type TextSearchConfiguration struct {
	db *Database

	// Origin: IsSystem for a configuration in a system schema
	// (pg_catalog's simple, english…); Extension for one an extension
	// owns.
	Origin

	OID    OID
	Schema string
	Name   string

	// Owner is the owning role's name (cfgowner).
	Owner string

	// Parser is the parser it splits documents with.
	Parser ObjectName

	// Mappings are its token types in the parser's numbering, each with
	// its dictionaries; a type with none is not listed (the parser's tokens
	// of it are dropped). Nil for a configuration mapping nothing.
	Mappings []TextSearchMapping

	// Comment is COMMENT ON TEXT SEARCH CONFIGURATION's text, empty for
	// none.
	Comment string
}

// Database is the Database the parser was read from.
func (p *TextSearchParser) Database() *Database { return p.db }

// QualifiedName is the parser's name quoted for a statement: schema.name.
func (p *TextSearchParser) QualifiedName() string { return pgsql.QualifiedName(p.Schema, p.Name) }

// Database is the Database the template was read from.
func (t *TextSearchTemplate) Database() *Database { return t.db }

// QualifiedName is the template's name quoted for a statement:
// schema.name.
func (t *TextSearchTemplate) QualifiedName() string { return pgsql.QualifiedName(t.Schema, t.Name) }

// Database is the Database the dictionary was read from.
func (dc *TextSearchDictionary) Database() *Database { return dc.db }

// QualifiedName is the dictionary's name quoted for a statement:
// schema.name.
func (dc *TextSearchDictionary) QualifiedName() string {
	return pgsql.QualifiedName(dc.Schema, dc.Name)
}

// Database is the Database the configuration was read from.
func (c *TextSearchConfiguration) Database() *Database { return c.db }

// QualifiedName is the configuration's name quoted for a statement:
// schema.name.
func (c *TextSearchConfiguration) QualifiedName() string {
	return pgsql.QualifiedName(c.Schema, c.Name)
}

// ============================================================
// Queries
// ============================================================

// textSearchComment LEFT JOINs the comment of the object alias.oid of
// catalog as ds.
func textSearchComment(catalog catalogTable, alias string) string {
	return `LEFT JOIN pg_catalog.pg_description ds ON ds.objoid = ` + alias + `.oid
	AND ds.classoid = '` + string(catalog) + `'::pg_catalog.regclass AND ds.objsubid = 0`
}

// textSearchFilterColumns are a text search listing's ObjectFilter columns
// for the catalog whose row is alias and whose name column is
// alias.<prefix>name.
func textSearchFilterColumns(catalog catalogTable, alias, prefix string) filterColumns {
	return filterColumns{
		name:      alias + "." + prefix + "name",
		schema:    "n.nspname",
		system:    systemSchema("n.nspname"),
		extension: isExtensionMember(catalog, alias+".oid"),
	}
}

// textSearchFamily is what one text search family's reads differ by: its
// noun for messages, its select (ending WHERE true), its filter columns, the
// ORDER BY of a listing, and the schema and name columns a by-name read
// binds.
type textSearchFamily struct {
	noun       string
	sel        string
	cols       filterColumns
	order      string
	schemaName string // "n.nspname = $1 AND x.xname = $2"
}

// listTextSearch is the listing every text search family shares.
func listTextSearch[T any](ctx context.Context, d *Database, fam textSearchFamily, f ObjectFilter,
	scan func(*Database, func(...any) error) (T, error)) ([]T, error) {
	where, args, err := f.clause(fam.cols, nil)
	if err != nil {
		return nil, err
	}
	q, err := d.server.onDatabase(ctx, d.Name)
	if err != nil {
		return nil, err
	}
	return scanRows(ctx, q, fmt.Sprintf("list %ss of database %q", fam.noun, d.Name), func(s func(...any) error) (T, error) {
		return scan(d, s)
	}, fam.sel+where+`
ORDER BY `+fam.order, args...)
}

// textSearchByName is the by-name read every text search family shares:
// schema is required (ErrSchemaRequired), no row is ErrNotFound.
func textSearchByName[T any](ctx context.Context, d *Database, fam textSearchFamily, schema, name string,
	scan func(*Database, func(...any) error) (T, error)) (T, error) {
	var zero T
	qualified := pgsql.QualifiedName(schema, name)
	if schema == "" {
		return zero, fmt.Errorf("pgmo: read %s %q: %w", fam.noun, name, ErrSchemaRequired)
	}
	q, err := d.server.onDatabase(ctx, d.Name)
	if err != nil {
		return zero, err
	}
	return readByName(ctx, q, d, scan, fam.sel+`
	AND `+fam.schemaName, []any{schema, name},
		notFoundf("pgmo: %s %s not found in database %q", fam.noun, qualified, d.Name),
		fmt.Sprintf("read %s %s of database %q", fam.noun, qualified, d.Name))
}

// ---- parsers ----

var textSearchParserFamily = textSearchFamily{
	noun: "text search parser",
	sel: `SELECT pr.oid, n.nspname::pg_catalog.text, pr.prsname::pg_catalog.text,
	` + originJoinColumns("n.nspname") + `,
	` + procedureName("fst") + `, ` + procedureName("ftok") + `, ` + procedureName("fend") + `,
	` + procedureName("flex") + `, ` + procedureName("fhl") + `,
	COALESCE(ds.description, '')
FROM pg_catalog.pg_ts_parser pr
JOIN pg_catalog.pg_namespace n ON n.oid = pr.prsnamespace
` + procedureJoin("pr.prsstart", "fst") + `
` + procedureJoin("pr.prstoken", "ftok") + `
` + procedureJoin("pr.prsend", "fend") + `
` + procedureJoin("pr.prslextype", "flex") + `
` + procedureJoin("pr.prsheadline", "fhl") + `
` + originJoin(catalogTSParser, "pr.oid") + `
` + textSearchComment(catalogTSParser, "pr") + `
WHERE true`,
	cols:       textSearchFilterColumns(catalogTSParser, "pr", "prs"),
	order:      "n.nspname, pr.prsname",
	schemaName: "n.nspname = $1::pg_catalog.text AND pr.prsname = $2::pg_catalog.text",
}

// scanTextSearchParser builds a *TextSearchParser of d from one row.
func scanTextSearchParser(d *Database, scan func(...any) error) (*TextSearchParser, error) {
	p := &TextSearchParser{db: d}
	var start, token, end, lex, hl []string
	dest := []any{&p.OID, &p.Schema, &p.Name}
	dest = append(dest, scanOrigin(&p.Origin)...)
	dest = append(dest, &start, &token, &end, &lex, &hl, &p.Comment)
	if err := scan(dest...); err != nil {
		return nil, err
	}
	p.Start, p.Token, p.End = objectNameOf(start), objectNameOf(token), objectNameOf(end)
	p.LexTypes, p.Headline = objectNameOf(lex), objectNameOf(hl)
	return p, nil
}

// TextSearchParsers lists the database's text search parsers by schema then
// name, narrowed by f: Name, Schema, System (pg_catalog's), ExtensionMember.
// They are read through the database's own pool.
func (d *Database) TextSearchParsers(ctx context.Context, f ObjectFilter) ([]*TextSearchParser, error) {
	return listTextSearch(ctx, d, textSearchParserFamily, f, scanTextSearchParser)
}

// TextSearchParserByName reads the text search parser schema.name — stored
// names, matched exactly (fold typed input with pgsql.FoldIdent first).
// schema is required (ErrSchemaRequired); none is ErrNotFound. Use
// TextSearchParserRef instead when only a handle to write through is
// needed.
func (d *Database) TextSearchParserByName(ctx context.Context, schema, name string) (*TextSearchParser, error) {
	return textSearchByName(ctx, d, textSearchParserFamily, schema, name, scanTextSearchParser)
}

// TextSearchParserRef returns a handle for the text search parser
// schema.name without reading the catalog: Schema and Name are set and
// every other field is zero, OID included. It is not interchangeable with
// TextSearchParserByName.
func (d *Database) TextSearchParserRef(schema, name string) *TextSearchParser {
	return &TextSearchParser{db: d, Schema: schema, Name: name}
}

// ---- templates ----

var textSearchTemplateFamily = textSearchFamily{
	noun: "text search template",
	sel: `SELECT tm.oid, n.nspname::pg_catalog.text, tm.tmplname::pg_catalog.text,
	` + originJoinColumns("n.nspname") + `,
	` + procedureName("fin") + `, ` + procedureName("flx") + `,
	COALESCE(ds.description, '')
FROM pg_catalog.pg_ts_template tm
JOIN pg_catalog.pg_namespace n ON n.oid = tm.tmplnamespace
` + procedureJoin("tm.tmplinit", "fin") + `
` + procedureJoin("tm.tmpllexize", "flx") + `
` + originJoin(catalogTSTemplate, "tm.oid") + `
` + textSearchComment(catalogTSTemplate, "tm") + `
WHERE true`,
	cols:       textSearchFilterColumns(catalogTSTemplate, "tm", "tmpl"),
	order:      "n.nspname, tm.tmplname",
	schemaName: "n.nspname = $1::pg_catalog.text AND tm.tmplname = $2::pg_catalog.text",
}

// scanTextSearchTemplate builds a *TextSearchTemplate of d from one row.
func scanTextSearchTemplate(d *Database, scan func(...any) error) (*TextSearchTemplate, error) {
	t := &TextSearchTemplate{db: d}
	var init, lexize []string
	dest := []any{&t.OID, &t.Schema, &t.Name}
	dest = append(dest, scanOrigin(&t.Origin)...)
	dest = append(dest, &init, &lexize, &t.Comment)
	if err := scan(dest...); err != nil {
		return nil, err
	}
	t.Init, t.Lexize = objectNameOf(init), objectNameOf(lexize)
	return t, nil
}

// TextSearchTemplates lists the database's text search templates by schema
// then name, narrowed by f: Name, Schema, System (pg_catalog's),
// ExtensionMember. They are read through the database's own pool.
func (d *Database) TextSearchTemplates(ctx context.Context, f ObjectFilter) ([]*TextSearchTemplate, error) {
	return listTextSearch(ctx, d, textSearchTemplateFamily, f, scanTextSearchTemplate)
}

// TextSearchTemplateByName reads the text search template schema.name —
// stored names, matched exactly (fold typed input with pgsql.FoldIdent
// first). schema is required (ErrSchemaRequired); none is ErrNotFound. Use
// TextSearchTemplateRef instead when only a handle to write through is
// needed.
func (d *Database) TextSearchTemplateByName(ctx context.Context, schema, name string) (*TextSearchTemplate, error) {
	return textSearchByName(ctx, d, textSearchTemplateFamily, schema, name, scanTextSearchTemplate)
}

// TextSearchTemplateRef returns a handle for the text search template
// schema.name without reading the catalog: Schema and Name are set and
// every other field is zero, OID included. It is not interchangeable with
// TextSearchTemplateByName.
func (d *Database) TextSearchTemplateRef(schema, name string) *TextSearchTemplate {
	return &TextSearchTemplate{db: d, Schema: schema, Name: name}
}

// ---- dictionaries ----

var textSearchDictionaryFamily = textSearchFamily{
	noun: "text search dictionary",
	sel: `SELECT dc.oid, n.nspname::pg_catalog.text, dc.dictname::pg_catalog.text,
	pg_catalog.pg_get_userbyid(dc.dictowner)::pg_catalog.text,
	` + originJoinColumns("n.nspname") + `,
	tn.nspname::pg_catalog.text, tm.tmplname::pg_catalog.text,
	COALESCE(dc.dictinitoption, ''),
	COALESCE(ds.description, '')
FROM pg_catalog.pg_ts_dict dc
JOIN pg_catalog.pg_namespace n ON n.oid = dc.dictnamespace
JOIN pg_catalog.pg_ts_template tm ON tm.oid = dc.dicttemplate
JOIN pg_catalog.pg_namespace tn ON tn.oid = tm.tmplnamespace
` + originJoin(catalogTSDict, "dc.oid") + `
` + textSearchComment(catalogTSDict, "dc") + `
WHERE true`,
	cols:       textSearchFilterColumns(catalogTSDict, "dc", "dict"),
	order:      "n.nspname, dc.dictname",
	schemaName: "n.nspname = $1::pg_catalog.text AND dc.dictname = $2::pg_catalog.text",
}

// scanTextSearchDictionary builds a *TextSearchDictionary of d from one row.
func scanTextSearchDictionary(d *Database, scan func(...any) error) (*TextSearchDictionary, error) {
	dc := &TextSearchDictionary{db: d}
	dest := []any{&dc.OID, &dc.Schema, &dc.Name, &dc.Owner}
	dest = append(dest, scanOrigin(&dc.Origin)...)
	dest = append(dest, &dc.Template.Schema, &dc.Template.Name, &dc.Options, &dc.Comment)
	if err := scan(dest...); err != nil {
		return nil, err
	}
	return dc, nil
}

// TextSearchDictionaries lists the database's text search dictionaries by
// schema then name, narrowed by f: Name, Schema, System (pg_catalog's),
// ExtensionMember. They are read through the database's own pool.
func (d *Database) TextSearchDictionaries(ctx context.Context, f ObjectFilter) ([]*TextSearchDictionary, error) {
	return listTextSearch(ctx, d, textSearchDictionaryFamily, f, scanTextSearchDictionary)
}

// TextSearchDictionaryByName reads the text search dictionary schema.name —
// stored names, matched exactly (fold typed input with pgsql.FoldIdent
// first). schema is required (ErrSchemaRequired); none is ErrNotFound. Use
// TextSearchDictionaryRef instead when only a handle to write through is
// needed.
func (d *Database) TextSearchDictionaryByName(ctx context.Context, schema, name string) (*TextSearchDictionary, error) {
	return textSearchByName(ctx, d, textSearchDictionaryFamily, schema, name, scanTextSearchDictionary)
}

// TextSearchDictionaryRef returns a handle for the text search dictionary
// schema.name without reading the catalog: Schema and Name are set and
// every other field is zero, OID included. It is not interchangeable with
// TextSearchDictionaryByName.
func (d *Database) TextSearchDictionaryRef(schema, name string) *TextSearchDictionary {
	return &TextSearchDictionary{db: d, Schema: schema, Name: name}
}

// ---- configurations ----

// textSearchMappings is the LEFT JOIN reading every configuration's
// mappings as tm's three parallel arrays — token type alias, dictionary
// schema, dictionary name — ordered by token type then position, which
// scanTextSearchConfiguration groups by token type. The aliases are each
// parser's ts_token_type, called once per parser (there are few), not per
// configuration or mapping; a type the parser no longer reports reads as
// its number.
const textSearchMappings = `LEFT JOIN (SELECT m.mapcfg,
		pg_catalog.array_agg(COALESCE(tt.alias, m.maptokentype::pg_catalog.text) ORDER BY m.maptokentype, m.mapseqno),
		pg_catalog.array_agg(mdn.nspname::pg_catalog.text ORDER BY m.maptokentype, m.mapseqno),
		pg_catalog.array_agg(md.dictname::pg_catalog.text ORDER BY m.maptokentype, m.mapseqno)
	FROM pg_catalog.pg_ts_config_map m
	JOIN pg_catalog.pg_ts_config mc ON mc.oid = m.mapcfg
	JOIN pg_catalog.pg_ts_dict md ON md.oid = m.mapdict
	JOIN pg_catalog.pg_namespace mdn ON mdn.oid = md.dictnamespace
	LEFT JOIN (SELECT p.oid AS parser, t.tokid, t.alias
		FROM pg_catalog.pg_ts_parser p
		CROSS JOIN LATERAL pg_catalog.ts_token_type(p.oid) AS t) AS tt
		ON tt.parser = mc.cfgparser AND tt.tokid = m.maptokentype
	GROUP BY m.mapcfg) AS tmap(cfg, tokens, schemas, names) ON tmap.cfg = c.oid`

var textSearchConfigurationFamily = textSearchFamily{
	noun: "text search configuration",
	sel: `SELECT c.oid, n.nspname::pg_catalog.text, c.cfgname::pg_catalog.text,
	pg_catalog.pg_get_userbyid(c.cfgowner)::pg_catalog.text,
	` + originJoinColumns("n.nspname") + `,
	pn.nspname::pg_catalog.text, pr.prsname::pg_catalog.text,
	tmap.tokens, tmap.schemas, tmap.names,
	COALESCE(ds.description, '')
FROM pg_catalog.pg_ts_config c
JOIN pg_catalog.pg_namespace n ON n.oid = c.cfgnamespace
JOIN pg_catalog.pg_ts_parser pr ON pr.oid = c.cfgparser
JOIN pg_catalog.pg_namespace pn ON pn.oid = pr.prsnamespace
` + textSearchMappings + `
` + originJoin(catalogTSConfig, "c.oid") + `
` + textSearchComment(catalogTSConfig, "c") + `
WHERE true`,
	cols:       textSearchFilterColumns(catalogTSConfig, "c", "cfg"),
	order:      "n.nspname, c.cfgname",
	schemaName: "n.nspname = $1::pg_catalog.text AND c.cfgname = $2::pg_catalog.text",
}

// scanTextSearchConfiguration builds a *TextSearchConfiguration of d from
// one row.
func scanTextSearchConfiguration(d *Database, scan func(...any) error) (*TextSearchConfiguration, error) {
	c := &TextSearchConfiguration{db: d}
	var tokens, schemas, names []string
	dest := []any{&c.OID, &c.Schema, &c.Name, &c.Owner}
	dest = append(dest, scanOrigin(&c.Origin)...)
	dest = append(dest, &c.Parser.Schema, &c.Parser.Name, &tokens, &schemas, &names, &c.Comment)
	if err := scan(dest...); err != nil {
		return nil, err
	}
	if len(schemas) != len(tokens) || len(names) != len(tokens) {
		return nil, fmt.Errorf("mapping arrays of different lengths: %d token types, %d schemas, %d names",
			len(tokens), len(schemas), len(names))
	}
	for i, tok := range tokens {
		dict := ObjectName{Schema: schemas[i], Name: names[i]}
		if n := len(c.Mappings); n > 0 && c.Mappings[n-1].TokenType == tok {
			c.Mappings[n-1].Dictionaries = append(c.Mappings[n-1].Dictionaries, dict)
			continue
		}
		c.Mappings = append(c.Mappings, TextSearchMapping{TokenType: tok, Dictionaries: []ObjectName{dict}})
	}
	return c, nil
}

// TextSearchConfigurations lists the database's text search configurations
// by schema then name, with their mappings, narrowed by f: Name, Schema,
// System (pg_catalog's), ExtensionMember. They are read through the
// database's own pool.
func (d *Database) TextSearchConfigurations(ctx context.Context, f ObjectFilter) ([]*TextSearchConfiguration, error) {
	return listTextSearch(ctx, d, textSearchConfigurationFamily, f, scanTextSearchConfiguration)
}

// TextSearchConfigurationByName reads the text search configuration
// schema.name — stored names, matched exactly (fold typed input with
// pgsql.FoldIdent first). schema is required (ErrSchemaRequired); none is
// ErrNotFound. Use TextSearchConfigurationRef instead when only a handle
// to write through is needed.
func (d *Database) TextSearchConfigurationByName(ctx context.Context, schema, name string) (*TextSearchConfiguration, error) {
	return textSearchByName(ctx, d, textSearchConfigurationFamily, schema, name, scanTextSearchConfiguration)
}

// TextSearchConfigurationRef returns a handle for the text search
// configuration schema.name without reading the catalog: Schema and Name
// are set and every other field is zero, OID included. It is not
// interchangeable with TextSearchConfigurationByName.
func (d *Database) TextSearchConfigurationRef(schema, name string) *TextSearchConfiguration {
	return &TextSearchConfiguration{db: d, Schema: schema, Name: name}
}
