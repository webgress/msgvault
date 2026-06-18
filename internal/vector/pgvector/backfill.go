//go:build pgvector

package pgvector

import (
	"context"
	"errors"
	"fmt"

	"go.kenn.io/msgvault/internal/vector"
)

// embedGenBackfillMigration is the applied_migrations ledger key that
// guards the one-time embed_gen upgrade backfill. Stable string — never
// change it, or the backfill would re-run on every Open.
const embedGenBackfillMigration = "embed_gen_backfill_active_v1"

// BackfillEmbedGenForUpgrade performs the ONE-TIME upgrade backfill
// (Package A): when an active generation exists, it stamps embed_gen=active
// on every message that already has >=1 embedding row under that generation
// but whose embed_gen is still NULL.
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
//     reachable here.
//   - The stamp UPDATE only touches rows where embed_gen IS NULL, so it
//     never overwrites a row already stamped for another generation.
//
// No-ops cleanly when the ledger already records it, there is no active
// generation, the embeddings table is empty, or there are no
// embedded-but-unstamped rows. Idempotent.
//
// Single DB on PostgreSQL: messages, embeddings, and the ledger all share
// b.db, so the backfill is one EXISTS-correlated UPDATE.
func (b *Backend) BackfillEmbedGenForUpgrade(ctx context.Context) error {
	// A database without applied_migrations is not a real msgvault store
	// (e.g. a minimal test fixture, or a DB opened before the store schema
	// ran); skip the backfill entirely rather than fail Open.
	var ledger *string
	if err := b.db.QueryRowContext(ctx,
		`SELECT to_regclass('applied_migrations')::text`).Scan(&ledger); err != nil {
		return fmt.Errorf("backfill: probe ledger: %w", err)
	}
	if ledger == nil {
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
	// that re-stamps rows repair-encoding may have reset.
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

	// Stamp embed_gen=active for messages with an embedding row under the
	// active generation, only where embed_gen is still NULL (never overwrite
	// a row stamped for another generation).
	if _, err := b.db.ExecContext(ctx,
		`UPDATE messages SET embed_gen = $1
		  WHERE embed_gen IS NULL
		    AND EXISTS (
		        SELECT 1 FROM embeddings e
		         WHERE e.message_id = messages.id
		           AND e.generation_id = $1)`,
		int64(active.ID)); err != nil {
		return fmt.Errorf("backfill: stamp embed_gen: %w", err)
	}

	return b.markBackfillApplied(ctx)
}

// backfillApplied reports whether the one-time backfill ledger row exists.
func (b *Backend) backfillApplied(ctx context.Context) (bool, error) {
	var n int
	if err := b.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM applied_migrations WHERE name = $1`,
		embedGenBackfillMigration).Scan(&n); err != nil {
		return false, fmt.Errorf("backfill: check ledger: %w", err)
	}
	return n > 0, nil
}

// markBackfillApplied records the one-time backfill in the ledger. ON
// CONFLICT DO NOTHING keeps it idempotent under a concurrent Open.
func (b *Backend) markBackfillApplied(ctx context.Context) error {
	if _, err := b.db.ExecContext(ctx,
		`INSERT INTO applied_migrations (name) VALUES ($1) ON CONFLICT DO NOTHING`,
		embedGenBackfillMigration); err != nil {
		return fmt.Errorf("backfill: mark ledger: %w", err)
	}
	return nil
}
