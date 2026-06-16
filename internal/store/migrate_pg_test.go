package store_test

import (
	"context"
	"strings"
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

// TestMigratePGEmptyTableSequenceStartsAtOne (adjacent / M5) asserts the 3-arg
// setval fix: after migrating a source where an identity-PK table is EMPTY, the
// destination PG sequence is left UNCALLED at 1 so the very first insert yields
// id=1 (the old 2-arg COALESCE(MAX(id),1) form would skip id=1). reactions is
// empty in buildSourceVault, so its sequence must hand out id=1 first.
func TestMigratePGEmptyTableSequenceStartsAtOne(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := context.Background()

	pg := testutil.NewPostgresTestStore(t) // skips if no PG env
	sqliteSrc := testutil.NewSQLiteTestStore(t)

	pop := buildSourceVault(t, sqliteSrc)
	clearDefaultCollection(t, pg)

	// Sanity: reactions is empty in the fixture.
	require.Equal(int64(0), scanInt(t, sqliteSrc, "SELECT COUNT(*) FROM reactions"),
		"fixture reactions empty")

	_, err := store.MigrateVault(ctx, sqliteSrc, pg, store.MigrateOptions{Batch: 50})
	require.NoError(err, "migrate sqlite->pg")

	// Insert a reaction through raw SQL using the identity default (no explicit
	// id) and assert it gets id=1.
	var newID int64
	require.NoError(pg.DB().QueryRowContext(ctx,
		"INSERT INTO reactions (message_id, participant_id, reaction_type, reaction_value) "+
			"VALUES ($1,$2,$3,$4) RETURNING id",
		pop.rootMsgID, pop.aliceID, "like", "👍").Scan(&newID),
		"insert reaction on empty table")
	assert.Equal(int64(1), newID, "first insert into empty migrated table must be id=1")

	// And a verify of the empty-table sequence must be OK (effective next > max,
	// trivially true for an empty table).
	vr, err := store.VerifyMigration(ctx, sqliteSrc, pg)
	require.NoError(err, "VerifyMigration")
	// The reaction we just inserted makes pg's reactions count differ from the
	// source, which is an expected row-count problem — but there must be NO
	// "reactions sequence behind max id" problem.
	for _, p := range vr.Problems {
		assert.NotContains(p, "reactions sequence behind max id",
			"empty-table sequence must not be reported behind max")
	}
}

// TestMigratePGNonEmptySequenceVerify (M5) asserts the verify sequence check
// uses effective-next (last_value + is_called) and passes after a normal
// migration, and FAILS when the sequence is deliberately rewound below MAX(id).
func TestMigratePGNonEmptySequenceVerify(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := context.Background()

	pg := testutil.NewPostgresTestStore(t)
	sqliteSrc := testutil.NewSQLiteTestStore(t)

	buildSourceVault(t, sqliteSrc)
	clearDefaultCollection(t, pg)

	_, err := store.MigrateVault(ctx, sqliteSrc, pg, store.MigrateOptions{Batch: 50})
	require.NoError(err, "migrate sqlite->pg")
	rebuildFTSForTest(t, pg)

	// Healthy migration verifies clean.
	vr, err := store.VerifyMigration(ctx, sqliteSrc, pg)
	require.NoError(err, "VerifyMigration")
	assert.Truef(vr.OK(), "verify problems: %v", vr.Problems)

	// Rewind the messages identity sequence below MAX(id): set it UNCALLED at 1
	// so effective next = 1, which is <= the copied max. Verify must flag it.
	_, err = pg.DB().ExecContext(ctx,
		"SELECT setval(pg_get_serial_sequence('messages','id'), 1, false)")
	require.NoError(err, "rewind messages sequence")

	vr2, err := store.VerifyMigration(ctx, sqliteSrc, pg)
	require.NoError(err, "VerifyMigration after rewind")
	assert.False(vr2.OK(), "verify must fail with sequence behind max")
	found := false
	for _, p := range vr2.Problems {
		if strings.Contains(p, "messages sequence behind max id") {
			found = true
		}
	}
	assert.Truef(found, "expected a messages sequence problem, got: %v", vr2.Problems)
}

// TestPGOrphanChecksCoverAllFKEdges (M4) asserts the dynamic, catalog-driven FK
// enumeration covers ALL foreign-key edges in the destination schema — not the
// old hand-curated ~6 — including edges the hardcoded subset omitted.
func TestPGOrphanChecksCoverAllFKEdges(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := context.Background()

	pg := testutil.NewPostgresTestStore(t)

	edges, err := store.PGForeignKeyEdgesForTest(ctx, pg)
	require.NoError(err, "enumerate FK edges")

	// The PG schema declares 24 FK column edges; the dynamic check must cover
	// them all (one probe per column edge).
	assert.GreaterOrEqual(len(edges), 24,
		"dynamic FK enumeration must cover every edge, got %d: %v", len(edges), edges)

	// Spot-check that edges the OLD hardcoded subset did NOT include are now
	// covered (proving completeness beyond the original 6).
	for _, want := range []string{
		"conversation_participants.conversation_id->conversations.id",
		"conversation_participants.participant_id->participants.id",
		"account_identities.source_id->sources.id",
		"collection_sources.collection_id->collections.id",
		"sync_runs.source_id->sources.id",
		"message_bodies.message_id->messages.id",
	} {
		assert.Containsf(edges, want, "missing FK edge %q from dynamic coverage", want)
	}
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
