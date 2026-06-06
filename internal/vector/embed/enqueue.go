package embed

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/vector"
)

// enqueueChunkRows caps how many (gen, message) tuples go into a single
// INSERT statement. Each row contributes 3 placeholders (generation_id,
// message_id, enqueued_at), so 500 rows = 1,500 bound parameters — well
// under SQLite's default SQLITE_MAX_VARIABLE_NUMBER (999 on older builds,
// 32k on newer ones; 1,500 is safe on the latter and the loop simply
// issues more statements on the former is moot because we stay at 500).
//
// The Enqueuer can be handed up to ~5,000 IDs by sync, fanned out across
// up to two non-retired generations; without chunking that would be
// 3×5,000 = 15,000 placeholders per statement, which exceeds the old
// SQLite cap and bloats the prepared statement. 500 keeps every
// statement comfortably small on both SQLite and PostgreSQL while still
// amortizing the per-statement overhead (a 5,000-ID batch becomes 10
// statements, not 5,000 single-row inserts).
const enqueueChunkRows = 500

// Enqueuer inserts message IDs into pending_embeddings for every
// non-retired generation. Implements the EmbedEnqueuer interface
// expected by internal/sync.
//
// Dual-enqueue is intentional: when a rebuild is in progress there are
// two non-retired generations (active + building); every newly-synced
// message gets queued into both so the building index stays current.
type Enqueuer struct {
	db *sql.DB
	// rebind translates ?-placeholders to the driver's native form; nil
	// is normalized to identity (SQLite). insertOrIgnore rewrites a
	// complete "INSERT OR IGNORE INTO ..." statement into the dialect's
	// conflict-ignoring form (SQLite: identity; PostgreSQL: strips
	// "OR IGNORE" and appends "ON CONFLICT DO NOTHING"); nil is
	// normalized to identity. Both are applied in the same order the
	// store package uses: insertOrIgnore first (it operates on the
	// ?-placeholder SQLite form), then rebind.
	rebind         func(string) string
	insertOrIgnore func(string) string
}

// NewEnqueuer returns an Enqueuer backed by the embeddings database
// (vectors.db on SQLite, the shared main DB on PostgreSQL). rebind and
// insertOrIgnore make the Enqueuer dialect-portable without importing
// internal/store, mirroring NewQueue's decoupled func style: pass nil
// for both on SQLite (identity), or the dialect's Rebind and
// InsertOrIgnore for pgx.
func NewEnqueuer(db *sql.DB, rebind, insertOrIgnore func(string) string) *Enqueuer {
	if rebind == nil {
		rebind = func(q string) string { return q }
	}
	if insertOrIgnore == nil {
		insertOrIgnore = func(q string) string { return q }
	}
	return &Enqueuer{db: db, rebind: rebind, insertOrIgnore: insertOrIgnore}
}

// EnqueueMessages adds the given IDs to pending_embeddings for every
// generation not in state 'retired'. Duplicate IDs are silently ignored
// via INSERT OR IGNORE. Caller must only pass non-deleted message IDs —
// the deletion predicate is not checked here.
func (e *Enqueuer) EnqueueMessages(ctx context.Context, messageIDs []int64) error {
	if len(messageIDs) == 0 {
		return nil
	}
	tx, err := e.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin enqueue tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	gens, err := func() ([]int64, error) {
		rows, err := tx.QueryContext(ctx,
			e.rebind(`SELECT id FROM index_generations WHERE state != ?`),
			string(vector.GenerationRetired))
		if err != nil {
			return nil, fmt.Errorf("select non-retired generations: %w", err)
		}
		defer func() { _ = rows.Close() }()
		var out []int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				return nil, fmt.Errorf("scan generation id: %w", err)
			}
			out = append(out, id)
		}
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("iterate generations: %w", err)
		}
		return out, nil
	}()
	if err != nil {
		return err
	}
	if len(gens) == 0 {
		return tx.Commit()
	}

	// Bulk-insert one row per (gen, message) pair via chunked multi-row
	// VALUES statements. Each (gen, message) tuple binds 3 parameters, so
	// we cap each statement at enqueueChunkRows rows to stay under
	// SQLite's parameter limit and avoid an oversized prepared statement
	// on either backend. For a 5,000-message batch with two non-retired
	// generations this is ~20 writes against the embeddings DB lock
	// instead of 10,000 single-row inserts — keeps the embed worker's
	// Claim from starving while sync flushes. The previous json_each
	// path issued one statement per generation but is SQLite-only;
	// chunked VALUES is portable to pgx.
	now := time.Now().Unix()
	for _, g := range gens {
		for start := 0; start < len(messageIDs); start += enqueueChunkRows {
			end := min(start+enqueueChunkRows, len(messageIDs))
			chunk := messageIDs[start:end]

			placeholders := make([]string, len(chunk))
			args := make([]any, 0, len(chunk)*3)
			for i, id := range chunk {
				placeholders[i] = "(?, ?, ?)"
				args = append(args, g, id, now)
			}
			// Build the SQLite-form statement, then apply the dialect's
			// insert-or-ignore rewrite (operates on ? placeholders),
			// then rebind ? → $N. Same ordering as the store package's
			// InsertOrIgnore-then-loggedDB-Rebind pipeline.
			stmt := `INSERT OR IGNORE INTO pending_embeddings (generation_id, message_id, enqueued_at) VALUES ` +
				strings.Join(placeholders, ",")
			stmt = e.rebind(e.insertOrIgnore(stmt))
			if _, err := tx.ExecContext(ctx, stmt, args...); err != nil {
				return fmt.Errorf("insert pending (gen=%d): %w", g, err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit enqueue: %w", err)
	}
	return nil
}
