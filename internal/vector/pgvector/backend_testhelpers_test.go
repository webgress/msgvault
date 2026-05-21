//go:build pgvector

package pgvector

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/wesm/msgvault/internal/vector"
)

// testDBURL returns the value of MSGVAULT_TEST_DB if it names a
// PostgreSQL DSN; otherwise it skips the calling test. pgvector tests
// require a live Postgres with the pgvector extension installed — they
// cannot fall back to an in-memory database the way sqlitevec tests do.
func testDBURL(t *testing.T) string {
	t.Helper()
	url := os.Getenv("MSGVAULT_TEST_DB")
	if !(strings.HasPrefix(url, "postgres://") || strings.HasPrefix(url, "postgresql://")) {
		t.Skip("pgvector tests require MSGVAULT_TEST_DB to point at a PostgreSQL DSN")
	}
	return url
}

// openPGTestDB connects to MSGVAULT_TEST_DB on its own per-test schema
// and applies the minimum main-schema scaffolding the backend's
// SELECT/JOIN paths exercise. Returns the *sql.DB plus a cleanup that
// drops the schema; the *sql.DB is closed via t.Cleanup as well.
//
// We deliberately stand up a minimal messages schema rather than
// importing the full store.InitSchema because the vector backend only
// touches a small slice of the main tables and pulling in the full
// schema entangles every test with the rest of the codebase's
// migrations.
func openPGTestDB(t *testing.T) *sql.DB {
	t.Helper()
	url := testDBURL(t)

	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		t.Fatalf("random schema name: %v", err)
	}
	schemaName := "pgvec_test_" + hex.EncodeToString(buf)

	setup, err := sql.Open("pgx", url)
	if err != nil {
		t.Fatalf("open setup: %v", err)
	}
	defer func() { _ = setup.Close() }()
	if _, err := setup.Exec(fmt.Sprintf("CREATE SCHEMA %s", schemaName)); err != nil {
		t.Fatalf("create schema: %v", err)
	}

	testURL := url
	sep := "?"
	if strings.Contains(url, "?") {
		sep = "&"
	}
	// Search the per-test schema first, then the shared "public" schema
	// where pgvector's extension objects live. Without "public" in the
	// path the per-test connection can't resolve the `vector` type.
	testURL += sep + "search_path=" + schemaName + ",public"

	db, err := sql.Open("pgx", testURL)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	t.Cleanup(func() {
		_ = db.Close()
		cleanup, err := sql.Open("pgx", url)
		if err != nil {
			return
		}
		defer func() { _ = cleanup.Close() }()
		_, _ = cleanup.Exec(fmt.Sprintf("DROP SCHEMA %s CASCADE", schemaName))
	})

	if _, err := db.Exec(`
		CREATE TABLE messages (
			id BIGINT PRIMARY KEY,
			source_id BIGINT,
			sender_id BIGINT,
			subject TEXT,
			has_attachments BOOLEAN DEFAULT FALSE,
			size_estimate BIGINT,
			sent_at TIMESTAMPTZ,
			deleted_at TIMESTAMPTZ,
			deleted_from_source_at TIMESTAMPTZ
		);
		CREATE TABLE message_recipients (
			id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
			message_id BIGINT NOT NULL,
			recipient_type TEXT NOT NULL,
			participant_id BIGINT NOT NULL
		);
		CREATE TABLE message_labels (
			message_id BIGINT NOT NULL,
			label_id BIGINT NOT NULL,
			PRIMARY KEY (message_id, label_id)
		);`); err != nil {
		t.Fatalf("create main schema: %v", err)
	}

	return db
}

// seedOneMessage inserts a single live message (id=1) into the main
// schema, mirroring the SQLite testhelper of the same shape.
func seedOneMessage(t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO messages (id) VALUES (1)`); err != nil {
		t.Fatalf("seed message: %v", err)
	}
}

// newBackendForTest opens a per-test database with one live message
// and returns a Backend ready for assertions.
func newBackendForTest(t *testing.T) (*Backend, context.Context, *sql.DB) {
	t.Helper()
	db := openPGTestDB(t)
	seedOneMessage(t, db)
	ctx := context.Background()
	b, err := Open(ctx, Options{DB: db, Dimension: 768})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = b.Close() })
	return b, ctx, db
}

// unitVec returns a unit vector of the given dimension with 1.0 at
// position axis and 0.0 elsewhere — the building block for similarity
// assertions where we know which message should rank first.
func unitVec(dim, axis int) []float32 {
	v := make([]float32, dim)
	v[axis] = 1
	return v
}

// seedAndEmbed inserts any missing message rows, creates a building
// generation sized to the first vector, upserts every supplied vector
// as a chunk, and clears pending_embeddings for those rows so the
// caller sees the "fully embedded" end state. Mirrors the sqlitevec
// helper of the same name.
func seedAndEmbed(t *testing.T, b *Backend, db *sql.DB, vecs map[int64][]float32) vector.GenerationID {
	t.Helper()
	if len(vecs) == 0 {
		t.Fatal("seedAndEmbed: no vectors supplied")
	}
	ctx := context.Background()

	ids := make([]int64, 0, len(vecs))
	for id := range vecs {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	expectedDim := len(vecs[ids[0]])
	for _, id := range ids {
		if v := vecs[id]; len(v) != expectedDim {
			t.Fatalf("seedAndEmbed: vector for msg %d has %d dims, want %d", id, len(v), expectedDim)
		}
	}

	for _, id := range ids {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO messages (id) VALUES ($1) ON CONFLICT DO NOTHING`, id); err != nil {
			t.Fatalf("seed message %d: %v", id, err)
		}
	}

	gid, err := b.CreateGeneration(ctx, "m", expectedDim)
	if err != nil {
		t.Fatalf("CreateGeneration: %v", err)
	}

	chunks := make([]vector.Chunk, 0, len(ids))
	for _, id := range ids {
		chunks = append(chunks, vector.Chunk{MessageID: id, Vector: vecs[id]})
	}
	if err := b.Upsert(ctx, gid, chunks); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if _, err := b.db.ExecContext(ctx,
		`DELETE FROM pending_embeddings WHERE generation_id = $1`, int64(gid)); err != nil {
		t.Fatalf("clear pending: %v", err)
	}
	return gid
}
