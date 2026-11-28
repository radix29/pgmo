package pgmo

// ============================================================
// Object addresses
// ============================================================
//
// Every family handle names itself as pg_depend does, for
// Database.Dependencies and Dependents (Server's for a role, a database or
// a tablespace), as every handle gives its own Securable(). A Ref's OID is
// zero — it was never read — so its address is refused by those reads
// (ErrHandleNotLoaded): read the object by name first.

// Address is the database's address: pg_database, its OID.
func (d *Database) Address() ObjectAddress { return ObjectAddress{Catalog: "pg_database", OID: d.OID} }

// Address is the role's address: pg_authid, its OID.
func (r *Role) Address() ObjectAddress { return ObjectAddress{Catalog: "pg_authid", OID: r.OID} }

// Address is the tablespace's address: pg_tablespace, its OID.
func (t *Tablespace) Address() ObjectAddress {
	return ObjectAddress{Catalog: "pg_tablespace", OID: t.OID}
}

// Address is the schema's address: pg_namespace, its OID.
func (s *Schema) Address() ObjectAddress { return ObjectAddress{Catalog: "pg_namespace", OID: s.OID} }

// Address is the table's address: pg_class, its OID.
func (t *Table) Address() ObjectAddress { return ObjectAddress{Catalog: "pg_class", OID: t.OID} }

// Address is the view's address: pg_class, its OID.
func (v *View) Address() ObjectAddress { return ObjectAddress{Catalog: "pg_class", OID: v.OID} }

// Address is the materialized view's address: pg_class, its OID.
func (m *MaterializedView) Address() ObjectAddress {
	return ObjectAddress{Catalog: "pg_class", OID: m.OID}
}

// Address is the foreign table's address: pg_class, its OID.
func (f *ForeignTable) Address() ObjectAddress { return ObjectAddress{Catalog: "pg_class", OID: f.OID} }

// Address is the sequence's address: pg_class, its OID.
func (q *Sequence) Address() ObjectAddress { return ObjectAddress{Catalog: "pg_class", OID: q.OID} }

// Address is the index's address: pg_class, its OID.
func (i *Index) Address() ObjectAddress { return ObjectAddress{Catalog: "pg_class", OID: i.OID} }

// Address is the constraint's address: pg_constraint, its OID.
func (c *Constraint) Address() ObjectAddress {
	return ObjectAddress{Catalog: "pg_constraint", OID: c.OID}
}

// Address is the function's address: pg_proc, its OID.
func (f *Function) Address() ObjectAddress { return ObjectAddress{Catalog: "pg_proc", OID: f.OID} }

// Address is the procedure's address: pg_proc, its OID.
func (p *Procedure) Address() ObjectAddress { return ObjectAddress{Catalog: "pg_proc", OID: p.OID} }

// Address is the aggregate's address: pg_proc, its OID.
func (a *Aggregate) Address() ObjectAddress { return ObjectAddress{Catalog: "pg_proc", OID: a.OID} }

// Address is the type's address: pg_type, its OID.
func (t *Type) Address() ObjectAddress { return ObjectAddress{Catalog: "pg_type", OID: t.OID} }

// Address is the domain's address: pg_type, its OID.
func (m *Domain) Address() ObjectAddress { return ObjectAddress{Catalog: "pg_type", OID: m.OID} }

// Address is the trigger's address: pg_trigger, its OID.
func (t *Trigger) Address() ObjectAddress { return ObjectAddress{Catalog: "pg_trigger", OID: t.OID} }

// Address is the event trigger's address: pg_event_trigger, its OID.
func (e *EventTrigger) Address() ObjectAddress {
	return ObjectAddress{Catalog: "pg_event_trigger", OID: e.OID}
}

// Address is the policy's address: pg_policy, its OID.
func (p *Policy) Address() ObjectAddress { return ObjectAddress{Catalog: "pg_policy", OID: p.OID} }

// Address is the extension's address: pg_extension, its OID.
func (e *Extension) Address() ObjectAddress {
	return ObjectAddress{Catalog: "pg_extension", OID: e.OID}
}

// Address is the collation's address: pg_collation, its OID.
func (c *Collation) Address() ObjectAddress {
	return ObjectAddress{Catalog: "pg_collation", OID: c.OID}
}

// Address is the text search parser's address: pg_ts_parser, its OID.
func (p *TextSearchParser) Address() ObjectAddress {
	return ObjectAddress{Catalog: "pg_ts_parser", OID: p.OID}
}

// Address is the text search template's address: pg_ts_template, its OID.
func (t *TextSearchTemplate) Address() ObjectAddress {
	return ObjectAddress{Catalog: "pg_ts_template", OID: t.OID}
}

// Address is the text search dictionary's address: pg_ts_dict, its OID.
func (d *TextSearchDictionary) Address() ObjectAddress {
	return ObjectAddress{Catalog: "pg_ts_dict", OID: d.OID}
}

// Address is the text search configuration's address: pg_ts_config, its OID.
func (c *TextSearchConfiguration) Address() ObjectAddress {
	return ObjectAddress{Catalog: "pg_ts_config", OID: c.OID}
}

// Address is the language's address: pg_language, its OID.
func (l *Language) Address() ObjectAddress { return ObjectAddress{Catalog: "pg_language", OID: l.OID} }

// Address is the cast's address: pg_cast, its OID.
func (c *Cast) Address() ObjectAddress { return ObjectAddress{Catalog: "pg_cast", OID: c.OID} }

// Address is the operator's address: pg_operator, its OID.
func (o *Operator) Address() ObjectAddress { return ObjectAddress{Catalog: "pg_operator", OID: o.OID} }

// Address is the foreign-data wrapper's address: pg_foreign_data_wrapper, its OID.
func (w *ForeignDataWrapper) Address() ObjectAddress {
	return ObjectAddress{Catalog: "pg_foreign_data_wrapper", OID: w.OID}
}

// Address is the foreign server's address: pg_foreign_server, its OID.
func (s *ForeignServer) Address() ObjectAddress {
	return ObjectAddress{Catalog: "pg_foreign_server", OID: s.OID}
}

// Address is the user mapping's address: pg_user_mapping, its OID.
func (u *UserMapping) Address() ObjectAddress {
	return ObjectAddress{Catalog: "pg_user_mapping", OID: u.OID}
}

// Address is the member's address, to read its dependencies.
func (m ExtensionMember) Address() ObjectAddress {
	return ObjectAddress{Catalog: m.Catalog, OID: m.OID}
}
