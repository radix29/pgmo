-- PostgreSQL 15
SELECT co.oid, co.conname::pg_catalog.text, co.contype::pg_catalog.text,
	pg_catalog.pg_get_constraintdef(co.oid),
	ARRAY(SELECT a.attname::pg_catalog.text FROM pg_catalog.unnest(co.conkey) WITH ORDINALITY AS k(num, n)
		JOIN pg_catalog.pg_attribute a ON a.attrelid = co.conrelid AND a.attnum = k.num ORDER BY k.n),
	co.condeferrable, co.condeferred, co.convalidated,
	true,
	co.conislocal, co.coninhcount::integer, co.connoinherit,
	COALESCE(ic.relname::pg_catalog.text, ''),
	COALESCE(fn.nspname::pg_catalog.text, ''), COALESCE(fc.relname::pg_catalog.text, ''),
	ARRAY(SELECT a.attname::pg_catalog.text FROM pg_catalog.unnest(co.confkey) WITH ORDINALITY AS k(num, n)
		JOIN pg_catalog.pg_attribute a ON a.attrelid = co.confrelid AND a.attnum = k.num ORDER BY k.n),
	CASE WHEN co.contype = 'f' THEN co.confupdtype::pg_catalog.text ELSE '' END,
	CASE WHEN co.contype = 'f' THEN co.confdeltype::pg_catalog.text ELSE '' END,
	CASE WHEN co.contype = 'f' THEN co.confmatchtype::pg_catalog.text ELSE '' END,
	COALESCE(pg_catalog.obj_description(co.oid, 'pg_constraint'), '')
FROM pg_catalog.pg_constraint co
LEFT JOIN pg_catalog.pg_class ic ON ic.oid = co.conindid AND co.contype IN ('p', 'u', 'x')
LEFT JOIN pg_catalog.pg_class fc ON fc.oid = co.confrelid
LEFT JOIN pg_catalog.pg_namespace fn ON fn.oid = fc.relnamespace
WHERE co.conrelid = $1 AND co.contype <> 't'
ORDER BY co.conname
-- PostgreSQL 16
SELECT co.oid, co.conname::pg_catalog.text, co.contype::pg_catalog.text,
	pg_catalog.pg_get_constraintdef(co.oid),
	ARRAY(SELECT a.attname::pg_catalog.text FROM pg_catalog.unnest(co.conkey) WITH ORDINALITY AS k(num, n)
		JOIN pg_catalog.pg_attribute a ON a.attrelid = co.conrelid AND a.attnum = k.num ORDER BY k.n),
	co.condeferrable, co.condeferred, co.convalidated,
	true,
	co.conislocal, co.coninhcount::integer, co.connoinherit,
	COALESCE(ic.relname::pg_catalog.text, ''),
	COALESCE(fn.nspname::pg_catalog.text, ''), COALESCE(fc.relname::pg_catalog.text, ''),
	ARRAY(SELECT a.attname::pg_catalog.text FROM pg_catalog.unnest(co.confkey) WITH ORDINALITY AS k(num, n)
		JOIN pg_catalog.pg_attribute a ON a.attrelid = co.confrelid AND a.attnum = k.num ORDER BY k.n),
	CASE WHEN co.contype = 'f' THEN co.confupdtype::pg_catalog.text ELSE '' END,
	CASE WHEN co.contype = 'f' THEN co.confdeltype::pg_catalog.text ELSE '' END,
	CASE WHEN co.contype = 'f' THEN co.confmatchtype::pg_catalog.text ELSE '' END,
	COALESCE(pg_catalog.obj_description(co.oid, 'pg_constraint'), '')
FROM pg_catalog.pg_constraint co
LEFT JOIN pg_catalog.pg_class ic ON ic.oid = co.conindid AND co.contype IN ('p', 'u', 'x')
LEFT JOIN pg_catalog.pg_class fc ON fc.oid = co.confrelid
LEFT JOIN pg_catalog.pg_namespace fn ON fn.oid = fc.relnamespace
WHERE co.conrelid = $1 AND co.contype <> 't'
ORDER BY co.conname
-- PostgreSQL 17
SELECT co.oid, co.conname::pg_catalog.text, co.contype::pg_catalog.text,
	pg_catalog.pg_get_constraintdef(co.oid),
	ARRAY(SELECT a.attname::pg_catalog.text FROM pg_catalog.unnest(co.conkey) WITH ORDINALITY AS k(num, n)
		JOIN pg_catalog.pg_attribute a ON a.attrelid = co.conrelid AND a.attnum = k.num ORDER BY k.n),
	co.condeferrable, co.condeferred, co.convalidated,
	true,
	co.conislocal, co.coninhcount::integer, co.connoinherit,
	COALESCE(ic.relname::pg_catalog.text, ''),
	COALESCE(fn.nspname::pg_catalog.text, ''), COALESCE(fc.relname::pg_catalog.text, ''),
	ARRAY(SELECT a.attname::pg_catalog.text FROM pg_catalog.unnest(co.confkey) WITH ORDINALITY AS k(num, n)
		JOIN pg_catalog.pg_attribute a ON a.attrelid = co.confrelid AND a.attnum = k.num ORDER BY k.n),
	CASE WHEN co.contype = 'f' THEN co.confupdtype::pg_catalog.text ELSE '' END,
	CASE WHEN co.contype = 'f' THEN co.confdeltype::pg_catalog.text ELSE '' END,
	CASE WHEN co.contype = 'f' THEN co.confmatchtype::pg_catalog.text ELSE '' END,
	COALESCE(pg_catalog.obj_description(co.oid, 'pg_constraint'), '')
FROM pg_catalog.pg_constraint co
LEFT JOIN pg_catalog.pg_class ic ON ic.oid = co.conindid AND co.contype IN ('p', 'u', 'x')
LEFT JOIN pg_catalog.pg_class fc ON fc.oid = co.confrelid
LEFT JOIN pg_catalog.pg_namespace fn ON fn.oid = fc.relnamespace
WHERE co.conrelid = $1 AND co.contype <> 't'
ORDER BY co.conname
-- PostgreSQL 18
SELECT co.oid, co.conname::pg_catalog.text, co.contype::pg_catalog.text,
	pg_catalog.pg_get_constraintdef(co.oid),
	ARRAY(SELECT a.attname::pg_catalog.text FROM pg_catalog.unnest(co.conkey) WITH ORDINALITY AS k(num, n)
		JOIN pg_catalog.pg_attribute a ON a.attrelid = co.conrelid AND a.attnum = k.num ORDER BY k.n),
	co.condeferrable, co.condeferred, co.convalidated,
	co.conenforced,
	co.conislocal, co.coninhcount::integer, co.connoinherit,
	COALESCE(ic.relname::pg_catalog.text, ''),
	COALESCE(fn.nspname::pg_catalog.text, ''), COALESCE(fc.relname::pg_catalog.text, ''),
	ARRAY(SELECT a.attname::pg_catalog.text FROM pg_catalog.unnest(co.confkey) WITH ORDINALITY AS k(num, n)
		JOIN pg_catalog.pg_attribute a ON a.attrelid = co.confrelid AND a.attnum = k.num ORDER BY k.n),
	CASE WHEN co.contype = 'f' THEN co.confupdtype::pg_catalog.text ELSE '' END,
	CASE WHEN co.contype = 'f' THEN co.confdeltype::pg_catalog.text ELSE '' END,
	CASE WHEN co.contype = 'f' THEN co.confmatchtype::pg_catalog.text ELSE '' END,
	COALESCE(pg_catalog.obj_description(co.oid, 'pg_constraint'), '')
FROM pg_catalog.pg_constraint co
LEFT JOIN pg_catalog.pg_class ic ON ic.oid = co.conindid AND co.contype IN ('p', 'u', 'x')
LEFT JOIN pg_catalog.pg_class fc ON fc.oid = co.confrelid
LEFT JOIN pg_catalog.pg_namespace fn ON fn.oid = fc.relnamespace
WHERE co.conrelid = $1 AND co.contype <> 't'
ORDER BY co.conname
