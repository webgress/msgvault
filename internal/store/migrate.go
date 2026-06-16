package store

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"time"
)

// migrateTableOrder lists every data table copied by MigrateVault, in
// parent-before-child order so that foreign keys are satisfied as rows land.
// The order mirrors subset.go's dependency walk (which is SQLite-ATTACH-only
// and not reusable cross-backend) but is enumerated explicitly here.
//
// messages.reply_to_message_id is a SELF-FK, handled by the two-pass copy in
// copyMessages — it is NOT a reason to reorder this slice.
var migrateTableOrder = []string{
	"sources",
	"participants",
	"participant_identifiers",
	"conversations",
	"conversation_participants",
	"messages",
	"message_recipients",
	"reactions",
	"attachments",
	"labels",
	"message_labels",
	"message_bodies",
	"message_raw",
	"sync_runs",
	"sync_checkpoints",
	"source_import_items",
	"collections",
	"collection_sources",
	"account_identities",
	"applied_migrations",
}

// identityPKTables are the tables whose primary key is a generated identity /
// autoincrement integer named "id". After copying these to PostgreSQL the
// backing sequence must be advanced past the largest copied id (setval) so that
// subsequent normal inserts through the store don't collide with preserved ids.
//
// Junction / composite-PK tables (conversation_participants, message_labels,
// collection_sources, sync_checkpoints, account_identities) and the
// message_id-keyed tables (message_bodies, message_raw) have no identity
// sequence and are deliberately excluded — verified against schema_pg.sql.
var identityPKTables = []string{
	"sources",
	"participants",
	"participant_identifiers",
	"conversations",
	"messages",
	"message_recipients",
	"reactions",
	"attachments",
	"labels",
	"sync_runs",
	"source_import_items",
	"collections",
}

// pkOrderColumn returns the column the reader orders rows by for a stable,
// FK-friendly copy. Identity-PK tables use "id"; the rest use a deterministic
// composite that exists on the table.
var pkOrderColumn = map[string]string{
	"sources":                   "id",
	"participants":              "id",
	"participant_identifiers":   "id",
	"conversations":             "id",
	"conversation_participants": "conversation_id, participant_id",
	"messages":                  "id",
	"message_recipients":        "id",
	"reactions":                 "id",
	"attachments":               "id",
	"labels":                    "id",
	"message_labels":            "message_id, label_id",
	"message_bodies":            "message_id",
	"message_raw":               "message_id",
	"sync_runs":                 "id",
	"sync_checkpoints":          "source_id, checkpoint_type",
	"source_import_items":       "id",
	"collections":               "id",
	"collection_sources":        "collection_id, source_id",
	"account_identities":        "source_id, address",
	"applied_migrations":        "name",
}

// neverCopyColumns names columns that must never cross backends. messages
// carries an inline tsvector (search_fts) on PostgreSQL that has no SQLite
// counterpart; FTS is always rebuilt on the destination, never copied, so this
// column is dropped from the projection in both directions.
var neverCopyColumns = map[string]map[string]bool{
	"messages": {"search_fts": true},
}

// jsonColumns names the JSON/JSONB columns per table. On a PostgreSQL
// destination these bind through ?::JSONB; on a SQLite destination a []byte
// value read from PG's JSONB is converted back to a string so it lands as TEXT
// (not a BLOB).
var jsonColumns = map[string]map[string]bool{
	"sources":       {"sync_config": true},
	"conversations": {"metadata": true},
	"messages":      {"metadata": true},
	"attachments":   {"attachment_metadata": true},
}

// boolColumns names the boolean columns per table. SQLite stores them as 0/1
// INTEGER, PostgreSQL as BOOLEAN. The mattn/pgx drivers already decode a
// BOOLEAN-declared column to a Go bool, but legacy SQLite rows can surface a
// raw int64; coerceBool normalizes those before binding to a PG destination.
var boolColumns = map[string]map[string]bool{
	"participant_identifiers": {"is_primary": true},
	"messages": {
		"is_from_me":      true,
		"is_read":         true,
		"is_delivered":    true,
		"is_sent":         true,
		"is_edited":       true,
		"is_forwarded":    true,
		"has_attachments": true,
	},
}

// DefaultMigrateBatch is the default number of rows per multi-row INSERT batch
// for a MigrateVault run. Referenced by MigrateVault's own default, the CLI
// flag default, and tests so the three never drift apart.
const DefaultMigrateBatch = 5000

// MigrateOptions configures a MigrateVault run.
type MigrateOptions struct {
	// Batch is the number of rows per multi-row INSERT (and per transaction).
	Batch int
	// Resume uses conflict-ignoring inserts so a re-run skips already-copied
	// rows instead of erroring on duplicate keys.
	Resume bool
	// DryRun reports source row counts without writing anything.
	DryRun bool
	// Progress, when non-nil, is called as each table copies with the table
	// name, rows done so far, and total rows for that table.
	Progress func(table string, done, total int64)
}

// TableResult is the per-table outcome of a copy.
type TableResult struct {
	Table  string
	Rows   int64 // rows read from source (== rows attempted)
	Source int64 // source row count (for dry-run / verify parity)
}

// MigrateResult summarizes a MigrateVault run, mirroring CopyResult's style.
type MigrateResult struct {
	Tables  []TableResult
	Elapsed time.Duration
	DryRun  bool
}

// RowsCopied returns the total rows read across all tables.
func (r *MigrateResult) RowsCopied() int64 {
	var n int64
	for _, t := range r.Tables {
		n += t.Rows
	}
	return n
}

// MigrateVault copies every data table from src into dst, preserving all
// primary keys verbatim so foreign keys remain valid without remapping. The
// destination schema must already be initialized (dst.InitSchema()).
//
// Backends are chosen by the *Store the caller opened: src/dst may each be
// SQLite or PostgreSQL, in any combination. Vectors and FTS are NOT copied —
// FTS is rebuilt on the destination by the caller (see the migrate command),
// and vectors are out of scope for the data copy (re-embed on the destination).
//
// The copy proceeds table-by-table in migrateTableOrder. messages is copied in
// two passes so its self-referential reply_to_message_id can be set only after
// every message row exists. On a PostgreSQL destination each identity-PK table's
// sequence is advanced past the largest copied id after the table is copied.
//
// When opts.DryRun is set, only source counts are read and dst is never touched;
// callers may pass a nil dst for a dry run.
func MigrateVault(ctx context.Context, src, dst *Store, opts MigrateOptions) (*MigrateResult, error) {
	if opts.Batch <= 0 {
		opts.Batch = DefaultMigrateBatch
	}
	start := time.Now()
	result := &MigrateResult{DryRun: opts.DryRun}

	for _, table := range migrateTableOrder {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		srcCount, err := countRows(ctx, src, table)
		if err != nil {
			return nil, fmt.Errorf("count source %s: %w", table, err)
		}

		if opts.DryRun {
			result.Tables = append(result.Tables, TableResult{
				Table: table, Rows: srcCount, Source: srcCount,
			})
			if opts.Progress != nil {
				opts.Progress(table, srcCount, srcCount)
			}
			continue
		}

		var copied int64
		if table == "messages" {
			copied, err = copyMessages(ctx, src, dst, opts)
		} else {
			copied, err = copyTable(ctx, src, dst, table, nil, opts)
		}
		if err != nil {
			return nil, fmt.Errorf("copy %s: %w", table, err)
		}

		result.Tables = append(result.Tables, TableResult{
			Table: table, Rows: copied, Source: srcCount,
		})
	}

	// Advance PostgreSQL identity sequences past the largest copied id so
	// subsequent normal store inserts don't collide with preserved ids.
	if !opts.DryRun && dst.IsPostgreSQL() {
		for _, table := range identityPKTables {
			if err := resetPGSequence(ctx, dst, table); err != nil {
				return nil, fmt.Errorf("reset sequence for %s: %w", table, err)
			}
		}
	}

	result.Elapsed = time.Since(start)
	return result, nil
}

// DestHasData reports whether dst holds any real archive data beyond the
// pristine baseline a freshly initialized store carries. InitSchema seeds a
// single default "All" collection (collections has one row, collection_sources
// is empty); that baseline is treated as empty so a fresh destination is not
// mistaken for a populated one. Returns the first table found with real data.
//
// Used by the migrate command to refuse clobbering a populated destination
// unless --resume or --truncate-dest is given.
func DestHasData(ctx context.Context, dst *Store) (bool, string, error) {
	for _, table := range migrateTableOrder {
		// applied_migrations is internal bookkeeping that InitSchema seeds
		// (e.g. participants_phone_unique_index). It is always conflict-skipped
		// on copy and never represents user data, so it is not a signal that the
		// destination is "populated".
		if table == "applied_migrations" {
			continue
		}
		n, err := countRows(ctx, dst, table)
		if err != nil {
			return false, "", fmt.Errorf("count %s: %w", table, err)
		}
		if n == 0 {
			continue
		}
		// The auto-seeded default collection is part of the empty baseline.
		if table == "collections" && n == 1 {
			only, err := onlyDefaultCollection(ctx, dst)
			if err != nil {
				return false, "", err
			}
			if only {
				continue
			}
		}
		return true, table, nil
	}
	return false, "", nil
}

// onlyDefaultCollection reports whether the single collections row is the
// auto-seeded default ("All").
func onlyDefaultCollection(ctx context.Context, dst *Store) (bool, error) {
	var name string
	err := dst.DB().QueryRowContext(ctx, "SELECT name FROM collections").Scan(&name)
	if err != nil {
		return false, fmt.Errorf("read collection name: %w", err)
	}
	return name == DefaultCollectionName, nil
}

// TruncateDest removes all rows from every data table on dst. On PostgreSQL it
// uses a single TRUNCATE ... CASCADE RESTART IDENTITY so identity sequences are
// reset too; on SQLite it DELETEs in reverse FK order inside one transaction.
func TruncateDest(ctx context.Context, dst *Store) error {
	if dst.IsPostgreSQL() {
		stmt := "TRUNCATE TABLE " + strings.Join(migrateTableOrder, ", ") +
			" RESTART IDENTITY CASCADE"
		if _, err := dst.DB().ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("truncate: %w", err)
		}
		return nil
	}

	tx, err := dst.DB().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin truncate tx: %w", err)
	}
	// Reverse FK order: delete children before parents.
	for _, table := range slices.Backward(migrateTableOrder) {
		if _, err := tx.ExecContext(ctx, "DELETE FROM "+table); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("delete %s: %w", table, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit truncate tx: %w", err)
	}
	return nil
}

// countRows returns the number of rows in table on st.
func countRows(ctx context.Context, st *Store, table string) (int64, error) {
	var n int64
	err := st.DB().QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&n)
	if err != nil {
		return 0, err
	}
	return n, nil
}

// copyTable streams every row of table from src to dst, preserving column
// values verbatim. When excludeCols is non-empty those columns are omitted from
// the copy (used by the messages two-pass to defer reply_to_message_id).
//
// Each batch of opts.Batch rows is one multi-row INSERT in its own transaction
// so partial progress survives an interruption (and --resume can re-run).
func copyTable(ctx context.Context, src, dst *Store, table string, excludeCols map[string]bool, opts MigrateOptions) (int64, error) {
	order := pkOrderColumn[table]
	if order == "" {
		order = "1"
	}
	rows, err := src.DB().QueryContext(ctx, "SELECT * FROM "+table+" ORDER BY "+order)
	if err != nil {
		return 0, fmt.Errorf("read %s: %w", table, err)
	}
	defer func() { _ = rows.Close() }()

	allCols, err := rows.Columns()
	if err != nil {
		return 0, fmt.Errorf("columns %s: %w", table, err)
	}

	// Build the kept-column projection and the index map into the scan buffer.
	// Always-excluded columns (search_fts) drop here regardless of caller.
	never := neverCopyColumns[table]
	cols := make([]string, 0, len(allCols))
	keepIdx := make([]int, 0, len(allCols))
	for i, c := range allCols {
		if excludeCols[c] || never[c] {
			continue
		}
		cols = append(cols, c)
		keepIdx = append(keepIdx, i)
	}
	if len(cols) == 0 {
		return 0, fmt.Errorf("no columns to copy for %s", table)
	}

	w := newTableWriter(dst, table, cols, opts)

	scan := make([]any, len(allCols))
	ptrs := make([]any, len(allCols))
	for i := range scan {
		ptrs[i] = &scan[i]
	}

	var copied int64
	total, _ := countRows(ctx, src, table)
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return copied, err
		}
		if err := rows.Scan(ptrs...); err != nil {
			return copied, fmt.Errorf("scan %s: %w", table, err)
		}
		rowVals := make([]any, len(keepIdx))
		for j, idx := range keepIdx {
			rowVals[j] = bridgeValue(table, cols[j], scan[idx], dst.IsPostgreSQL())
		}
		flushed, err := w.add(ctx, rowVals)
		if err != nil {
			return copied, err
		}
		copied += flushed
		if opts.Progress != nil {
			opts.Progress(table, copied, total)
		}
	}
	if err := rows.Err(); err != nil {
		return copied, fmt.Errorf("iterate %s: %w", table, err)
	}
	flushed, err := w.flush(ctx)
	if err != nil {
		return copied, err
	}
	copied += flushed
	if opts.Progress != nil {
		opts.Progress(table, copied, total)
	}
	return copied, nil
}

// copyMessages copies the messages table in two passes so the self-referential
// reply_to_message_id can be populated only after every message row exists:
//
//	pass 1: INSERT every message column EXCEPT reply_to_message_id
//	pass 2: UPDATE reply_to_message_id for rows that have one
//
// This keeps the copy valid under SQLite's foreign_keys=ON and PostgreSQL's
// always-on FK enforcement.
func copyMessages(ctx context.Context, src, dst *Store, opts MigrateOptions) (int64, error) {
	copied, err := copyTable(ctx, src, dst, "messages",
		map[string]bool{"reply_to_message_id": true}, opts)
	if err != nil {
		return copied, err
	}
	if err := updateReplyTo(ctx, src, dst, opts); err != nil {
		return copied, fmt.Errorf("messages reply_to pass: %w", err)
	}
	return copied, nil
}

// updateReplyTo runs the second messages pass: set reply_to_message_id on every
// destination row whose source row had a non-NULL value. Batched into its own
// transactions like the copy path.
func updateReplyTo(ctx context.Context, src, dst *Store, opts MigrateOptions) error {
	rows, err := src.DB().QueryContext(ctx,
		"SELECT id, reply_to_message_id FROM messages "+
			"WHERE reply_to_message_id IS NOT NULL ORDER BY id")
	if err != nil {
		return fmt.Errorf("read reply_to: %w", err)
	}
	defer func() { _ = rows.Close() }()

	type pair struct{ id, replyTo int64 }
	var batch []pair
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		tx, err := dst.DB().BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin reply_to tx: %w", err)
		}
		stmt := dst.Rebind("UPDATE messages SET reply_to_message_id = ? WHERE id = ?")
		for _, p := range batch {
			if _, err := tx.ExecContext(ctx, stmt, p.replyTo, p.id); err != nil {
				_ = tx.Rollback()
				return fmt.Errorf("update reply_to id=%d: %w", p.id, err)
			}
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit reply_to tx: %w", err)
		}
		batch = batch[:0]
		return nil
	}

	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return err
		}
		var p pair
		if err := rows.Scan(&p.id, &p.replyTo); err != nil {
			return fmt.Errorf("scan reply_to: %w", err)
		}
		batch = append(batch, p)
		if len(batch) >= opts.Batch {
			if err := flush(); err != nil {
				return err
			}
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate reply_to: %w", err)
	}
	return flush()
}

// tableWriter accumulates rows and emits chunked multi-row INSERTs, each in its
// own transaction. It honors the parameter limit (SQLite caps at 999 bound
// variables) by sizing chunks to opts.Batch while staying under the cap.
type tableWriter struct {
	dst          *Store
	table        string
	cols         []string
	valuesPerRow int
	placeholders []string // per-row "(?,?,...)" with JSONB casts applied
	chunkRows    int
	resume       bool
	pending      [][]any
}

func newTableWriter(dst *Store, table string, cols []string, opts MigrateOptions) *tableWriter {
	// Per-row placeholder tuple. JSON columns on PG need ?::JSONB.
	jcols := jsonColumns[table]
	pgDest := dst.IsPostgreSQL()
	jsonExpr := dst.dialect.JSONBindExpr()
	ph := make([]string, len(cols))
	for i, c := range cols {
		if pgDest && jcols[c] {
			ph[i] = jsonExpr
		} else {
			ph[i] = "?"
		}
	}

	// Stay under SQLite's 999-variable cap with margin; honor the batch size.
	const maxParams = 900
	chunkRows := opts.Batch
	if perRowCap := maxParams / max(len(cols), 1); chunkRows > perRowCap {
		chunkRows = perRowCap
	}
	if chunkRows < 1 {
		chunkRows = 1
	}

	return &tableWriter{
		dst:          dst,
		table:        table,
		cols:         cols,
		valuesPerRow: len(cols),
		placeholders: ph,
		chunkRows:    chunkRows,
		resume:       opts.Resume,
	}
}

// add buffers one row and flushes a chunk when the buffer is full. Returns the
// number of rows committed by this call (0 unless a chunk was flushed).
func (w *tableWriter) add(ctx context.Context, row []any) (int64, error) {
	w.pending = append(w.pending, row)
	if len(w.pending) >= w.chunkRows {
		return w.flush(ctx)
	}
	return 0, nil
}

// flush writes all buffered rows as a single multi-row INSERT in one tx.
func (w *tableWriter) flush(ctx context.Context) (int64, error) {
	if len(w.pending) == 0 {
		return 0, nil
	}
	n := int64(len(w.pending))

	tuple := "(" + strings.Join(w.placeholders, ",") + ")"
	tuples := make([]string, len(w.pending))
	args := make([]any, 0, len(w.pending)*w.valuesPerRow)
	for i, row := range w.pending {
		tuples[i] = tuple
		args = append(args, row...)
	}

	// applied_migrations is idempotent bookkeeping that InitSchema may have
	// already seeded on the destination, so it is ALWAYS conflict-skipped — a
	// duplicate name must not abort a non-resume copy.
	conflictSkip := w.resume || w.table == "applied_migrations"

	prefix := "INSERT INTO " + w.table + " (" + strings.Join(w.cols, ",") + ") VALUES "
	var suffix string
	if conflictSkip {
		// Conflict-skip: INSERT OR IGNORE (SQLite) / ON CONFLICT DO NOTHING (PG).
		prefix = w.dst.dialect.InsertOrIgnorePrefix("INSERT OR IGNORE INTO " +
			w.table + " (" + strings.Join(w.cols, ",") + ") VALUES ")
		suffix = w.dst.dialect.InsertOrIgnoreSuffix()
	}
	// OVERRIDING SYSTEM VALUE lets PG accept explicit values for an identity
	// (GENERATED ALWAYS) primary key. Harmless to include only when the table
	// has an identity id column; SQLite never sees this branch.
	if w.dst.IsPostgreSQL() && tableHasIdentityID(w.table) {
		prefix = "INSERT INTO " + w.table + " (" + strings.Join(w.cols, ",") +
			") OVERRIDING SYSTEM VALUE VALUES "
		if conflictSkip {
			suffix = w.dst.dialect.InsertOrIgnoreSuffix()
		}
	}

	query := w.dst.Rebind(prefix + strings.Join(tuples, ",") + suffix)

	tx, err := w.dst.DB().BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin %s tx: %w", w.table, err)
	}
	if _, err := tx.ExecContext(ctx, query, args...); err != nil {
		_ = tx.Rollback()
		return 0, fmt.Errorf("insert %s: %w", w.table, err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit %s tx: %w", w.table, err)
	}

	w.pending = w.pending[:0]
	return n, nil
}

// tableHasIdentityID reports whether table has a generated identity "id" column
// that needs OVERRIDING SYSTEM VALUE on a PostgreSQL destination.
func tableHasIdentityID(table string) bool {
	return slices.Contains(identityPKTables, table)
}

// bridgeValue normalizes a scanned source value for binding to dst.
//
//   - JSON columns: PG's JSONB reads back as []byte; when the destination is
//     SQLite, convert to string so it lands as TEXT (not a BLOB). On a PG
//     destination the ?::JSONB cast in the placeholder handles a string bind.
//   - boolean columns: a legacy SQLite int64 (0/1) is coerced to bool for a PG
//     destination, which rejects integer→boolean binds.
//
// Other values (int64, string, time.Time, []byte BYTEA) bind natively across
// the mattn (SQLite) and pgx (PostgreSQL) drivers.
func bridgeValue(table, col string, v any, pgDest bool) any {
	if v == nil {
		return nil
	}
	if jsonColumns[table][col] {
		if !pgDest {
			if b, ok := v.([]byte); ok {
				return string(b)
			}
		}
		return v
	}
	if pgDest && boolColumns[table][col] {
		return coerceBool(v)
	}
	return v
}

// coerceBool converts a 0/1 integer (legacy SQLite boolean storage) to a Go
// bool. Values already bool (the common case) pass through unchanged.
func coerceBool(v any) any {
	switch t := v.(type) {
	case bool:
		return t
	case int64:
		return t != 0
	case int:
		return t != 0
	default:
		return v
	}
}

// resetPGSequence advances the identity sequence backing table.id past the
// largest copied id so subsequent normal inserts don't collide. Uses
// pg_get_serial_sequence so it works regardless of the sequence's exact name.
//
// The 3-arg setval(seq, value, is_called) form is deliberate:
//   - empty table  -> setval(seq, 1, false): the sequence stays UNCALLED at 1,
//     so the very first subsequent insert yields id=1 (the 2-arg form would
//     mark it called and skip id=1).
//   - non-empty    -> setval(seq, MAX(id), true): the next insert yields
//     MAX(id)+1.
//
// This stays consistent with pgSequenceAtLeastMax's effective-next computation
// (last_value + (is_called ? 1 : 0)).
func resetPGSequence(ctx context.Context, dst *Store, table string) error {
	var seq sql.NullString
	err := dst.DB().QueryRowContext(ctx,
		"SELECT pg_get_serial_sequence($1, 'id')", table).Scan(&seq)
	if err != nil {
		return fmt.Errorf("get sequence for %s: %w", table, err)
	}
	if !seq.Valid || seq.String == "" {
		// No identity sequence (e.g. table empty of identity metadata); nothing
		// to reset.
		return nil
	}
	_, err = dst.DB().ExecContext(ctx, fmt.Sprintf(
		"SELECT setval(%s, GREATEST(COALESCE(MAX(id),1),1), MAX(id) IS NOT NULL) FROM %s",
		quoteLiteral(seq.String), table))
	if err != nil {
		return fmt.Errorf("setval %s: %w", table, err)
	}
	return nil
}

// quoteLiteral renders s as a single-quoted SQL string literal with embedded
// quotes doubled. Used for the sequence name (a pg_get_serial_sequence result,
// not user input) passed to setval, which takes a regclass text literal.
func quoteLiteral(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}
