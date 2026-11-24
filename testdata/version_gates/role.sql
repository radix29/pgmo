-- PostgreSQL 15
SELECT r.oid, r.rolname::pg_catalog.text, r.rolsuper, r.rolinherit, r.rolcreaterole,
	r.rolcreatedb, r.rolcanlogin, r.rolreplication, r.rolbypassrls, r.rolconnlimit,
	r.rolvaliduntil, r.rolconfig,
	ds.dbs, ds.entries,
	mo.roles, mo.grantors, mo.admins, mo.inherits, mo.sets,
	mb.members, mb.grantors, mb.admins, mb.inherits, mb.sets,
	COALESCE(pg_catalog.shobj_description(r.oid, 'pg_authid'), '')
FROM pg_catalog.pg_roles r
LEFT JOIN LATERAL (SELECT pg_catalog.array_agg(d.datname::pg_catalog.text ORDER BY d.datname, c.n),
		pg_catalog.array_agg(c.entry ORDER BY d.datname, c.n)
	FROM pg_catalog.pg_db_role_setting s
	JOIN pg_catalog.pg_database d ON d.oid = s.setdatabase
	CROSS JOIN LATERAL pg_catalog.unnest(s.setconfig) WITH ORDINALITY AS c(entry, n)
	WHERE s.setrole = r.oid) AS ds(dbs, entries) ON true
LEFT JOIN LATERAL (SELECT pg_catalog.array_agg(g.rolname::pg_catalog.text ORDER BY g.rolname, m.grantor),
		pg_catalog.array_agg(COALESCE(gr.rolname::pg_catalog.text, '') ORDER BY g.rolname, m.grantor),
		pg_catalog.array_agg(m.admin_option ORDER BY g.rolname, m.grantor),
		pg_catalog.array_agg(r.rolinherit ORDER BY g.rolname, m.grantor),
		pg_catalog.array_agg(true ORDER BY g.rolname, m.grantor)
	FROM pg_catalog.pg_auth_members m
	JOIN pg_catalog.pg_roles g ON g.oid = m.roleid
	LEFT JOIN pg_catalog.pg_roles gr ON gr.oid = m.grantor
	WHERE m.member = r.oid) AS mo(roles, grantors, admins, inherits, sets) ON true
LEFT JOIN LATERAL (SELECT pg_catalog.array_agg(u.rolname::pg_catalog.text ORDER BY u.rolname, m.grantor),
		pg_catalog.array_agg(COALESCE(gr.rolname::pg_catalog.text, '') ORDER BY u.rolname, m.grantor),
		pg_catalog.array_agg(m.admin_option ORDER BY u.rolname, m.grantor),
		pg_catalog.array_agg(u.rolinherit ORDER BY u.rolname, m.grantor),
		pg_catalog.array_agg(true ORDER BY u.rolname, m.grantor)
	FROM pg_catalog.pg_auth_members m
	JOIN pg_catalog.pg_roles u ON u.oid = m.member
	LEFT JOIN pg_catalog.pg_roles gr ON gr.oid = m.grantor
	WHERE m.roleid = r.oid) AS mb(members, grantors, admins, inherits, sets) ON true
WHERE true
-- PostgreSQL 16
SELECT r.oid, r.rolname::pg_catalog.text, r.rolsuper, r.rolinherit, r.rolcreaterole,
	r.rolcreatedb, r.rolcanlogin, r.rolreplication, r.rolbypassrls, r.rolconnlimit,
	r.rolvaliduntil, r.rolconfig,
	ds.dbs, ds.entries,
	mo.roles, mo.grantors, mo.admins, mo.inherits, mo.sets,
	mb.members, mb.grantors, mb.admins, mb.inherits, mb.sets,
	COALESCE(pg_catalog.shobj_description(r.oid, 'pg_authid'), '')
FROM pg_catalog.pg_roles r
LEFT JOIN LATERAL (SELECT pg_catalog.array_agg(d.datname::pg_catalog.text ORDER BY d.datname, c.n),
		pg_catalog.array_agg(c.entry ORDER BY d.datname, c.n)
	FROM pg_catalog.pg_db_role_setting s
	JOIN pg_catalog.pg_database d ON d.oid = s.setdatabase
	CROSS JOIN LATERAL pg_catalog.unnest(s.setconfig) WITH ORDINALITY AS c(entry, n)
	WHERE s.setrole = r.oid) AS ds(dbs, entries) ON true
LEFT JOIN LATERAL (SELECT pg_catalog.array_agg(g.rolname::pg_catalog.text ORDER BY g.rolname, m.grantor),
		pg_catalog.array_agg(COALESCE(gr.rolname::pg_catalog.text, '') ORDER BY g.rolname, m.grantor),
		pg_catalog.array_agg(m.admin_option ORDER BY g.rolname, m.grantor),
		pg_catalog.array_agg(m.inherit_option ORDER BY g.rolname, m.grantor),
		pg_catalog.array_agg(m.set_option ORDER BY g.rolname, m.grantor)
	FROM pg_catalog.pg_auth_members m
	JOIN pg_catalog.pg_roles g ON g.oid = m.roleid
	LEFT JOIN pg_catalog.pg_roles gr ON gr.oid = m.grantor
	WHERE m.member = r.oid) AS mo(roles, grantors, admins, inherits, sets) ON true
LEFT JOIN LATERAL (SELECT pg_catalog.array_agg(u.rolname::pg_catalog.text ORDER BY u.rolname, m.grantor),
		pg_catalog.array_agg(COALESCE(gr.rolname::pg_catalog.text, '') ORDER BY u.rolname, m.grantor),
		pg_catalog.array_agg(m.admin_option ORDER BY u.rolname, m.grantor),
		pg_catalog.array_agg(m.inherit_option ORDER BY u.rolname, m.grantor),
		pg_catalog.array_agg(m.set_option ORDER BY u.rolname, m.grantor)
	FROM pg_catalog.pg_auth_members m
	JOIN pg_catalog.pg_roles u ON u.oid = m.member
	LEFT JOIN pg_catalog.pg_roles gr ON gr.oid = m.grantor
	WHERE m.roleid = r.oid) AS mb(members, grantors, admins, inherits, sets) ON true
WHERE true
-- PostgreSQL 17
SELECT r.oid, r.rolname::pg_catalog.text, r.rolsuper, r.rolinherit, r.rolcreaterole,
	r.rolcreatedb, r.rolcanlogin, r.rolreplication, r.rolbypassrls, r.rolconnlimit,
	r.rolvaliduntil, r.rolconfig,
	ds.dbs, ds.entries,
	mo.roles, mo.grantors, mo.admins, mo.inherits, mo.sets,
	mb.members, mb.grantors, mb.admins, mb.inherits, mb.sets,
	COALESCE(pg_catalog.shobj_description(r.oid, 'pg_authid'), '')
FROM pg_catalog.pg_roles r
LEFT JOIN LATERAL (SELECT pg_catalog.array_agg(d.datname::pg_catalog.text ORDER BY d.datname, c.n),
		pg_catalog.array_agg(c.entry ORDER BY d.datname, c.n)
	FROM pg_catalog.pg_db_role_setting s
	JOIN pg_catalog.pg_database d ON d.oid = s.setdatabase
	CROSS JOIN LATERAL pg_catalog.unnest(s.setconfig) WITH ORDINALITY AS c(entry, n)
	WHERE s.setrole = r.oid) AS ds(dbs, entries) ON true
LEFT JOIN LATERAL (SELECT pg_catalog.array_agg(g.rolname::pg_catalog.text ORDER BY g.rolname, m.grantor),
		pg_catalog.array_agg(COALESCE(gr.rolname::pg_catalog.text, '') ORDER BY g.rolname, m.grantor),
		pg_catalog.array_agg(m.admin_option ORDER BY g.rolname, m.grantor),
		pg_catalog.array_agg(m.inherit_option ORDER BY g.rolname, m.grantor),
		pg_catalog.array_agg(m.set_option ORDER BY g.rolname, m.grantor)
	FROM pg_catalog.pg_auth_members m
	JOIN pg_catalog.pg_roles g ON g.oid = m.roleid
	LEFT JOIN pg_catalog.pg_roles gr ON gr.oid = m.grantor
	WHERE m.member = r.oid) AS mo(roles, grantors, admins, inherits, sets) ON true
LEFT JOIN LATERAL (SELECT pg_catalog.array_agg(u.rolname::pg_catalog.text ORDER BY u.rolname, m.grantor),
		pg_catalog.array_agg(COALESCE(gr.rolname::pg_catalog.text, '') ORDER BY u.rolname, m.grantor),
		pg_catalog.array_agg(m.admin_option ORDER BY u.rolname, m.grantor),
		pg_catalog.array_agg(m.inherit_option ORDER BY u.rolname, m.grantor),
		pg_catalog.array_agg(m.set_option ORDER BY u.rolname, m.grantor)
	FROM pg_catalog.pg_auth_members m
	JOIN pg_catalog.pg_roles u ON u.oid = m.member
	LEFT JOIN pg_catalog.pg_roles gr ON gr.oid = m.grantor
	WHERE m.roleid = r.oid) AS mb(members, grantors, admins, inherits, sets) ON true
WHERE true
-- PostgreSQL 18
SELECT r.oid, r.rolname::pg_catalog.text, r.rolsuper, r.rolinherit, r.rolcreaterole,
	r.rolcreatedb, r.rolcanlogin, r.rolreplication, r.rolbypassrls, r.rolconnlimit,
	r.rolvaliduntil, r.rolconfig,
	ds.dbs, ds.entries,
	mo.roles, mo.grantors, mo.admins, mo.inherits, mo.sets,
	mb.members, mb.grantors, mb.admins, mb.inherits, mb.sets,
	COALESCE(pg_catalog.shobj_description(r.oid, 'pg_authid'), '')
FROM pg_catalog.pg_roles r
LEFT JOIN LATERAL (SELECT pg_catalog.array_agg(d.datname::pg_catalog.text ORDER BY d.datname, c.n),
		pg_catalog.array_agg(c.entry ORDER BY d.datname, c.n)
	FROM pg_catalog.pg_db_role_setting s
	JOIN pg_catalog.pg_database d ON d.oid = s.setdatabase
	CROSS JOIN LATERAL pg_catalog.unnest(s.setconfig) WITH ORDINALITY AS c(entry, n)
	WHERE s.setrole = r.oid) AS ds(dbs, entries) ON true
LEFT JOIN LATERAL (SELECT pg_catalog.array_agg(g.rolname::pg_catalog.text ORDER BY g.rolname, m.grantor),
		pg_catalog.array_agg(COALESCE(gr.rolname::pg_catalog.text, '') ORDER BY g.rolname, m.grantor),
		pg_catalog.array_agg(m.admin_option ORDER BY g.rolname, m.grantor),
		pg_catalog.array_agg(m.inherit_option ORDER BY g.rolname, m.grantor),
		pg_catalog.array_agg(m.set_option ORDER BY g.rolname, m.grantor)
	FROM pg_catalog.pg_auth_members m
	JOIN pg_catalog.pg_roles g ON g.oid = m.roleid
	LEFT JOIN pg_catalog.pg_roles gr ON gr.oid = m.grantor
	WHERE m.member = r.oid) AS mo(roles, grantors, admins, inherits, sets) ON true
LEFT JOIN LATERAL (SELECT pg_catalog.array_agg(u.rolname::pg_catalog.text ORDER BY u.rolname, m.grantor),
		pg_catalog.array_agg(COALESCE(gr.rolname::pg_catalog.text, '') ORDER BY u.rolname, m.grantor),
		pg_catalog.array_agg(m.admin_option ORDER BY u.rolname, m.grantor),
		pg_catalog.array_agg(m.inherit_option ORDER BY u.rolname, m.grantor),
		pg_catalog.array_agg(m.set_option ORDER BY u.rolname, m.grantor)
	FROM pg_catalog.pg_auth_members m
	JOIN pg_catalog.pg_roles u ON u.oid = m.member
	LEFT JOIN pg_catalog.pg_roles gr ON gr.oid = m.grantor
	WHERE m.roleid = r.oid) AS mb(members, grantors, admins, inherits, sets) ON true
WHERE true
