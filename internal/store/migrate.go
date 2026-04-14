package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// MigrateOptions configures a data migration between two Store instances.
type MigrateOptions struct {
	// BatchSize is the max number of rows bundled into a single multi-VALUES
	// INSERT. 0 picks a sensible default (200 rows, clamped per-table to stay
	// under the SQLite 999-parameter limit).
	BatchSize int

	// Progress, if non-nil, is called periodically with per-table progress.
	// Called at most once per flushed batch.
	Progress func(table string, rowsSoFar int64)

	// SkipFTSBackfill, when true, skips the post-migration FTS index rebuild.
	// Default is to rebuild the index on the destination so search works
	// immediately; set this to defer the backfill to a manual step.
	SkipFTSBackfill bool

	// AllowNonEmptyDestination bypasses the pre-flight check that refuses
	// to run when the destination has any rows in core tables. Use with
	// caution — mixing two datasets generally produces FK/PK conflicts.
	AllowNonEmptyDestination bool
}

// MigrateStats reports per-table row counts after a successful migration.
type MigrateStats struct {
	RowsByTable map[string]int64
	TotalRows   int64
	Elapsed     time.Duration
}

// Migrate copies all msgvault data from src to dst. Both stores must have
// their schemas initialized (call InitSchema on each before calling this).
//
// The two stores may use different dialects — SQLite→Postgres and
// Postgres→SQLite are both supported. All rows are copied with their original
// IDs preserved; after the copy, identity sequences on Postgres are reset
// so subsequent auto-generated IDs continue after the max migrated value.
//
// The copy happens inside a single transaction on the destination, so a
// failure rolls the destination back to its pre-migration state.
func Migrate(ctx context.Context, src, dst *Store, opts MigrateOptions) (*MigrateStats, error) {
	if src == nil || dst == nil {
		return nil, errors.New("migrate: src and dst must both be non-nil")
	}
	if src.dbPath == dst.dbPath {
		return nil, errors.New("migrate: source and destination must be different databases")
	}

	batchSize := opts.BatchSize
	if batchSize <= 0 {
		batchSize = 200
	}

	if !opts.AllowNonEmptyDestination {
		if err := assertEmptyDestination(dst); err != nil {
			return nil, err
		}
	}

	if err := assertSourceSchemaCurrent(ctx, src); err != nil {
		return nil, err
	}

	stats := &MigrateStats{RowsByTable: map[string]int64{}}
	start := time.Now()

	tx, err := dst.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("migrate: begin destination tx: %w", err)
	}
	rollback := func() { _ = tx.Rollback() }

	for _, spec := range migrationTables {
		n, err := copyTable(ctx, src, dst, tx, spec, batchSize, opts.Progress)
		if err != nil {
			rollback()
			return nil, fmt.Errorf("migrate table %s: %w", spec.Name, err)
		}
		stats.RowsByTable[spec.Name] = n
		stats.TotalRows += n
		if opts.Progress != nil {
			opts.Progress(spec.Name, n)
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("migrate: commit destination tx: %w", err)
	}

	if dst.IsPostgres() {
		if err := resetPostgresSequences(ctx, dst); err != nil {
			return stats, fmt.Errorf("migrate: reset sequences: %w", err)
		}
	}

	if !opts.SkipFTSBackfill && dst.FTS5Available() {
		if _, err := dst.BackfillFTS(nil); err != nil {
			return stats, fmt.Errorf("migrate: backfill FTS: %w", err)
		}
	}

	stats.Elapsed = time.Since(start)
	return stats, nil
}

// assertEmptyDestination fails if the destination has any rows in core tables.
// Skipped when AllowNonEmptyDestination is set.
func assertEmptyDestination(dst *Store) error {
	for _, tbl := range []string{"sources", "messages", "conversations", "participants"} {
		var n int64
		err := dst.db.QueryRow(fmt.Sprintf("SELECT COUNT(*) FROM %s", tbl)).Scan(&n)
		if err != nil {
			if dst.dialect.IsNoSuchTableError(err) {
				return fmt.Errorf(
					"destination table %q does not exist — "+
						"run 'msgvault init-db' against the destination first",
					tbl,
				)
			}
			return fmt.Errorf("check destination %s: %w", tbl, err)
		}
		if n != 0 {
			return fmt.Errorf(
				"destination %s has %d rows — refusing to migrate into "+
					"a non-empty database (use --allow-non-empty to override)",
				tbl, n,
			)
		}
	}
	return nil
}

// assertSourceSchemaCurrent runs a single-row SELECT against each source
// table to verify every column the migrator expects is present. Catches
// stale-schema databases (populated by an older msgvault that predates
// a column we now copy) before we start the long-running copy and emit a
// generic "no such column" error halfway through.
//
// LIMIT 0 ensures the query costs nothing beyond plan parsing.
func assertSourceSchemaCurrent(ctx context.Context, src *Store) error {
	for _, spec := range migrationTables {
		q := fmt.Sprintf("SELECT %s FROM %s LIMIT 0",
			strings.Join(spec.Columns, ", "), spec.Name)
		rows, err := src.db.QueryContext(ctx, q)
		if err != nil {
			return fmt.Errorf(
				"source schema check for %s: %w — "+
					"the source database looks out of date; "+
					"run 'msgvault init-db' against it first",
				spec.Name, err,
			)
		}
		_ = rows.Close()
	}
	return nil
}

// copyTable streams rows from src to dst (inside tx) in FK-safe order.
// Returns the number of rows copied. An empty table is not an error.
func copyTable(
	ctx context.Context,
	src *Store, dst *Store, tx *sql.Tx,
	spec tableSpec, batchSize int,
	progress func(string, int64),
) (int64, error) {
	// Scale the batch down to stay under SQLite's 999-parameter limit.
	maxBatchParams := 900
	if cap := maxBatchParams / len(spec.Columns); cap < batchSize {
		batchSize = cap
	}
	// Tables with a bytes column (message_raw.raw_data) can hold multi-MB
	// blobs per row. A full-size batch would pin gigabytes in memory and
	// in the destination's write buffer. Cap these tables at 10 rows per
	// INSERT — roundtrips cost less than OOM crashes.
	if hasBytesColumn(spec.Kinds) && batchSize > 10 {
		batchSize = 10
	}
	if batchSize < 1 {
		batchSize = 1
	}

	selectSQL := fmt.Sprintf("SELECT %s FROM %s %s",
		strings.Join(spec.Columns, ", "), spec.Name, spec.OrderBy)
	rows, err := src.db.QueryContext(ctx, selectSQL)
	if err != nil {
		return 0, fmt.Errorf("select: %w", err)
	}
	defer func() { _ = rows.Close() }()

	tuple := "(" + strings.Join(placeholdersFor(dst, spec.Kinds), ",") + ")"
	insertPrefix := "INSERT INTO " + spec.Name + " (" + strings.Join(spec.Columns, ", ") + ")"
	if dst.IsPostgres() && spec.HasIdentity() {
		insertPrefix += " OVERRIDING SYSTEM VALUE"
	}
	insertPrefix += " VALUES "

	var (
		total  int64
		tuples []string
		args   []any
	)
	flush := func() error {
		if len(tuples) == 0 {
			return nil
		}
		q := dst.dialect.Rebind(insertPrefix + strings.Join(tuples, ","))
		if _, err := tx.ExecContext(ctx, q, args...); err != nil {
			return err
		}
		tuples = tuples[:0]
		args = args[:0]
		return nil
	}

	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		rowArgs, err := scanRow(rows, spec.Kinds)
		if err != nil {
			return total, fmt.Errorf("scan: %w", err)
		}
		tuples = append(tuples, tuple)
		args = append(args, rowArgs...)
		total++
		if len(tuples) >= batchSize {
			if err := flush(); err != nil {
				return total, fmt.Errorf("insert: %w", err)
			}
			if progress != nil {
				progress(spec.Name, total)
			}
		}
	}
	if err := rows.Err(); err != nil {
		return total, fmt.Errorf("iterate: %w", err)
	}
	if err := flush(); err != nil {
		return total, fmt.Errorf("insert: %w", err)
	}
	return total, nil
}

// scanRow reads one row into typed NullXxx values matching the column kinds
// and returns them as driver-ready args.
func scanRow(rows *sql.Rows, kinds []colKind) ([]any, error) {
	scanTargets := make([]any, len(kinds))
	holders := make([]any, len(kinds))
	for i, k := range kinds {
		switch k {
		case kText, kJSON:
			v := &sql.NullString{}
			scanTargets[i] = v
			holders[i] = v
		case kInt:
			v := &sql.NullInt64{}
			scanTargets[i] = v
			holders[i] = v
		case kBool:
			v := &sql.NullBool{}
			scanTargets[i] = v
			holders[i] = v
		case kTime:
			v := &sql.NullTime{}
			scanTargets[i] = v
			holders[i] = v
		case kBytes:
			var b []byte
			scanTargets[i] = &b
			holders[i] = &b
		}
	}
	if err := rows.Scan(scanTargets...); err != nil {
		return nil, err
	}
	args := make([]any, len(kinds))
	for i, h := range holders {
		switch v := h.(type) {
		case *sql.NullString:
			args[i] = *v
		case *sql.NullInt64:
			args[i] = *v
		case *sql.NullBool:
			args[i] = *v
		case *sql.NullTime:
			args[i] = *v
		case *[]byte:
			// Copy to detach from the driver's row buffer.
			if *v == nil {
				args[i] = nil
			} else {
				buf := make([]byte, len(*v))
				copy(buf, *v)
				args[i] = buf
			}
		default:
			return nil, fmt.Errorf("scanRow: unexpected holder type %T at column %d", h, i)
		}
	}
	return args, nil
}

// hasBytesColumn reports whether any column in the spec is kBytes.
// Used to cap batch size for tables that may hold multi-MB blobs per row.
func hasBytesColumn(kinds []colKind) bool {
	for _, k := range kinds {
		if k == kBytes {
			return true
		}
	}
	return false
}

// placeholdersFor returns per-column placeholder tokens. JSON columns get
// "?::jsonb" on Postgres (required cast when binding text into a JSONB column);
// every other column uses a plain "?".
func placeholdersFor(dst *Store, kinds []colKind) []string {
	out := make([]string, len(kinds))
	jsonPh := "?"
	if dst.IsPostgres() {
		jsonPh = dst.dialect.JSONPlaceholder()
	}
	for i, k := range kinds {
		if k == kJSON {
			out[i] = jsonPh
		} else {
			out[i] = "?"
		}
	}
	return out
}

// resetPostgresSequences sets every identity sequence so the next auto-generated
// value is greater than MAX(id). Necessary because Migrate inserts with
// OVERRIDING SYSTEM VALUE, which bypasses the sequence.
func resetPostgresSequences(ctx context.Context, dst *Store) error {
	for _, spec := range migrationTables {
		if !spec.HasIdentity() {
			continue
		}
		// pg_get_serial_sequence returns NULL if the table has no identity
		// column — we guard with HasIdentity above. setval with false leaves
		// nextval() returning the given value; using COALESCE(MAX, 0) + is_called=false
		// makes the next generated id = MAX+1, or 1 if the table is empty.
		q := fmt.Sprintf(
			`SELECT setval(
				pg_get_serial_sequence(%s, %s),
				COALESCE((SELECT MAX(%s) FROM %s), 0) + 1,
				false
			)`,
			quoteString(spec.Name),
			quoteString(spec.IDColumn),
			spec.IDColumn,
			spec.Name,
		)
		if _, err := dst.db.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("%s: %w", spec.Name, err)
		}
	}
	return nil
}

// quoteString emits a single-quoted SQL string literal safe to embed in
// a SELECT/SET statement. Used for identifiers passed to
// pg_get_serial_sequence (which takes text, not an identifier).
func quoteString(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// colKind is the Go-level scan/bind shape for a column.
type colKind int

const (
	kText colKind = iota
	kInt
	kBool
	kTime
	kJSON
	kBytes
)

// tableSpec describes how to migrate one table: its column list (identical on
// both dialects), the scan/bind kind per column, the identity column name (or
// "" for non-identity tables), and the ORDER BY clause used for the SELECT.
//
// Ordering matters for two reasons:
//  1. messages.reply_to_message_id is a self-FK — inserting ascending by id
//     ensures the parent is already present before its reply.
//  2. Deterministic order makes per-row progress meaningful and reproducible.
type tableSpec struct {
	Name     string
	Columns  []string
	Kinds    []colKind
	IDColumn string
	OrderBy  string
}

// HasIdentity reports whether the table has a serial/identity primary key
// that requires OVERRIDING SYSTEM VALUE on Postgres inserts and a sequence
// reset after migration.
func (s tableSpec) HasIdentity() bool { return s.IDColumn != "" }

// migrationTables lists every user-data table in foreign-key dependency order.
// Parents appear before children. The list intentionally omits:
//   - messages_fts (SQLite FTS5 virtual table — rebuilt via BackfillFTS)
//   - messages.search_fts (Postgres tsvector column — rebuilt via BackfillFTS)
//   - sqlite_sequence (managed by SQLite itself)
var migrationTables = []tableSpec{
	{
		Name: "sources",
		Columns: []string{
			"id", "source_type", "identifier", "display_name",
			"google_user_id", "last_sync_at", "sync_cursor", "sync_config",
			"oauth_app", "created_at", "updated_at",
		},
		Kinds: []colKind{
			kInt, kText, kText, kText,
			kText, kTime, kText, kJSON,
			kText, kTime, kTime,
		},
		IDColumn: "id", OrderBy: "ORDER BY id",
	},
	{
		Name: "participants",
		Columns: []string{
			"id", "email_address", "phone_number", "display_name",
			"domain", "canonical_id", "created_at", "updated_at",
		},
		Kinds: []colKind{
			kInt, kText, kText, kText,
			kText, kText, kTime, kTime,
		},
		IDColumn: "id", OrderBy: "ORDER BY id",
	},
	{
		Name: "participant_identifiers",
		Columns: []string{
			"id", "participant_id", "identifier_type",
			"identifier_value", "display_value", "is_primary",
		},
		Kinds: []colKind{
			kInt, kInt, kText,
			kText, kText, kBool,
		},
		IDColumn: "id", OrderBy: "ORDER BY id",
	},
	{
		Name: "conversations",
		Columns: []string{
			"id", "source_id", "source_conversation_id", "conversation_type",
			"title", "participant_count", "message_count", "unread_count",
			"last_message_at", "last_message_preview", "metadata",
			"created_at", "updated_at",
		},
		Kinds: []colKind{
			kInt, kInt, kText, kText,
			kText, kInt, kInt, kInt,
			kTime, kText, kJSON,
			kTime, kTime,
		},
		IDColumn: "id", OrderBy: "ORDER BY id",
	},
	{
		Name: "conversation_participants",
		Columns: []string{
			"conversation_id", "participant_id",
			"role", "joined_at", "left_at",
		},
		Kinds: []colKind{
			kInt, kInt,
			kText, kTime, kTime,
		},
		IDColumn: "",
		OrderBy:  "ORDER BY conversation_id, participant_id",
	},
	{
		Name: "labels",
		Columns: []string{
			"id", "source_id", "source_label_id",
			"name", "label_type", "color",
		},
		Kinds: []colKind{
			kInt, kInt, kText,
			kText, kText, kText,
		},
		IDColumn: "id", OrderBy: "ORDER BY id",
	},
	{
		Name: "messages",
		Columns: []string{
			"id", "conversation_id", "source_id", "source_message_id",
			"rfc822_message_id", "message_type",
			"sent_at", "received_at", "read_at", "delivered_at", "internal_date",
			"sender_id", "is_from_me",
			"subject", "snippet",
			"reply_to_message_id", "thread_position",
			"is_read", "is_delivered", "is_sent", "is_edited", "is_forwarded",
			"size_estimate", "has_attachments", "attachment_count",
			"deleted_at", "deleted_from_source_at", "delete_batch_id",
			"archived_at", "indexing_version", "metadata",
		},
		Kinds: []colKind{
			kInt, kInt, kInt, kText,
			kText, kText,
			kTime, kTime, kTime, kTime, kTime,
			kInt, kBool,
			kText, kText,
			kInt, kInt,
			kBool, kBool, kBool, kBool, kBool,
			kInt, kBool, kInt,
			kTime, kTime, kText,
			kTime, kInt, kJSON,
		},
		IDColumn: "id", OrderBy: "ORDER BY id",
	},
	{
		Name: "message_recipients",
		Columns: []string{
			"id", "message_id", "participant_id",
			"recipient_type", "display_name",
		},
		Kinds: []colKind{
			kInt, kInt, kInt,
			kText, kText,
		},
		IDColumn: "id", OrderBy: "ORDER BY id",
	},
	{
		Name: "reactions",
		Columns: []string{
			"id", "message_id", "participant_id",
			"reaction_type", "reaction_value",
			"created_at", "removed_at",
		},
		Kinds: []colKind{
			kInt, kInt, kInt,
			kText, kText,
			kTime, kTime,
		},
		IDColumn: "id", OrderBy: "ORDER BY id",
	},
	{
		Name: "attachments",
		Columns: []string{
			"id", "message_id",
			"filename", "mime_type", "size",
			"content_hash", "storage_path",
			"media_type", "width", "height", "duration_ms",
			"thumbnail_hash", "thumbnail_path",
			"source_attachment_id", "attachment_metadata",
			"encryption_version", "created_at",
		},
		Kinds: []colKind{
			kInt, kInt,
			kText, kText, kInt,
			kText, kText,
			kText, kInt, kInt, kInt,
			kText, kText,
			kText, kJSON,
			kInt, kTime,
		},
		IDColumn: "id", OrderBy: "ORDER BY id",
	},
	{
		Name:     "message_labels",
		Columns:  []string{"message_id", "label_id"},
		Kinds:    []colKind{kInt, kInt},
		IDColumn: "",
		OrderBy:  "ORDER BY message_id, label_id",
	},
	{
		Name:     "message_bodies",
		Columns:  []string{"message_id", "body_text", "body_html"},
		Kinds:    []colKind{kInt, kText, kText},
		IDColumn: "",
		OrderBy:  "ORDER BY message_id",
	},
	{
		Name: "message_raw",
		Columns: []string{
			"message_id", "raw_data", "raw_format",
			"compression", "encryption_version",
		},
		Kinds: []colKind{
			kInt, kBytes, kText,
			kText, kInt,
		},
		IDColumn: "",
		OrderBy:  "ORDER BY message_id",
	},
	{
		Name: "sync_runs",
		Columns: []string{
			"id", "source_id",
			"started_at", "completed_at", "status",
			"messages_processed", "messages_added", "messages_updated", "errors_count",
			"error_message", "cursor_before", "cursor_after",
		},
		Kinds: []colKind{
			kInt, kInt,
			kTime, kTime, kText,
			kInt, kInt, kInt, kInt,
			kText, kText, kText,
		},
		IDColumn: "id", OrderBy: "ORDER BY id",
	},
	{
		Name: "sync_checkpoints",
		Columns: []string{
			"source_id", "checkpoint_type",
			"checkpoint_value", "updated_at",
		},
		Kinds: []colKind{
			kInt, kText,
			kText, kTime,
		},
		IDColumn: "",
		OrderBy:  "ORDER BY source_id, checkpoint_type",
	},
}
