-- PostgreSQL 15
SELECT co.oid, n.nspname::pg_catalog.text, co.collname::pg_catalog.text,
	pg_catalog.pg_get_userbyid(co.collowner)::pg_catalog.text,
	(pg_catalog.starts_with(n.nspname, 'pg_') OR n.nspname = 'information_schema'), COALESCE(ext.extname::pg_catalog.text, ''),
	co.collprovider::pg_catalog.text, co.collisdeterministic,
	pg_catalog.pg_encoding_to_char(co.collencoding)::pg_catalog.text,
	COALESCE(co.collcollate, ''), COALESCE(co.collctype, ''),
	COALESCE(co.colliculocale, ''),
	COALESCE(NULL::pg_catalog.text, ''),
	COALESCE(co.collversion, ''),
	COALESCE(ds.description, '')
FROM pg_catalog.pg_collation co
JOIN pg_catalog.pg_namespace n ON n.oid = co.collnamespace
LEFT JOIN pg_catalog.pg_depend d ON d.classid = 'pg_catalog.pg_collation'::pg_catalog.regclass AND d.objid = co.oid AND d.objsubid = 0
			AND d.refclassid = 'pg_catalog.pg_extension'::pg_catalog.regclass AND d.deptype = 'e'
LEFT JOIN pg_catalog.pg_extension ext ON ext.oid = d.refobjid
LEFT JOIN pg_catalog.pg_description ds ON ds.objoid = co.oid
	AND ds.classoid = 'pg_catalog.pg_collation'::pg_catalog.regclass AND ds.objsubid = 0
WHERE true
-- PostgreSQL 16
SELECT co.oid, n.nspname::pg_catalog.text, co.collname::pg_catalog.text,
	pg_catalog.pg_get_userbyid(co.collowner)::pg_catalog.text,
	(pg_catalog.starts_with(n.nspname, 'pg_') OR n.nspname = 'information_schema'), COALESCE(ext.extname::pg_catalog.text, ''),
	co.collprovider::pg_catalog.text, co.collisdeterministic,
	pg_catalog.pg_encoding_to_char(co.collencoding)::pg_catalog.text,
	COALESCE(co.collcollate, ''), COALESCE(co.collctype, ''),
	COALESCE(co.colliculocale, ''),
	COALESCE(co.collicurules, ''),
	COALESCE(co.collversion, ''),
	COALESCE(ds.description, '')
FROM pg_catalog.pg_collation co
JOIN pg_catalog.pg_namespace n ON n.oid = co.collnamespace
LEFT JOIN pg_catalog.pg_depend d ON d.classid = 'pg_catalog.pg_collation'::pg_catalog.regclass AND d.objid = co.oid AND d.objsubid = 0
			AND d.refclassid = 'pg_catalog.pg_extension'::pg_catalog.regclass AND d.deptype = 'e'
LEFT JOIN pg_catalog.pg_extension ext ON ext.oid = d.refobjid
LEFT JOIN pg_catalog.pg_description ds ON ds.objoid = co.oid
	AND ds.classoid = 'pg_catalog.pg_collation'::pg_catalog.regclass AND ds.objsubid = 0
WHERE true
-- PostgreSQL 17
SELECT co.oid, n.nspname::pg_catalog.text, co.collname::pg_catalog.text,
	pg_catalog.pg_get_userbyid(co.collowner)::pg_catalog.text,
	(pg_catalog.starts_with(n.nspname, 'pg_') OR n.nspname = 'information_schema'), COALESCE(ext.extname::pg_catalog.text, ''),
	co.collprovider::pg_catalog.text, co.collisdeterministic,
	pg_catalog.pg_encoding_to_char(co.collencoding)::pg_catalog.text,
	COALESCE(co.collcollate, ''), COALESCE(co.collctype, ''),
	COALESCE(co.colllocale, ''),
	COALESCE(co.collicurules, ''),
	COALESCE(co.collversion, ''),
	COALESCE(ds.description, '')
FROM pg_catalog.pg_collation co
JOIN pg_catalog.pg_namespace n ON n.oid = co.collnamespace
LEFT JOIN pg_catalog.pg_depend d ON d.classid = 'pg_catalog.pg_collation'::pg_catalog.regclass AND d.objid = co.oid AND d.objsubid = 0
			AND d.refclassid = 'pg_catalog.pg_extension'::pg_catalog.regclass AND d.deptype = 'e'
LEFT JOIN pg_catalog.pg_extension ext ON ext.oid = d.refobjid
LEFT JOIN pg_catalog.pg_description ds ON ds.objoid = co.oid
	AND ds.classoid = 'pg_catalog.pg_collation'::pg_catalog.regclass AND ds.objsubid = 0
WHERE true
-- PostgreSQL 18
SELECT co.oid, n.nspname::pg_catalog.text, co.collname::pg_catalog.text,
	pg_catalog.pg_get_userbyid(co.collowner)::pg_catalog.text,
	(pg_catalog.starts_with(n.nspname, 'pg_') OR n.nspname = 'information_schema'), COALESCE(ext.extname::pg_catalog.text, ''),
	co.collprovider::pg_catalog.text, co.collisdeterministic,
	pg_catalog.pg_encoding_to_char(co.collencoding)::pg_catalog.text,
	COALESCE(co.collcollate, ''), COALESCE(co.collctype, ''),
	COALESCE(co.colllocale, ''),
	COALESCE(co.collicurules, ''),
	COALESCE(co.collversion, ''),
	COALESCE(ds.description, '')
FROM pg_catalog.pg_collation co
JOIN pg_catalog.pg_namespace n ON n.oid = co.collnamespace
LEFT JOIN pg_catalog.pg_depend d ON d.classid = 'pg_catalog.pg_collation'::pg_catalog.regclass AND d.objid = co.oid AND d.objsubid = 0
			AND d.refclassid = 'pg_catalog.pg_extension'::pg_catalog.regclass AND d.deptype = 'e'
LEFT JOIN pg_catalog.pg_extension ext ON ext.oid = d.refobjid
LEFT JOIN pg_catalog.pg_description ds ON ds.objoid = co.oid
	AND ds.classoid = 'pg_catalog.pg_collation'::pg_catalog.regclass AND ds.objsubid = 0
WHERE true
