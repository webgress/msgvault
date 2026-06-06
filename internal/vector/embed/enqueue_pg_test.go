//go:build pgvector

package embed

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
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/vector/pgvector"
)

// openPGEnqueueDB stands up a per-test schema on MSGVAULT_TEST_DB with the
// pgvector schema (index_generations + pending_embeddings). The Enqueuer
// only reads index_generations and writes pending_embeddings, so the main
// messages table is not needed here. Returns the *sql.DB; cleanup drops
// the schema via t.Cleanup.
func openPGEnqueueDB(t *testing.T) *sql.DB {
	t.Helper()
	url := os.Getenv("MSGVAULT_TEST_DB")
	if !strings.HasPrefix(url, "postgres://") && !strings.HasPrefix(url, "postgresql://") {
		t.Skip("pgvector enqueue tests require MSGVAULT_TEST_DB to point at a PostgreSQL DSN")
	}

	buf := make([]byte, 8)
	_, err := rand.Read(buf)
	require.NoError(t, err, "random schema name")
	schemaName := "embed_e_test_" + hex.EncodeToString(buf)

	setup, err := sql.Open("pgx", url)
	require.NoError(t, err, "open setup")
	defer func() { _ = setup.Close() }()
	_, err = setup.Exec("CREATE SCHEMA " + schemaName)
	require.NoError(t, err, "create schema")

	testURL := url
	sep := "?"
	if strings.Contains(url, "?") {
		sep = "&"
	}
	testURL += sep + "search_path=" + schemaName + ",public"

	db, err := sql.Open("pgx", testURL)
	require.NoError(t, err, "open")
	t.Cleanup(func() {
		_ = db.Close()
		cleanup, err := sql.Open("pgx", url)
		if err != nil {
			return
		}
		defer func() { _ = cleanup.Close() }()
		_, _ = cleanup.Exec("DROP SCHEMA " + schemaName + " CASCADE")
	})

	require.NoError(t, pgvector.Migrate(context.Background(), db, 0), "pgvector.Migrate")
	return db
}

// insertPGGeneration inserts an index_generations row with an explicit id
// and state so the test can control which generations are non-retired.
func insertPGGeneration(t *testing.T, db *sql.DB, id int64, state string) {
	t.Helper()
	_, err := db.ExecContext(context.Background(), `
		INSERT INTO index_generations (id, model, dimension, fingerprint, started_at, state)
		OVERRIDING SYSTEM VALUE
		VALUES ($1, 'm', 768, 'm:768', 0, $2)`, id, state)
	require.NoError(t, err, "insert generation")
}

func pgPendingCount(t *testing.T, db *sql.DB, gen int64) int {
	t.Helper()
	var n int
	require.NoError(t, db.QueryRow(
		`SELECT COUNT(*) FROM pending_embeddings WHERE generation_id = $1`, gen).Scan(&n),
		"pending count")
	return n
}

func pgEnqueuer(db *sql.DB) *Enqueuer {
	d := &store.PostgreSQLDialect{}
	return NewEnqueuer(db, d.Rebind, d.InsertOrIgnore)
}

// TestEnqueuerPG_DualEnqueueAndRetiredExclusion asserts that on pgx the
// Enqueuer inserts one pending row per (non-retired generation, message)
// and skips retired generations. Before the json_each → chunked-VALUES
// port this failed against pgx: json_each is SQLite-only and the bare `?`
// placeholders are rejected by the pgx driver.
func TestEnqueuerPG_DualEnqueueAndRetiredExclusion(t *testing.T) {
	ctx := context.Background()
	db := openPGEnqueueDB(t)
	insertPGGeneration(t, db, 1, "active")
	insertPGGeneration(t, db, 2, "building")
	insertPGGeneration(t, db, 3, "retired") // must NOT receive rows.

	e := pgEnqueuer(db)
	require.NoError(t, e.EnqueueMessages(ctx, []int64{10, 11, 12}), "EnqueueMessages")

	assert.Equal(t, 3, pgPendingCount(t, db, 1), "active generation pending count")
	assert.Equal(t, 3, pgPendingCount(t, db, 2), "building generation pending count")
	assert.Equal(t, 0, pgPendingCount(t, db, 3), "retired generation must be excluded")
}

// TestEnqueuerPG_Idempotent asserts re-enqueueing the same IDs is a no-op
// via ON CONFLICT (generation_id, message_id) DO NOTHING — exercised both
// across calls and within a single call carrying duplicate IDs.
func TestEnqueuerPG_Idempotent(t *testing.T) {
	ctx := context.Background()
	db := openPGEnqueueDB(t)
	insertPGGeneration(t, db, 1, "active")

	e := pgEnqueuer(db)
	require.NoError(t, e.EnqueueMessages(ctx, []int64{42}), "first enqueue")
	// Re-enqueue across calls and with an intra-call duplicate.
	require.NoError(t, e.EnqueueMessages(ctx, []int64{42, 42}), "re-enqueue with duplicates")
	assert.Equal(t, 1, pgPendingCount(t, db, 1), "duplicate (gen, message) must collapse to one row")
}

// TestEnqueuerPG_MultiChunk enqueues more IDs than enqueueChunkRows so the
// chunked-VALUES insert spans more than one statement. This exercises the
// chunk loop's boundary handling and confirms the total parameter count
// per statement stays bounded on pgx. The count uses
// 2*enqueueChunkRows + 1 so the final chunk is a small remainder.
func TestEnqueuerPG_MultiChunk(t *testing.T) {
	ctx := context.Background()
	db := openPGEnqueueDB(t)
	insertPGGeneration(t, db, 1, "active")
	insertPGGeneration(t, db, 2, "building")

	const total = 2*enqueueChunkRows + 1
	ids := make([]int64, total)
	for i := range ids {
		ids[i] = int64(i + 1)
	}

	e := pgEnqueuer(db)
	require.NoError(t, e.EnqueueMessages(ctx, ids), "EnqueueMessages spanning multiple chunks")

	assert.Equal(t, total, pgPendingCount(t, db, 1), "active generation got every id across chunks")
	assert.Equal(t, total, pgPendingCount(t, db, 2), "building generation got every id across chunks")

	// Re-enqueue the full batch: still idempotent across multiple chunks.
	require.NoError(t, e.EnqueueMessages(ctx, ids), "re-enqueue multi-chunk batch")
	assert.Equal(t, total, pgPendingCount(t, db, 1), "active count unchanged after idempotent re-enqueue")
}

// TestEnqueuerPG_NoGenerations_Noop asserts EnqueueMessages is a clean
// no-op when there are no non-retired generations.
func TestEnqueuerPG_NoGenerations_Noop(t *testing.T) {
	ctx := context.Background()
	db := openPGEnqueueDB(t)
	insertPGGeneration(t, db, 1, "retired")

	e := pgEnqueuer(db)
	require.NoError(t, e.EnqueueMessages(ctx, []int64{1, 2, 3}), "EnqueueMessages with only retired gen")

	var n int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM pending_embeddings`).Scan(&n))
	assert.Equal(t, 0, n, "no pending rows when only retired generations exist")
}
