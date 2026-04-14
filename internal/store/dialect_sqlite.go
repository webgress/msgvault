package store

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/mattn/go-sqlite3"
)

// SQLiteDialect implements Dialect for SQLite (the default backend).
type SQLiteDialect struct{}

func (d *SQLiteDialect) DriverName() string { return "sqlite3" }

// Rebind is a no-op for SQLite — it uses ? placeholders natively.
func (d *SQLiteDialect) Rebind(query string) string { return query }

// Now returns the SQLite expression for the current UTC timestamp.
func (d *SQLiteDialect) Now() string { return "datetime('now')" }

// InsertOrIgnore is a no-op for SQLite — the syntax is native.
func (d *SQLiteDialect) InsertOrIgnore(sql string) string { return sql }

// InsertOrIgnoreSuffix returns "" for SQLite — OR IGNORE is in the statement prefix.
func (d *SQLiteDialect) InsertOrIgnoreSuffix() string { return "" }

// UpdateOrIgnore is a no-op for SQLite — the syntax is native.
func (d *SQLiteDialect) UpdateOrIgnore(sql string) string { return sql }

// FTSUpsertSQL returns the SQL to upsert an FTS5 row.
// Parameters (6): messageID, subject, bodyText, fromAddr, toAddrs, ccAddrs.
// Uses SQLite's numbered placeholders (?1) so messageID can serve as both
// rowid and message_id without passing it twice from Go.
func (d *SQLiteDialect) FTSUpsertSQL() string {
	return `INSERT OR REPLACE INTO messages_fts(rowid, message_id, subject, body, from_addr, to_addr, cc_addr)
		VALUES (?1, ?1, ?2, ?3, ?4, ?5, ?6)`
}

// FTSSearchClause returns SQL fragments for FTS5 full-text search.
// The search term appears only in the WHERE clause (queryArgCount=1).
// The MATCH ordering uses the built-in "rank" pseudo-column (no parameter).
func (d *SQLiteDialect) FTSSearchClause() (join, where, orderBy string, queryArgCount int) {
	return "JOIN messages_fts fts ON fts.rowid = m.id",
		"messages_fts MATCH ?",
		"rank",
		1
}

// FTSDeleteSQL returns the SQL to delete a message's FTS5 entry.
func (d *SQLiteDialect) FTSDeleteSQL() string {
	return `DELETE FROM messages_fts WHERE message_id IN (
		SELECT id FROM messages WHERE source_id = ?
	)`
}

// FTSBackfillBatchSQL returns the SQL to backfill FTS5 for a range of message IDs.
// Parameters: fromID(?), toID(?)
func (d *SQLiteDialect) FTSBackfillBatchSQL() string {
	return `INSERT OR REPLACE INTO messages_fts (rowid, message_id, subject, body, from_addr, to_addr, cc_addr)
		SELECT m.id, m.id, COALESCE(m.subject, ''), COALESCE(mb.body_text, ''),
			COALESCE(
				CASE WHEN m.message_type != 'email' AND m.message_type IS NOT NULL AND m.message_type != ''
				     THEN (SELECT COALESCE(p.phone_number, p.email_address) FROM participants p WHERE p.id = m.sender_id)
				END,
				(SELECT GROUP_CONCAT(p.email_address, ' ') FROM message_recipients mr JOIN participants p ON p.id = mr.participant_id WHERE mr.message_id = m.id AND mr.recipient_type = 'from'),
				''
			),
			COALESCE((SELECT GROUP_CONCAT(p.email_address, ' ') FROM message_recipients mr JOIN participants p ON p.id = mr.participant_id WHERE mr.message_id = m.id AND mr.recipient_type = 'to'), ''),
			COALESCE((SELECT GROUP_CONCAT(p.email_address, ' ') FROM message_recipients mr JOIN participants p ON p.id = mr.participant_id WHERE mr.message_id = m.id AND mr.recipient_type = 'cc'), '')
		FROM messages m
		LEFT JOIN message_bodies mb ON mb.message_id = m.id
		WHERE m.id >= ? AND m.id < ?`
}

// FTSAvailable probes for FTS5 by querying the virtual table.
// Checking sqlite_master alone is insufficient: a binary built without FTS5
// support will fail with "no such module: fts5" even if the table exists.
func (d *SQLiteDialect) FTSAvailable(db *sql.DB) (bool, error) {
	var probe int
	err := db.QueryRow("SELECT 1 FROM messages_fts LIMIT 1").Scan(&probe)
	if err == nil || err == sql.ErrNoRows {
		return true, nil
	}
	return false, nil
}

// FTSNeedsBackfill reports whether the FTS5 table needs population.
// Uses MAX(id) comparisons (instant B-tree lookups) instead of COUNT(*).
func (d *SQLiteDialect) FTSNeedsBackfill(db *sql.DB) (bool, error) {
	var msgMax int64
	if err := db.QueryRow("SELECT COALESCE(MAX(id), 0) FROM messages").Scan(&msgMax); err != nil || msgMax == 0 {
		return false, nil
	}
	var ftsMax int64
	if err := db.QueryRow("SELECT COALESCE(MAX(rowid), 0) FROM messages_fts").Scan(&ftsMax); err != nil {
		return false, nil
	}
	return ftsMax < msgMax-msgMax/10, nil
}

// FTSClearSQL returns the SQL to clear all FTS5 data.
func (d *SQLiteDialect) FTSClearSQL() string {
	return "DELETE FROM messages_fts"
}

// SchemaFTS returns the embedded filename containing FTS5 virtual table DDL.
func (d *SQLiteDialect) SchemaFTS() string {
	return "schema_sqlite.sql"
}

// InitConn is a no-op for SQLite — PRAGMAs are set via DSN parameters.
func (d *SQLiteDialect) InitConn(db *sql.DB) error { return nil }

// SchemaFiles returns the schema files to execute during InitSchema.
func (d *SQLiteDialect) SchemaFiles() []string {
	return []string{"schema.sql"}
}

// LegacyColumnMigrations returns the ALTER TABLE ADD COLUMN statements that
// bring older SQLite databases up to date. Each ADD COLUMN returns
// "duplicate column name" if already applied, which callers treat as success.
func (d *SQLiteDialect) LegacyColumnMigrations() []ColumnMigration {
	return []ColumnMigration{
		{`ALTER TABLE sources ADD COLUMN sync_config JSON`, "sync_config"},
		{`ALTER TABLE messages ADD COLUMN rfc822_message_id TEXT`, "rfc822_message_id"},
		{`ALTER TABLE sources ADD COLUMN oauth_app TEXT`, "oauth_app"},
		{`ALTER TABLE participants ADD COLUMN phone_number TEXT`, "phone_number"},
		{`ALTER TABLE participants ADD COLUMN canonical_id TEXT`, "canonical_id"},
		{`ALTER TABLE messages ADD COLUMN sender_id INTEGER REFERENCES participants(id)`, "sender_id"},
		{`ALTER TABLE messages ADD COLUMN message_type TEXT NOT NULL DEFAULT 'email'`, "message_type"},
		{`ALTER TABLE messages ADD COLUMN attachment_count INTEGER DEFAULT 0`, "attachment_count"},
		{`ALTER TABLE messages ADD COLUMN deleted_from_source_at DATETIME`, "deleted_from_source_at"},
		{`ALTER TABLE messages ADD COLUMN delete_batch_id TEXT`, "delete_batch_id"},
		{`ALTER TABLE conversations ADD COLUMN title TEXT`, "title"},
		{`ALTER TABLE conversations ADD COLUMN conversation_type TEXT NOT NULL DEFAULT 'email_thread'`, "conversation_type"},
	}
}

// DatabaseSize returns the on-disk size of the SQLite database file.
func (d *SQLiteDialect) DatabaseSize(db *sql.DB, dbPath string) (int64, error) {
	info, err := os.Stat(dbPath)
	if err != nil {
		// For in-memory or missing databases, return 0 silently.
		return 0, nil
	}
	return info.Size(), nil
}

// CheckpointWAL forces a WAL checkpoint using TRUNCATE mode.
func (d *SQLiteDialect) CheckpointWAL(db *sql.DB) error {
	var busy, log, checkpointed int
	err := db.QueryRow("PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy, &log, &checkpointed)
	if err != nil {
		return err
	}
	if busy != 0 {
		return fmt.Errorf(
			"WAL checkpoint incomplete: database busy "+
				"(log=%d, checkpointed=%d)", log, checkpointed,
		)
	}
	return nil
}

// SchemaStaleCheck returns the SQL to check whether the most recent migration column exists.
func (d *SQLiteDialect) SchemaStaleCheck() string {
	return "SELECT COUNT(*) FROM pragma_table_info('conversations') WHERE name = 'conversation_type'"
}

// IsDuplicateColumnError returns true if the error is "duplicate column name" from ALTER TABLE.
func (d *SQLiteDialect) IsDuplicateColumnError(err error) bool {
	return isSQLiteErrorMatch(err, "duplicate column name")
}

// IsConflictError returns true if the error is a UNIQUE constraint violation.
func (d *SQLiteDialect) IsConflictError(err error) bool {
	return isSQLiteErrorMatch(err, "UNIQUE constraint failed")
}

// IsNoSuchTableError returns true if the error indicates a missing table.
func (d *SQLiteDialect) IsNoSuchTableError(err error) bool {
	return isSQLiteErrorMatch(err, "no such table")
}

// IsNoSuchModuleError returns true if the error indicates a missing module (e.g., fts5).
func (d *SQLiteDialect) IsNoSuchModuleError(err error) bool {
	return isSQLiteErrorMatch(err, "no such module: fts5")
}

// IsReturningError returns true if the error indicates RETURNING is not supported.
func (d *SQLiteDialect) IsReturningError(err error) bool {
	return isSQLiteErrorMatch(err, "RETURNING")
}

// isSQLiteErrorMatch checks if err is a sqlite3.Error with a message containing substr.
// Handles both value (sqlite3.Error) and pointer (*sqlite3.Error) forms.
func isSQLiteErrorMatch(err error, substr string) bool {
	var sqliteErr sqlite3.Error
	if errors.As(err, &sqliteErr) {
		return strings.Contains(sqliteErr.Error(), substr)
	}
	var sqliteErrPtr *sqlite3.Error
	if errors.As(err, &sqliteErrPtr) && sqliteErrPtr != nil {
		return strings.Contains(sqliteErrPtr.Error(), substr)
	}
	return false
}
