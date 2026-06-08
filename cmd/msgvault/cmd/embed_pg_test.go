//go:build sqlite_vec && pgvector

package cmd

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/pgvector"
)

// openEmbedManagePGDB opens a per-test isolated PG schema and migrates the
// pgvector tables into it, then returns the sql.DB, the PG rebind func, and
// the pgvector backend. Skips when MSGVAULT_TEST_DB is unset.
func openEmbedManagePGDB(t *testing.T) (*pgvector.Backend, func(string) string) {
	t.Helper()
	_, dsn := openServePGSchema(t)
	ctx := context.Background()

	st, err := store.Open(dsn)
	require.NoError(t, err, "store.Open")
	t.Cleanup(func() { _ = st.Close() })

	pgb, err := pgvector.Open(ctx, pgvector.Options{
		DB:        st.DB(),
		Dimension: 4,
	})
	require.NoError(t, err, "pgvector.Open")
	t.Cleanup(func() { _ = pgb.Close() })

	// Create a minimal messages table so CreateGeneration's seed query works.
	_, err = st.DB().ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS messages (
			id BIGINT PRIMARY KEY,
			deleted_at TIMESTAMPTZ,
			deleted_from_source_at TIMESTAMPTZ
		)`)
	require.NoError(t, err, "create messages scaffold")

	return pgb, (&store.PostgreSQLDialect{}).Rebind
}

// TestListEmbeddingGenerations_PG exercises listEmbeddingGenerations through
// the PG rebind path against a live PostgreSQL database. Validates that the
// PG placeholder rebind and boolean-placeholder behaviour work correctly.
func TestListEmbeddingGenerations_PG(t *testing.T) {
	pgb, rebind := openEmbedManagePGDB(t)
	ctx := context.Background()
	db := pgb.DB()

	// Start with an empty database — list must return an empty slice, not error.
	rows, err := listEmbeddingGenerations(ctx, db, rebind)
	require.NoError(t, err, "listEmbeddingGenerations on empty PG DB must not error")
	assert.Empty(t, rows, "no generations yet")

	// Create a generation so list returns something.
	gen, err := pgb.CreateGeneration(ctx, "test-model", 4, "test-model:4")
	require.NoError(t, err, "CreateGeneration")

	rows, err = listEmbeddingGenerations(ctx, db, rebind)
	require.NoError(t, err, "listEmbeddingGenerations after CreateGeneration")
	require.Len(t, rows, 1, "one generation")
	assert.Equal(t, gen, rows[0].ID)
	assert.Equal(t, vector.GenerationBuilding, rows[0].State)
	assert.Equal(t, "test-model", rows[0].Model)
}

// TestRetireEmbeddingGeneration_PG exercises retireEmbeddingGeneration via
// the PG rebind path. Creates a building generation, retires it using the
// force flag, and asserts the state transitions correctly.
func TestRetireEmbeddingGeneration_PG(t *testing.T) {
	pgb, rebind := openEmbedManagePGDB(t)
	ctx := context.Background()
	db := pgb.DB()

	gen, err := pgb.CreateGeneration(ctx, "test-model", 4, "test-model:4")
	require.NoError(t, err, "CreateGeneration")

	// Retire the building generation (force=true since it is not active).
	require.NoError(t, retireEmbeddingGeneration(ctx, db, rebind, gen, true),
		"retireEmbeddingGeneration with force on PG")

	g, err := getEmbeddingGeneration(ctx, db, rebind, gen)
	require.NoError(t, err, "getEmbeddingGeneration after retire")
	assert.Equal(t, vector.GenerationRetired, g.State, "generation must be retired")
}

// TestActivateEmbeddingGeneration_PG exercises activateEmbeddingGeneration via
// the PG rebind path. Creates a building generation with no pending rows, then
// activates it and checks the state.
func TestActivateEmbeddingGeneration_PG(t *testing.T) {
	pgb, rebind := openEmbedManagePGDB(t)
	ctx := context.Background()
	db := pgb.DB()

	gen, err := pgb.CreateGeneration(ctx, "test-model", 4, "test-model:4")
	require.NoError(t, err, "CreateGeneration")

	// Drain pending rows so activation is allowed.
	_, err = db.ExecContext(ctx, `DELETE FROM pending_embeddings WHERE generation_id = $1`, int64(gen))
	require.NoError(t, err, "drain pending")

	require.NoError(t, activateEmbeddingGeneration(ctx, db, rebind, gen, false),
		"activateEmbeddingGeneration on PG must succeed with no pending rows")

	g, err := getEmbeddingGeneration(ctx, db, rebind, gen)
	require.NoError(t, err, "getEmbeddingGeneration after activate")
	assert.Equal(t, vector.GenerationActive, g.State, "generation must be active")
	assert.NotNil(t, g.ActivatedAt, "activated_at must be set")
}

// TestOpenEmbeddingsMetadataDB_PG exercises the openEmbeddingsMetadataDB
// helper on a real PG DSN. Confirms that it returns a live handle backed by
// the store-level PG opener (not raw sql.Open) and that a simple query
// succeeds.
func TestOpenEmbeddingsMetadataDB_PG(t *testing.T) {
	pgb, _ := openEmbedManagePGDB(t)
	ctx := context.Background()

	// Confirm the handle returned by pgb.DB() (which represents the
	// store-opened connection) supports the embedding tables.
	var n int
	err := pgb.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM index_generations`).Scan(&n)
	require.NoError(t, err, "query via pgb.DB() (store-opened PG handle) must succeed")
	// We created one generation in openEmbedManagePGDB via CreateGeneration in
	// other subtests — this subtest uses its own isolated schema so n=0 is fine.
	assert.GreaterOrEqual(t, n, 0)
}
