// Package query - PostgreSQL query engine construction.
//
// PostgreSQL support is provided by the dialect-parameterized SQLiteEngine
// (see sqlite.go). NewPostgreSQLEngine constructs an engine configured for
// PostgreSQL SQL (tsvector FTS, to_char time truncation, $N placeholders).
// The underlying engine implementation is the same struct used for SQLite.
package query

import "database/sql"

// NewPostgreSQLEngine creates a query engine backed by PostgreSQL. The engine
// uses PostgreSQLQueryDialect for all SQL generation: $N placeholders via
// Rebind, to_char() time truncation, tsvector @@ for full-text search.
func NewPostgreSQLEngine(db *sql.DB) *SQLiteEngine {
	return NewEngineWithDialect(db, PostgreSQLQueryDialect{})
}

// NewEngine picks the appropriate engine for the given database. isPostgres
// selects between PostgreSQLQueryDialect (true) and SQLiteQueryDialect (false).
// This is the preferred entry point for callers that have a Store with an
// unknown backend — pass store.IsPostgres() as the flag.
func NewEngine(db *sql.DB, isPostgres bool) *SQLiteEngine {
	if isPostgres {
		return NewPostgreSQLEngine(db)
	}
	return NewSQLiteEngine(db)
}
