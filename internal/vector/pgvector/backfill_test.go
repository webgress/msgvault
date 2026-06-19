//go:build pgvector

package pgvector

import (
	"context"
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/vector"
)

// embedGenOf reads embed_gen for a message, reporting whether it is NULL.
func embedGenOf(t *testing.T, db *sql.DB, id int64) (val int64, isNull bool) {
	t.Helper()
	var v sql.NullInt64
	require.NoError(t, db.QueryRow(`SELECT embed_gen FROM messages WHERE id = $1`, id).Scan(&v))
	return v.Int64, !v.Valid
}

// TestBackfillEmbedGen_UpgradeStampsEmbeddedOnly mirrors the sqlitevec FIX
// B test on PostgreSQL: an active generation already has embeddings for
// some messages but embed_gen is NULL everywhere (the ADD COLUMN did no
// backfill). The one-time backfill stamps embed_gen=active for the embedded
// messages and leaves the un-embedded one NULL; coverage becomes honest;
// re-running is a ledger-guarded no-op.
func TestBackfillEmbedGen_UpgradeStampsEmbeddedOnly(t *testing.T) {
	ctx := context.Background()
	db := openPGTestDB(t)
	// The minimal PG test schema omits applied_migrations; create it so the
	// ledger guard has somewhere to record.
	_, err := db.Exec(`CREATE TABLE applied_migrations (
		name TEXT PRIMARY KEY,
		applied_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
	)`)
	require.NoError(t, err, "create applied_migrations")

	// 3 messages: 1 and 2 embedded under the active gen, 3 not.
	for _, id := range []int64{1, 2, 3} {
		_, err := db.Exec(`INSERT INTO messages (id) VALUES ($1)`, id)
		require.NoError(t, err, "insert message")
	}

	b, err := Open(ctx, Options{DB: db, Dimension: 4})
	require.NoError(t, err, "Open")
	t.Cleanup(func() { _ = b.Close() })

	gen, err := b.CreateGeneration(ctx, "fake", 4, "")
	require.NoError(t, err, "CreateGeneration")

	chunks := []vector.Chunk{
		{MessageID: 1, Vector: []float32{1, 0, 0, 0}},
		{MessageID: 2, Vector: []float32{0, 1, 0, 0}},
	}
	require.NoError(t, b.Upsert(ctx, gen, chunks), "Upsert")

	// Stamp + activate, then simulate the upgrade by resetting embed_gen.
	_, err = db.ExecContext(ctx, `UPDATE messages SET embed_gen = $1`, int64(gen))
	require.NoError(t, err, "stamp")
	require.NoError(t, b.ActivateGeneration(ctx, gen, true), "activate (force)")
	_, err = db.ExecContext(ctx, `UPDATE messages SET embed_gen = NULL`)
	require.NoError(t, err, "reset embed_gen to NULL (simulate upgrade)")

	// Open already ran (and marked) the backfill at open time when no gen
	// existed; clear the ledger so this call reproduces the real upgrade
	// timing (first Open where an active gen + embeddings are present).
	_, err = db.ExecContext(ctx,
		`DELETE FROM applied_migrations WHERE name = $1`, embedGenBackfillMigration)
	require.NoError(t, err, "reset ledger")

	require.NoError(t, b.BackfillEmbedGenForUpgrade(ctx), "backfill")

	for _, id := range []int64{1, 2} {
		v, isNull := embedGenOf(t, db, id)
		assert.Falsef(t, isNull, "msg %d should be stamped", id)
		assert.Equalf(t, int64(gen), v, "msg %d embed_gen", id)
	}
	_, isNull3 := embedGenOf(t, db, 3)
	assert.True(t, isNull3, "msg 3 (un-embedded) stays NULL")

	// Coverage is honest: only message 3 missing.
	s, err := b.Stats(ctx, gen)
	require.NoError(t, err, "Stats")
	assert.Equal(t, int64(1), s.PendingCount, "post-backfill: only msg 3 missing")

	// Re-running is a ledger-guarded no-op: msg 3 stays NULL.
	require.NoError(t, b.BackfillEmbedGenForUpgrade(ctx), "backfill again (no-op)")
	_, isNull3Again := embedGenOf(t, db, 3)
	assert.True(t, isNull3Again, "msg 3 still NULL after second backfill (ledger no-op)")
}

// TestBackfillEmbedGen_StampAndMarkAtomic_RollbackOnMarkFailure is the
// PostgreSQL companion to the sqlitevec atomicity guard (Codex 129d #2/#3):
// the embed_gen stamp UPDATE and the applied_migrations ledger mark must be
// ONE transaction. messages and applied_migrations share b.db on PG, so a
// single tx covers both.
//
// Fault injection: a BEFORE INSERT trigger on applied_migrations RAISEs an
// exception when the backfill ledger row is inserted, so the mark step fails
// AFTER the embed_gen UPDATE has run inside the same tx. If atomic, the UPDATE
// must be ROLLED BACK (embed_gen stays NULL) and the ledger must stay UNMARKED,
// so a later clean backfill re-runs and completes.
func TestBackfillEmbedGen_StampAndMarkAtomic_RollbackOnMarkFailure(t *testing.T) {
	ctx := context.Background()
	db := openPGTestDB(t)
	_, err := db.Exec(`CREATE TABLE applied_migrations (
		name TEXT PRIMARY KEY,
		applied_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
	)`)
	require.NoError(t, err, "create applied_migrations")

	_, err = db.Exec(`INSERT INTO messages (id) VALUES (1)`)
	require.NoError(t, err, "insert message")

	b, err := Open(ctx, Options{DB: db, Dimension: 4})
	require.NoError(t, err, "Open")
	t.Cleanup(func() { _ = b.Close() })

	gen, err := b.CreateGeneration(ctx, "fake", 4, "")
	require.NoError(t, err, "CreateGeneration")
	require.NoError(t, b.Upsert(ctx, gen, []vector.Chunk{
		{MessageID: 1, Vector: []float32{1, 0, 0, 0}},
	}), "Upsert")
	require.NoError(t, b.ActivateGeneration(ctx, gen, true), "Activate")

	// Pre-upgrade state: embed_gen NULL, ledger cleared (Open already marked
	// it when no gen existed).
	_, err = db.ExecContext(ctx, `UPDATE messages SET embed_gen = NULL`)
	require.NoError(t, err, "reset embed_gen")
	_, err = db.ExecContext(ctx,
		`DELETE FROM applied_migrations WHERE name = $1`, embedGenBackfillMigration)
	require.NoError(t, err, "clear ledger")

	// Install a fault that makes ONLY the ledger mark fail. The embed_gen
	// UPDATE on messages still succeeds, so a non-atomic implementation would
	// leave embed_gen stamped while the ledger stays unmarked.
	_, err = db.Exec(`CREATE FUNCTION zz_fail_backfill_mark() RETURNS trigger AS $fn$
		BEGIN
			IF NEW.name = '` + embedGenBackfillMigration + `' THEN
				RAISE EXCEPTION 'injected backfill mark failure';
			END IF;
			RETURN NEW;
		END;
		$fn$ LANGUAGE plpgsql`)
	require.NoError(t, err, "create fault function")
	_, err = db.Exec(`CREATE TRIGGER zz_fail_backfill_mark
		BEFORE INSERT ON applied_migrations
		FOR EACH ROW EXECUTE FUNCTION zz_fail_backfill_mark()`)
	require.NoError(t, err, "install fault trigger")

	err = b.BackfillEmbedGenForUpgrade(ctx)
	require.Error(t, err, "backfill must surface the injected ledger-mark failure")
	assert.Contains(t, err.Error(), "injected backfill mark failure")

	// Atomicity: the stamp must have been ROLLED BACK with the failed mark.
	_, isNull := embedGenOf(t, db, 1)
	assert.True(t, isNull,
		"embed_gen must be rolled back to NULL when the ledger mark fails (atomic)")
	var marked int
	require.NoError(t, db.QueryRow(
		`SELECT COUNT(*) FROM applied_migrations WHERE name = $1`,
		embedGenBackfillMigration).Scan(&marked))
	assert.Equal(t, 0, marked, "ledger must stay unmarked when the backfill tx rolls back")

	// Recovery: remove the fault and re-run. The migration was never marked,
	// so the one-time backfill re-runs cleanly and now completes.
	_, err = db.Exec(`DROP TRIGGER zz_fail_backfill_mark ON applied_migrations`)
	require.NoError(t, err, "drop fault trigger")
	require.NoError(t, b.BackfillEmbedGenForUpgrade(ctx), "clean re-run must succeed")

	v, isNull := embedGenOf(t, db, 1)
	assert.False(t, isNull, "embed_gen stamped after clean re-run")
	assert.Equal(t, int64(gen), v, "embed_gen references the active generation")
	require.NoError(t, db.QueryRow(
		`SELECT COUNT(*) FROM applied_migrations WHERE name = $1`,
		embedGenBackfillMigration).Scan(&marked))
	assert.Equal(t, 1, marked, "ledger marked after clean re-run")
}

// TestResetOrphanedEmbedGen_RecreateScenario mirrors the sqlitevec recreate
// test on PostgreSQL: messages carry stamps for a generation id that no longer
// exists in index_generations (a partial restore — messages restored, the
// generation row not). A writable Open must reset those orphaned stamps to NULL
// so coverage reports them missing rather than masking an empty index.
func TestResetOrphanedEmbedGen_RecreateScenario(t *testing.T) {
	ctx := context.Background()
	db := openPGTestDB(t)

	for _, id := range []int64{1, 2} {
		_, err := db.Exec(`INSERT INTO messages (id) VALUES ($1)`, id)
		require.NoError(t, err, "insert message")
	}

	// Open (creates the empty index_generations), then stamp both messages
	// for a generation id (99) that does not exist — the orphan condition.
	b, err := Open(ctx, Options{DB: db, Dimension: 4})
	require.NoError(t, err, "Open")
	t.Cleanup(func() { _ = b.Close() })
	_, err = db.ExecContext(ctx, `UPDATE messages SET embed_gen = 99`)
	require.NoError(t, err, "stamp orphaned embed_gen=99")

	// Re-open writable: the reset runs and clears the orphaned stamps.
	b2, err := Open(ctx, Options{DB: db, Dimension: 4})
	require.NoError(t, err, "re-Open (writable)")
	t.Cleanup(func() { _ = b2.Close() })

	for _, id := range []int64{1, 2} {
		_, isNull := embedGenOf(t, db, id)
		assert.Truef(t, isNull, "msg %d embed_gen reset to NULL (orphaned)", id)
	}
}

// TestResetOrphanedEmbedGen_NoFalsePositive verifies the PG reset PRESERVES
// stamps that reference a still-existing generation row (active or retired —
// retire only flips state on PG, it does not delete the index_generations row).
func TestResetOrphanedEmbedGen_NoFalsePositive(t *testing.T) {
	ctx := context.Background()
	db := openPGTestDB(t)

	for _, id := range []int64{1, 2} {
		_, err := db.Exec(`INSERT INTO messages (id) VALUES ($1)`, id)
		require.NoError(t, err, "insert message")
	}

	b, err := Open(ctx, Options{DB: db, Dimension: 4})
	require.NoError(t, err, "Open")
	t.Cleanup(func() { _ = b.Close() })

	gen, err := b.CreateGeneration(ctx, "fake", 4, "")
	require.NoError(t, err, "CreateGeneration")
	require.NoError(t, b.Upsert(ctx, gen, []vector.Chunk{
		{MessageID: 1, Vector: []float32{1, 0, 0, 0}},
		{MessageID: 2, Vector: []float32{0, 1, 0, 0}},
	}), "Upsert")
	_, err = db.ExecContext(ctx, `UPDATE messages SET embed_gen = $1`, int64(gen))
	require.NoError(t, err, "stamp")
	require.NoError(t, b.ActivateGeneration(ctx, gen, true), "Activate")

	// Re-open writable: gen still exists, so its stamps must be preserved.
	b2, err := Open(ctx, Options{DB: db, Dimension: 4})
	require.NoError(t, err, "re-Open (writable)")
	t.Cleanup(func() { _ = b2.Close() })

	for _, id := range []int64{1, 2} {
		v, isNull := embedGenOf(t, db, id)
		assert.Falsef(t, isNull, "msg %d stamp preserved (gen still exists)", id)
		assert.Equalf(t, int64(gen), v, "msg %d embed_gen preserved", id)
	}
}

// TestResetOrphanedEmbedGen_SkipMigrate_Skipped verifies the orphaned-stamp
// reset is suppressed on the read-only (SkipMigrate) Open path, where writes
// are rejected. An orphaned stamp must be left untouched.
func TestResetOrphanedEmbedGen_SkipMigrate_Skipped(t *testing.T) {
	ctx := context.Background()
	db := openPGTestDB(t)

	_, err := db.Exec(`INSERT INTO messages (id) VALUES (1)`)
	require.NoError(t, err, "insert message")

	// Bring up the schema (index_generations etc.) via a writable Open, then
	// stamp an orphaned embed_gen.
	b, err := Open(ctx, Options{DB: db, Dimension: 4})
	require.NoError(t, err, "Open (writable, migrate)")
	t.Cleanup(func() { _ = b.Close() })
	_, err = db.ExecContext(ctx, `UPDATE messages SET embed_gen = 99`)
	require.NoError(t, err, "stamp orphaned embed_gen=99")

	// SkipMigrate Open (MCP read-only path): the reset must be skipped.
	b2, err := Open(ctx, Options{DB: db, Dimension: 4, SkipMigrate: true})
	require.NoError(t, err, "Open (SkipMigrate) must not error")
	t.Cleanup(func() { _ = b2.Close() })

	v, isNull := embedGenOf(t, db, 1)
	assert.False(t, isNull, "SkipMigrate Open must NOT reset the orphaned embed_gen")
	assert.Equal(t, int64(99), v, "orphaned stamp unchanged under SkipMigrate Open")
}
