// Package pgsql is pgmo's PostgreSQL text layer: quoting identifiers and
// literals, keeping names from breaking out of comments, and splitting
// script text into the runs the server lexes differently. It imports only
// the standard library, so a consumer that builds or reads PostgreSQL text
// without connecting to a server need not link pgx and its dependencies, as
// importing pgmo does. It is gosmo's tsql package for PostgreSQL.
//
// pgmo calls it for all of these. TestImportsOnlyStdlib keeps the package a
// leaf.
//
// The rules are the server's own: QuoteIdent and QuoteLiteral produce what
// PostgreSQL's quote_ident and quote_literal do, and Spans follows the
// server's lexer (src/backend/parser/scan.l), with standard_conforming_strings
// on — the default since PostgreSQL 9.1, and the only setting pgmo supports.
package pgsql
