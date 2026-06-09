//go:build sqlite_vec && pgvector

package cmd

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"os"
	"strings"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/pgvector"
)

// openEmbedManagePGDB opens a per-test isolated PG schema, migrates the
// pgvector tables into it (via pgvector.Open), and returns the pgvector
// backend and the PG rebind func. The underlying *sql.DB is reachable via
// the returned backend's DB() method. Skips when MSGVAULT_TEST_DB is unset.
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

// TestOpenEmbeddingsMetadataDB_PG exercises the real openEmbeddingsMetadataDB
// helper against a live PG DSN. It pins the PG branch's contract: the helper
// routes through store.OpenPostgresDB (not raw sql.Open), returns the PG
// rebind (not the SQLite identity rebind), and yields a live handle that the
// production query helpers can use. The cfg-global swap mirrors
// TestSetupVectorFeatures_SucceedsOnPostgres.
func TestOpenEmbeddingsMetadataDB_PG(t *testing.T) {
	// Stand up an isolated schema and migrate the pgvector metadata tables
	// into it so the helper's existence pre-check passes.
	db, dsn := openServePGSchema(t)
	ctx := context.Background()
	require.NoError(t, pgvector.Migrate(ctx, db, 4), "pgvector.Migrate")

	savedCfg := cfg
	defer func() { cfg = savedCfg }()
	cfg = &config.Config{}
	cfg.Data.DatabaseURL = dsn

	mdb, rebind, closeDB, err := openEmbeddingsMetadataDB()
	require.NoError(t, err, "openEmbeddingsMetadataDB on a migrated PG schema must succeed")
	require.NotNil(t, mdb, "metadata DB handle")
	require.NotNil(t, closeDB, "close callback")
	defer closeDB()

	// The PG branch must return the PG rebind, not the SQLite identity rebind.
	assert.Equal(t, "$1", rebind("?"), "PG rebind must convert ? to $1")

	// The handle must be live and usable by the production query helper.
	rows, err := listEmbeddingGenerations(ctx, mdb, rebind)
	require.NoError(t, err, "listEmbeddingGenerations via openEmbeddingsMetadataDB handle")
	assert.Empty(t, rows, "freshly migrated schema has no generations yet")
}

// TestOpenEmbeddingsMetadataDB_PG_FriendlyErrorWhenUnmigrated pins
// cmd-glue-stubs-2: on a PG deployment where the vector schema has not been
// migrated (no embed run yet), openEmbeddingsMetadataDB must return a
// friendly, build-pointing error rather than leaking a raw
// `relation "index_generations" does not exist (SQLSTATE 42P01)`.
func TestOpenEmbeddingsMetadataDB_PG_FriendlyErrorWhenUnmigrated(t *testing.T) {
	// Use a search_path scoped to ONLY the fresh isolated schema (no
	// "public") so to_regclass cannot resolve against tables that prior
	// non-isolated test runs may have left in public — the schema genuinely
	// has no metadata tables, matching an un-migrated PG deployment.
	dsn := openEmptyPGSchemaSolo(t)

	savedCfg := cfg
	defer func() { cfg = savedCfg }()
	cfg = &config.Config{}
	cfg.Data.DatabaseURL = dsn

	_, _, closeDB, err := openEmbeddingsMetadataDB()
	if closeDB != nil {
		closeDB()
	}
	require.Error(t, err, "must error when metadata tables are absent")
	assert.Contains(t, err.Error(), "embeddings build",
		"error must point the user at \"msgvault embeddings build\"")
	assert.NotContains(t, err.Error(), "42P01",
		"raw PostgreSQL SQLSTATE must not leak to the user")
}

// openEmptyPGSchemaSolo creates an isolated empty schema and returns a DSN
// whose search_path is that schema ALONE (no public). Used by the
// un-migrated negative test so to_regclass('index_generations') resolves to
// NULL regardless of what prior test runs left in public. Skips when
// MSGVAULT_TEST_DB is not a PostgreSQL DSN.
func openEmptyPGSchemaSolo(t *testing.T) string {
	t.Helper()
	url := os.Getenv("MSGVAULT_TEST_DB")
	if !strings.HasPrefix(url, "postgres://") && !strings.HasPrefix(url, "postgresql://") {
		t.Skip("requires MSGVAULT_TEST_DB to point at a PostgreSQL DSN")
	}
	buf := make([]byte, 8)
	_, err := rand.Read(buf)
	require.NoError(t, err, "random schema name")
	schemaName := "embed_solo_test_" + hex.EncodeToString(buf)

	setup, err := sql.Open("pgx", url)
	require.NoError(t, err, "open setup")
	defer func() { _ = setup.Close() }()
	_, err = setup.Exec("CREATE SCHEMA " + schemaName)
	require.NoError(t, err, "create schema")
	t.Cleanup(func() {
		cleanup, err := sql.Open("pgx", url)
		if err != nil {
			return
		}
		defer func() { _ = cleanup.Close() }()
		_, _ = cleanup.Exec("DROP SCHEMA " + schemaName + " CASCADE")
	})

	sep := "?"
	if strings.Contains(url, "?") {
		sep = "&"
	}
	return url + sep + "search_path=" + schemaName
}
