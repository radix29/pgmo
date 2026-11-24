-- PostgreSQL 15
SELECT d.oid, d.datname::pg_catalog.text,
	pg_catalog.pg_get_userbyid(d.datdba)::pg_catalog.text,
	pg_catalog.pg_encoding_to_char(d.encoding)::pg_catalog.text,
	d.datlocprovider::pg_catalog.text, d.datcollate, d.datctype,
	COALESCE(d.daticulocale, ''),
	COALESCE(NULL::pg_catalog.text, ''),
	COALESCE(d.datcollversion, ''),
	d.datistemplate, d.datallowconn, d.datconnlimit, t.spcname::pg_catalog.text,
	(SELECT s.setconfig FROM pg_catalog.pg_db_role_setting s WHERE s.setdatabase = d.oid AND s.setrole = 0),
	COALESCE(pg_catalog.shobj_description(d.oid, 'pg_database'), '')
FROM pg_catalog.pg_database d
JOIN pg_catalog.pg_tablespace t ON t.oid = d.dattablespace
WHERE true
-- PostgreSQL 16
SELECT d.oid, d.datname::pg_catalog.text,
	pg_catalog.pg_get_userbyid(d.datdba)::pg_catalog.text,
	pg_catalog.pg_encoding_to_char(d.encoding)::pg_catalog.text,
	d.datlocprovider::pg_catalog.text, d.datcollate, d.datctype,
	COALESCE(d.daticulocale, ''),
	COALESCE(d.daticurules, ''),
	COALESCE(d.datcollversion, ''),
	d.datistemplate, d.datallowconn, d.datconnlimit, t.spcname::pg_catalog.text,
	(SELECT s.setconfig FROM pg_catalog.pg_db_role_setting s WHERE s.setdatabase = d.oid AND s.setrole = 0),
	COALESCE(pg_catalog.shobj_description(d.oid, 'pg_database'), '')
FROM pg_catalog.pg_database d
JOIN pg_catalog.pg_tablespace t ON t.oid = d.dattablespace
WHERE true
-- PostgreSQL 17
SELECT d.oid, d.datname::pg_catalog.text,
	pg_catalog.pg_get_userbyid(d.datdba)::pg_catalog.text,
	pg_catalog.pg_encoding_to_char(d.encoding)::pg_catalog.text,
	d.datlocprovider::pg_catalog.text, d.datcollate, d.datctype,
	COALESCE(d.datlocale, ''),
	COALESCE(d.daticurules, ''),
	COALESCE(d.datcollversion, ''),
	d.datistemplate, d.datallowconn, d.datconnlimit, t.spcname::pg_catalog.text,
	(SELECT s.setconfig FROM pg_catalog.pg_db_role_setting s WHERE s.setdatabase = d.oid AND s.setrole = 0),
	COALESCE(pg_catalog.shobj_description(d.oid, 'pg_database'), '')
FROM pg_catalog.pg_database d
JOIN pg_catalog.pg_tablespace t ON t.oid = d.dattablespace
WHERE true
-- PostgreSQL 18
SELECT d.oid, d.datname::pg_catalog.text,
	pg_catalog.pg_get_userbyid(d.datdba)::pg_catalog.text,
	pg_catalog.pg_encoding_to_char(d.encoding)::pg_catalog.text,
	d.datlocprovider::pg_catalog.text, d.datcollate, d.datctype,
	COALESCE(d.datlocale, ''),
	COALESCE(d.daticurules, ''),
	COALESCE(d.datcollversion, ''),
	d.datistemplate, d.datallowconn, d.datconnlimit, t.spcname::pg_catalog.text,
	(SELECT s.setconfig FROM pg_catalog.pg_db_role_setting s WHERE s.setdatabase = d.oid AND s.setrole = 0),
	COALESCE(pg_catalog.shobj_description(d.oid, 'pg_database'), '')
FROM pg_catalog.pg_database d
JOIN pg_catalog.pg_tablespace t ON t.oid = d.dattablespace
WHERE true
