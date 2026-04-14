package store

import "database/sql"

// ColumnMigration is a single ALTER TABLE ADD COLUMN statement used by
// dialects that need to evolve older databases to the current schema.
type ColumnMigration struct {
	SQL  string // full ALTER TABLE ... ADD COLUMN statement
	Desc string // short label for error messages
}

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

	// FTSSearchClause returns SQL fragments for full-text search using ?
	// placeholders consistently. The caller passes the search query string as
	// an argument queryArgCount times — once if it appears only in WHERE,
	// twice if it also appears in ORDER BY (as with PostgreSQL's ts_rank).
	// The full query string (including the returned fragments) must be
	// rebound via Dialect.Rebind() before execution so the ? placeholders
	// become $N on PostgreSQL.
	//
	// Returns: join clause, where clause, order-by clause, number of times
	// the search query must appear in args.
	//
	// SQLite: uses messages_fts virtual table with JOIN/MATCH/rank; the
	// search term appears only in WHERE (queryArgCount=1).
	// PostgreSQL: uses tsvector column with @@/ts_rank (no JOIN needed); the
	// search term appears in both WHERE and ORDER BY (queryArgCount=2).
	FTSSearchClause() (join, where, orderBy string, queryArgCount int)

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

	// FTSSearchExpression returns the SQL boolean expression for a full-text
	// search MATCH-style clause, where `?` is the placeholder for the search term.
	// SQLite: "messages_fts MATCH ?" (requires JOIN messages_fts fts ON fts.rowid = m.id).
	// PostgreSQL: "m.search_fts @@ plainto_tsquery('simple', ?)".
	// Not all engine code paths need this — FTSSearchClause() above handles
	// JOIN + WHERE + ORDER BY together for the store layer.
	FTSSearchExpression() string

	// TimeTruncExpression returns an expression that formats a timestamp column
	// for GROUP BY by the given granularity (year/month/day).
	// SQLite: strftime('%Y', col) etc.
	// PostgreSQL: to_char(col, 'YYYY') etc.
	TimeTruncExpression(column string, granularity string) string

	// HasFTSTableSQL returns the SQL to probe whether the FTS index is available.
	// Returns a single-row result: 1 if present, 0 if absent.
	HasFTSTableSQL() string

	// JSONPlaceholder returns the placeholder expression for a JSON-typed column.
	// SQLite: "?" (JSON is stored as TEXT)
	// PostgreSQL: "?::jsonb" (explicit cast required when the column type is JSONB)
	// Callers substitute this for the bare "?" in UPDATE/INSERT statements that
	// write JSON strings into JSONB columns.
	JSONPlaceholder() string

	// Connection lifecycle

	// InitConn performs driver-specific connection initialization.
	// Called after opening a connection. For SQLite: no-op (PRAGMAs are set via
	// DSN parameters). For PostgreSQL: SET search_path, statement_timeout, etc.
	InitConn(db *sql.DB) error

	// SchemaFiles returns the filenames of embedded schema files to execute during InitSchema.
	SchemaFiles() []string

	// LegacyColumnMigrations returns ALTER TABLE statements to bring older
	// databases up to date with schema columns added over time.
	// For SQLite, this is a list of ADD COLUMN statements that no-op via
	// IsDuplicateColumnError when already applied. For PostgreSQL, this returns
	// an empty slice because schema_pg.sql is always the complete, current schema.
	LegacyColumnMigrations() []ColumnMigration

	// CheckpointWAL checkpoints the WAL (SQLite) or is a no-op (PostgreSQL).
	CheckpointWAL(db *sql.DB) error

	// DatabaseSize returns the on-disk size of the database in bytes.
	// For SQLite, this is the file size at dbPath (or 0 if dbPath isn't a file).
	// For PostgreSQL, this queries pg_database_size().
	// Returns 0 if the size cannot be determined; an error only for genuine failures.
	DatabaseSize(db *sql.DB, dbPath string) (int64, error)

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
