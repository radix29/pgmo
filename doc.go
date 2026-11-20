// Package pgmo is a library of PostgreSQL management objects: the PostgreSQL
// counterpart of gosmo (SQL Server Management Objects in Go). It connects to
// PostgreSQL 15 and later through pgx v5, used natively rather than through
// database/sql, and lets a program read and manage servers, databases,
// schemas, tables, roles, functions and the other catalog objects, script
// them as DDL, and read the server's monitoring views.
//
// The stdlib-only text layer (identifier and literal quoting, the SQL span
// scanner) is package [github.com/radix29/pgmo/pgsql]; the test kit for
// callers is package [github.com/radix29/pgmo/pgmotest]; pgmo's own version
// metadata is package [github.com/radix29/pgmo/version].
package pgmo
