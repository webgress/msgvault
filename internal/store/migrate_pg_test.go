package store_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

// TestMigrateSQLiteToPostgresRoundTrip exercises the full cross-backend path:
// build a populated SQLite vault, migrate it to PostgreSQL (asserting verify
// passes, sample ids identical, and the PG identity sequence advanced past the
// copied max), then migrate that PostgreSQL vault back into a fresh SQLite vault
// and assert counts/ids round-trip. FTS search returns hits in each direction.
//
// Skipped cleanly when MSGVAULT_TEST_DB is not a PostgreSQL DSN.
func TestMigrateSQLiteToPostgresRoundTrip(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := context.Background()

	pg := testutil.NewPostgresTestStore(t) // skips if no PG env
	sqliteSrc := testutil.NewSQLiteTestStore(t)

	want := buildSourceVault(t, sqliteSrc)
	clearDefaultCollection(t, pg)

	// --- Leg 1: SQLite -> PostgreSQL ---
	_, err := store.MigrateVault(ctx, sqliteSrc, pg, store.MigrateOptions{Batch: 2})
	require.NoError(err, "migrate sqlite->pg")
	rebuildFTSForTest(t, pg)

	// IDs preserved verbatim on PG.
	assert.Equal(want.rootMsgID, scanInt(t, pg, "SELECT id FROM messages WHERE source_message_id = 'm1'"))
	assert.Equal(want.replyMsgID, scanInt(t, pg, "SELECT id FROM messages WHERE source_message_id = 'm2'"))
	// Two-pass self-FK applied.
	assert.Equal(want.rootMsgID,
		scanInt(t, pg, "SELECT reply_to_message_id FROM messages WHERE source_message_id = 'm2'"))
	// JSON survived the JSONB cast.
	assert.Contains(scanString(t, pg, "SELECT sync_config::text FROM sources WHERE id = ?", want.sourceID), "all")

	vrPG, err := store.VerifyMigration(ctx, sqliteSrc, pg)
	require.NoError(err, "verify sqlite->pg")
	assert.Truef(vrPG.OK(), "pg verify problems: %v", vrPG.Problems)

	// PG sequence advanced: a normal insert through the store path must land
	// above the largest copied id (no collision with preserved ids).
	newSrc, err := pg.GetOrCreateSource("gmail", "carol@example.com")
	require.NoError(err, "insert new source on pg")
	assert.Greater(newSrc.ID, want.sourceID, "new pg source id above copied max")

	// FTS search works on PG.
	pgHits := searchHitIDs(t, pg, "invoice")
	assert.Contains(pgHits, want.replyMsgID, "pg FTS finds the reply body term")

	// --- Leg 2: PostgreSQL -> fresh SQLite ---
	sqliteDst := testutil.NewSQLiteTestStore(t)
	clearDefaultCollection(t, sqliteDst)
	_, err = store.MigrateVault(ctx, pg, sqliteDst, store.MigrateOptions{Batch: 3})
	require.NoError(err, "migrate pg->sqlite")
	rebuildFTSForTest(t, sqliteDst)

	// Counts/ids round-trip back to SQLite. Note: pg now has the extra carol
	// source we inserted above, so compare pg<->sqliteDst (the actual copy).
	vrBack, err := store.VerifyMigration(ctx, pg, sqliteDst)
	require.NoError(err, "verify pg->sqlite")
	assert.Truef(vrBack.OK(), "back verify problems: %v", vrBack.Problems)

	// Original message ids survive the full round trip.
	assert.Equal(want.rootMsgID,
		scanInt(t, sqliteDst, "SELECT id FROM messages WHERE source_message_id = 'm1'"))
	assert.Equal(want.rootMsgID,
		scanInt(t, sqliteDst, "SELECT reply_to_message_id FROM messages WHERE source_message_id = 'm2'"))

	// FTS search works on the round-tripped SQLite vault.
	backHits := searchHitIDs(t, sqliteDst, "invoice")
	assert.Contains(backHits, want.replyMsgID, "sqlite FTS finds the reply body term after round trip")
}

// searchHitIDs returns the message ids matching term via the public FTS search
// path (dialect-agnostic).
func searchHitIDs(t *testing.T, st *store.Store, term string) []int64 {
	t.Helper()
	msgs, _, err := st.SearchMessages(term, 0, 100)
	require.NoError(t, err, "FTS search")
	ids := make([]int64, len(msgs))
	for i, m := range msgs {
		ids[i] = m.ID
	}
	return ids
}
