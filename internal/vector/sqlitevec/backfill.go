//go:build sqlite_vec

package sqlitevec

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"go.kenn.io/msgvault/internal/vector"
)

// embedGenBackfillMigration is the applied_migrations ledger key that
// guards the one-time embed_gen upgrade backfill. Stable string — never
// change it, or the backfill would re-run on every Open.
const embedGenBackfillMigration = "embed_gen_backfill_active_v1"

// backfillStampChunk caps how many message ids go into one stamping
// UPDATE so the bind-parameter count stays well under SQLite's limit.
const backfillStampChunk = 500

// BackfillEmbedGenForUpgrade performs the ONE-TIME upgrade backfill
// (Package A): when an active generation exists, it stamps embed_gen=active
// on every main-DB message that already has >=1 embedding row under that
// generation but whose embed_gen is still NULL.
//
// Why: the embed_gen ADD COLUMN migration does no backfill, so a user
// upgrading from v0.14–v0.15 (who already has an active generation + a
// fully-embedded corpus) would have embed_gen=NULL everywhere. Coverage
// would then report the ENTIRE archive as missing and the worker would
// re-embed all of it. This stamps the already-embedded rows instead — a
// cheap metadata UPDATE, no re-embed.
//
// Guards:
//   - ONE-TIME via the applied_migrations ledger (key
//     embedGenBackfillMigration). Check-then-run-then-mark. It must NOT run
//     on every Open: re-running would clobber repair-encoding's NULL resets
//     before they re-embed, and fight an in-progress rebuild.
//   - It lives in the VECTOR layer because the embeddings table is only
//     reachable here (it is in vectors.db on SQLite, a separate *sql.DB
//     from messages).
//   - The stamp UPDATE only touches rows where embed_gen IS NULL, so it
//     never overwrites a row already stamped for another generation.
//
// No-ops cleanly when: no main DB handle, the ledger already records it, no
// active generation, no embeddings table, or no embedded-but-unstamped
// rows. Idempotent.
//
// Cross-DB on SQLite: the embeddings ids come from vectors.db (b.db); the
// stamp and the ledger live in main.db (b.mainDB), two separate *sql.DB
// handles. This mirrors the established cross-DB pattern (see
// EmbeddedMessageCount / dropDeletedFromSource): read ids from vectors.db,
// stamp on main.db.
func (b *Backend) BackfillEmbedGenForUpgrade(ctx context.Context) error {
	if b.mainDB == nil {
		// Management commands may open the backend without the main handle;
		// they never run the backfill.
		return nil
	}
	if b.readOnly {
		// The main handle was opened read-only (MCP: store.OpenReadOnly,
		// _query_only=true). The backfill WRITES messages.embed_gen and the
		// applied_migrations ledger, which the query-only handle rejects.
		// Skip it entirely — mirrors pgvector's SkipMigrate read-only guard.
		// A write-path process (serve, embeddings CLI) runs the backfill
		// instead.
		return nil
	}

	// A main DB without applied_migrations is not a real msgvault store
	// (e.g. a hand-rolled test fixture or a DB opened before the store
	// schema ran); skip the backfill entirely rather than fail Open.
	hasLedger, err := mainTableExists(ctx, b.mainDB, "applied_migrations")
	if err != nil {
		return err
	}
	if !hasLedger {
		return nil
	}

	applied, err := b.backfillApplied(ctx)
	if err != nil {
		return err
	}
	if applied {
		return nil
	}

	// Resolve the active generation. No active generation means nothing to
	// backfill — but we still mark the migration applied so a later
	// just-activated generation does not retroactively trigger a backfill
	// that re-stamps rows repair-encoding may have reset. The active gen at
	// upgrade time is the only one whose pre-existing embeddings predate the
	// embed_gen column; generations created after upgrade are stamped by the
	// worker as it embeds.
	//
	// Intentional scope limit: only the ACTIVE generation is backfilled. Any
	// BUILDING generation that existed pre-upgrade is left unstamped — a
	// resumed rebuild idempotently re-embeds that bounded portion (scan-and-
	// fill skips already-covered rows), so the cost is small and one-time.
	// Per-generation backfill complexity is not worth it for a single-user
	// tool.
	active, err := b.ActiveGeneration(ctx)
	if err != nil {
		if errors.Is(err, vector.ErrNoActiveGeneration) {
			return b.markBackfillApplied(ctx)
		}
		return fmt.Errorf("backfill: resolve active generation: %w", err)
	}

	// Distinct message ids that already have an embedding row for the active
	// generation, read from vectors.db.
	rows, err := b.db.QueryContext(ctx,
		`SELECT DISTINCT message_id FROM embeddings WHERE generation_id = ?`,
		int64(active.ID))
	if err != nil {
		return fmt.Errorf("backfill: list embedded message ids: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return fmt.Errorf("backfill: scan embedded message id: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("backfill: iterate embedded message ids: %w", err)
	}

	// Stamp embed_gen=active for those ids on main.db, but only where it is
	// still NULL — never overwrite a row already stamped for a different
	// generation. Chunked to stay under the bind limit.
	for start := 0; start < len(ids); start += backfillStampChunk {
		end := min(start+backfillStampChunk, len(ids))
		chunk := ids[start:end]
		placeholders := make([]string, len(chunk))
		args := make([]any, 0, 1+len(chunk))
		args = append(args, int64(active.ID))
		for i, id := range chunk {
			placeholders[i] = "?"
			args = append(args, id)
		}
		q := `UPDATE messages SET embed_gen = ?
		       WHERE embed_gen IS NULL
		         AND id IN (` + strings.Join(placeholders, ",") + `)`
		if _, err := b.mainDB.ExecContext(ctx, q, args...); err != nil {
			return fmt.Errorf("backfill: stamp embed_gen: %w", err)
		}
	}

	return b.markBackfillApplied(ctx)
}

// backfillApplied reports whether the one-time backfill ledger row exists
// in main.db. A missing applied_migrations table (older main schema) is
// treated as "not applied" — the table is created by the store schema, so
// this only matters in unusual test setups; the markBackfillApplied write
// would then surface the real error.
func (b *Backend) backfillApplied(ctx context.Context) (bool, error) {
	var n int
	if err := b.mainDB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM applied_migrations WHERE name = ?`,
		embedGenBackfillMigration).Scan(&n); err != nil {
		return false, fmt.Errorf("backfill: check ledger: %w", err)
	}
	return n > 0, nil
}

// markBackfillApplied records the one-time backfill in main.db's ledger.
// INSERT OR IGNORE keeps it idempotent under a concurrent Open.
func (b *Backend) markBackfillApplied(ctx context.Context) error {
	if _, err := b.mainDB.ExecContext(ctx,
		`INSERT OR IGNORE INTO applied_migrations (name) VALUES (?)`,
		embedGenBackfillMigration); err != nil {
		return fmt.Errorf("backfill: mark ledger: %w", err)
	}
	return nil
}

// mainTableExists asks sqlite_master in db (the MAIN db, distinct from
// vectors.db) whether a regular or virtual table named `name` exists.
func mainTableExists(ctx context.Context, db *sql.DB, name string) (bool, error) {
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type IN ('table','virtual') AND name = ?`,
		name).Scan(&n); err != nil {
		return false, fmt.Errorf("backfill: probe %s: %w", name, err)
	}
	return n > 0, nil
}
