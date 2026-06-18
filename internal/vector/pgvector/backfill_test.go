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
