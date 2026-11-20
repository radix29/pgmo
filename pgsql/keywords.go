package pgsql

// keywords is every PostgreSQL keyword that quote_ident quotes: those
// pg_get_keywords() lists with a catcode other than U (unreserved) — column
// names (C), type or function names (T) and reserved words (R). It is the
// union over every supported server version: keywords are only ever added
// (16 added json_array, json_arrayagg, json_object, json_objectagg and
// system_user; 17 json, json_exists, json_query, json_scalar,
// json_serialize, json_table, json_value and merge_action; 18 none), so
// this is pg18's list, read on 2026-11-20 with
//
//	SELECT word FROM pg_catalog.pg_get_keywords() WHERE catcode <> 'U'
//
// A name that is a keyword only on a newer server is still quoted when the
// text is generated against an older one: a script may run on either, and
// quoting a name that needed none is harmless. TestLiveQuoteIdentMatchesServer
// (package pgmo) pins the list to each lab server's.
var keywords = map[string]struct{}{
	"all": {}, "analyse": {}, "analyze": {}, "and": {}, "any": {}, "array": {},
	"as": {}, "asc": {}, "asymmetric": {}, "authorization": {}, "between": {}, "bigint": {},
	"binary": {}, "bit": {}, "boolean": {}, "both": {}, "case": {}, "cast": {},
	"char": {}, "character": {}, "check": {}, "coalesce": {}, "collate": {}, "collation": {},
	"column": {}, "concurrently": {}, "constraint": {}, "create": {}, "cross": {}, "current_catalog": {},
	"current_date": {}, "current_role": {}, "current_schema": {}, "current_time": {}, "current_timestamp": {}, "current_user": {},
	"dec": {}, "decimal": {}, "default": {}, "deferrable": {}, "desc": {}, "distinct": {},
	"do": {}, "else": {}, "end": {}, "except": {}, "exists": {}, "extract": {},
	"false": {}, "fetch": {}, "float": {}, "for": {}, "foreign": {}, "freeze": {},
	"from": {}, "full": {}, "grant": {}, "greatest": {}, "group": {}, "grouping": {},
	"having": {}, "ilike": {}, "in": {}, "initially": {}, "inner": {}, "inout": {},
	"int": {}, "integer": {}, "intersect": {}, "interval": {}, "into": {}, "is": {},
	"isnull": {}, "join": {}, "json": {}, "json_array": {}, "json_arrayagg": {}, "json_exists": {},
	"json_object": {}, "json_objectagg": {}, "json_query": {}, "json_scalar": {}, "json_serialize": {}, "json_table": {},
	"json_value": {}, "lateral": {}, "leading": {}, "least": {}, "left": {}, "like": {},
	"limit": {}, "localtime": {}, "localtimestamp": {}, "merge_action": {}, "national": {}, "natural": {},
	"nchar": {}, "none": {}, "normalize": {}, "not": {}, "notnull": {}, "null": {},
	"nullif": {}, "numeric": {}, "offset": {}, "on": {}, "only": {}, "or": {},
	"order": {}, "out": {}, "outer": {}, "overlaps": {}, "overlay": {}, "placing": {},
	"position": {}, "precision": {}, "primary": {}, "real": {}, "references": {}, "returning": {},
	"right": {}, "row": {}, "select": {}, "session_user": {}, "setof": {}, "similar": {},
	"smallint": {}, "some": {}, "substring": {}, "symmetric": {}, "system_user": {}, "table": {},
	"tablesample": {}, "then": {}, "time": {}, "timestamp": {}, "to": {}, "trailing": {},
	"treat": {}, "trim": {}, "true": {}, "union": {}, "unique": {}, "user": {},
	"using": {}, "values": {}, "varchar": {}, "variadic": {}, "verbose": {}, "when": {},
	"where": {}, "window": {}, "with": {}, "xmlattributes": {}, "xmlconcat": {}, "xmlelement": {},
	"xmlexists": {}, "xmlforest": {}, "xmlnamespaces": {}, "xmlparse": {}, "xmlpi": {}, "xmlroot": {},
	"xmlserialize": {}, "xmltable": {},
}
