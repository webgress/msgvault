package store

import "database/sql"

// Dialect abstracts database-specific SQL generation and behavior.
// Implementations exist for SQLite (default) and PostgreSQL (opt-in).
type Dialect interface {
	// DriverName returns the database/sql driver name ("sqlite3" or "pgx").
	DriverName() string

	// Rebind converts a query with ? placeholders to the appropriate format
	// for the database driver. No-op for SQLite; converts to $1, $2, ... for PostgreSQL.
	Rebind(query string) string

	// Now returns the SQL expression for the current timestamp.
	// SQLite: "datetime('now')"  PostgreSQL: "NOW()"
	Now() string

	// InsertOrIgnore rewrites a complete INSERT statement to silently ignore conflicts.
	// SQLite: INSERT OR IGNORE INTO ...  PostgreSQL: INSERT INTO ... ON CONFLICT DO NOTHING
	// The input sql must be a complete statement in SQLite form
	// (starting with "INSERT OR IGNORE INTO").
	InsertOrIgnore(sql string) string

	// InsertOrIgnoreSuffix returns a SQL suffix to append after VALUES for
	// conflict-ignoring inserts built incrementally (e.g., by insertInChunks).
	// SQLite: "" (OR IGNORE is in the prefix)
	// PostgreSQL: " ON CONFLICT DO NOTHING"
	InsertOrIgnoreSuffix() string

	// UpdateOrIgnore rewrites an UPDATE statement to silently ignore constraint violations.
	// SQLite: UPDATE OR IGNORE ...  PostgreSQL: requires a different approach.
	// The input sql must start with "UPDATE OR IGNORE " (SQLite form).
	UpdateOrIgnore(sql string) string

	// Full-text search

	// FTSUpsertSQL returns the SQL to insert or update the search index for one message.
	// Parameters: messageID, subject, bodyText, fromAddr, toAddrs, ccAddrs
	FTSUpsertSQL() string

	// FTSSearchClause returns SQL fragments for full-text search.
	// paramIndex is the placeholder number for the search query parameter (1-based).
	// Returns: join clause, where clause, order-by clause.
	// For SQLite: uses messages_fts virtual table with JOIN/MATCH/rank.
	// For PostgreSQL: uses tsvector column with @@/ts_rank (no extra join needed).
	FTSSearchClause(paramIndex int) (join, where, orderBy string)

	// FTSDeleteSQL returns the SQL to remove FTS entries for messages belonging to
	// a given source. Takes one parameter: source_id.
	FTSDeleteSQL() string

	// FTSBackfillBatchSQL returns the SQL to populate the search index for a range of message IDs.
	// Uses two ? placeholders for the ID range: WHERE m.id >= ? AND m.id < ?
	FTSBackfillBatchSQL() string

	// FTSAvailable reports whether full-text search is available for this database.
	FTSAvailable(db *sql.DB) (bool, error)

	// FTSNeedsBackfill reports whether the FTS index needs to be populated.
	FTSNeedsBackfill(db *sql.DB) (bool, error)

	// FTSClearSQL returns the SQL to clear all FTS data before a full backfill.
	FTSClearSQL() string

	// SchemaFTS returns the embedded filename containing FTS DDL to execute during
	// schema initialization. Returns "" if no separate FTS schema file is needed
	// (e.g., PostgreSQL includes tsvector in its main schema).
	SchemaFTS() string

	// Connection lifecycle

	// InitConn performs driver-specific connection initialization.
	// Called after opening a connection. For SQLite: no-op (PRAGMAs are set via
	// DSN parameters). For PostgreSQL: SET search_path, statement_timeout, etc.
	InitConn(db *sql.DB) error

	// SchemaFiles returns the filenames of embedded schema files to execute during InitSchema.
	SchemaFiles() []string

	// CheckpointWAL checkpoints the WAL (SQLite) or is a no-op (PostgreSQL).
	CheckpointWAL(db *sql.DB) error

	// Schema migration

	// SchemaStaleCheck returns the SQL to check whether migrations are needed.
	SchemaStaleCheck() string

	// IsDuplicateColumnError returns true if the error indicates an ALTER TABLE
	// ADD COLUMN failed because the column already exists.
	IsDuplicateColumnError(err error) bool

	// Error handling

	// IsConflictError returns true if the error indicates a unique constraint violation.
	IsConflictError(err error) bool

	// IsNoSuchTableError returns true if the error indicates a missing table.
	IsNoSuchTableError(err error) bool

	// IsNoSuchModuleError returns true if the error indicates a missing module
	// (e.g., FTS5 not compiled in for SQLite). Always false for PostgreSQL.
	IsNoSuchModuleError(err error) bool

	// IsReturningError returns true if the error indicates RETURNING is not supported.
	// This handles SQLite < 3.35 which doesn't support RETURNING.
	// Always false for PostgreSQL (which always supports RETURNING).
	IsReturningError(err error) bool
}
