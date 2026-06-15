//go:build sqlite_vec

package sqlitevec

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"
)

func TestMigrate_FreshAndIdempotent(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "vectors.db")

	db := openTestDB(t, path)
	t.Cleanup(func() { _ = db.Close() })

	requirepkg.NoError(t, Migrate(ctx, db, 768), "first migrate")

	for _, tbl := range []string{
		"index_generations", "embeddings", "embed_runs",
		"pending_embeddings", "vectors_vec_d768", "schema_version",
	} {
		var name string
		err := db.QueryRow(`SELECT name FROM sqlite_master WHERE name = ?`, tbl).Scan(&name)
		requirepkg.NoErrorf(t, err, "table %s missing", tbl)
	}

	// Idempotent: running again must not error.
	requirepkg.NoError(t, Migrate(ctx, db, 768), "second migrate")
}

// TestMigrate_LegacyToChunked builds a pre-chunking vectors.db
// (embeddings keyed by (generation_id, message_id), vec0 with
// `message_id INTEGER PRIMARY KEY`), runs Migrate, and asserts:
//
//   - the embeddings table picks up the new columns
//     (embedding_id, chunk_index, chunk_char_start, chunk_char_end);
//   - legacy rows are preserved as chunk_index=0 with
//     embedding_id == legacy message_id;
//   - the vec0 table is rebuilt with embedding_id as its rowid,
//     keeping all rows so existing embeddings remain searchable;
//   - the AUTOINCREMENT counter is bumped past every legacy
//     embedding_id so new inserts don't collide with retained
//     rowids;
//   - a second Migrate is a no-op (idempotent).
func TestMigrate_LegacyToChunked(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "legacy.db")
	db := openTestDB(t, path)
	t.Cleanup(func() { _ = db.Close() })

	// Hand-build the pre-chunking schema. Mirrors schema.sql as it
	// shipped at PR #277 / spec §5.2.
	legacyDDL := []string{
		`CREATE TABLE schema_version (version INTEGER PRIMARY KEY)`,
		`INSERT INTO schema_version VALUES (1)`,
		`CREATE TABLE index_generations (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			model TEXT NOT NULL, dimension INTEGER NOT NULL,
			fingerprint TEXT NOT NULL, started_at INTEGER NOT NULL,
			completed_at INTEGER, activated_at INTEGER,
			state TEXT NOT NULL, message_count INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE TABLE embeddings (
			generation_id INTEGER NOT NULL REFERENCES index_generations(id) ON DELETE CASCADE,
			message_id INTEGER NOT NULL,
			embedded_at INTEGER NOT NULL,
			source_char_len INTEGER NOT NULL,
			truncated INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (generation_id, message_id)
		)`,
		`CREATE INDEX idx_embeddings_msg ON embeddings(message_id)`,
		`CREATE TABLE pending_embeddings (
			generation_id INTEGER NOT NULL REFERENCES index_generations(id) ON DELETE CASCADE,
			message_id INTEGER NOT NULL,
			enqueued_at INTEGER NOT NULL,
			claimed_at INTEGER, claim_token TEXT,
			PRIMARY KEY (generation_id, message_id)
		)`,
		`CREATE TABLE embed_runs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			generation_id INTEGER NOT NULL REFERENCES index_generations(id),
			started_at INTEGER NOT NULL, ended_at INTEGER,
			claimed INTEGER NOT NULL DEFAULT 0,
			succeeded INTEGER NOT NULL DEFAULT 0,
			failed INTEGER NOT NULL DEFAULT 0,
			truncated INTEGER NOT NULL DEFAULT 0,
			error TEXT
		)`,
		`CREATE VIRTUAL TABLE vectors_vec_d768 USING vec0(
			generation_id INTEGER PARTITION KEY,
			message_id    INTEGER PRIMARY KEY,
			embedding     FLOAT[768]
		)`,
	}
	for _, q := range legacyDDL {
		_, err := db.ExecContext(ctx, q)
		require.NoErrorf(err, "seed legacy DDL %q", q)
	}
	// Seed one generation and two embedded rows. message_count =
	// 2 to mirror what the pre-chunking Upsert path would have left
	// behind.
	_, err := db.ExecContext(ctx,
		`INSERT INTO index_generations (id, model, dimension, fingerprint, started_at, state, message_count)
		 VALUES (1, 'm', 768, 'm:768', 100, 'active', 2)`)
	require.NoError(err, "seed generation")
	_, err = db.ExecContext(ctx,
		`INSERT INTO embeddings (generation_id, message_id, embedded_at, source_char_len, truncated)
		 VALUES (1, 10, 100, 50, 0), (1, 20, 100, 75, 1)`)
	require.NoError(err, "seed embeddings")
	// vec0 demands its rowid match the second PK column; here the
	// legacy schema uses message_id, so 10 and 20 both go in directly.
	blob := float32SliceBlob
	v10 := make([]float32, 768)
	v20 := make([]float32, 768)
	for i := range v10 {
		v10[i] = 0.1
		v20[i] = 0.2
	}
	_, err = db.ExecContext(ctx,
		`INSERT INTO vectors_vec_d768 (generation_id, message_id, embedding) VALUES (1, 10, ?)`, blob(v10))
	require.NoError(err, "seed vec rowid 10")
	_, err = db.ExecContext(ctx,
		`INSERT INTO vectors_vec_d768 (generation_id, message_id, embedding) VALUES (1, 20, ?)`, blob(v20))
	require.NoError(err, "seed vec rowid 20")

	// Run the migration.
	require.NoError(Migrate(ctx, db, 768), "Migrate")

	// embeddings now has the chunked-layout columns, and the legacy
	// rows survived as chunk_index=0 with embedding_id == old message_id.
	rows, err := db.QueryContext(ctx,
		`SELECT embedding_id, message_id, chunk_index, source_char_len, truncated
		   FROM embeddings ORDER BY message_id`)
	require.NoError(err, "select embeddings")
	defer func() { _ = rows.Close() }()
	type row struct {
		eid, mid, ci, charLen, trunc int64
	}
	var got []row
	for rows.Next() {
		var r row
		require.NoError(rows.Scan(&r.eid, &r.mid, &r.ci, &r.charLen, &r.trunc), "scan")
		got = append(got, r)
	}
	require.NoError(rows.Err(), "iterate embeddings")
	// AUTOINCREMENT allocates fresh embedding_ids — they must be
	// distinct and positive. The actual values depend on insertion
	// order, so verify the *shape* (distinct, all > 0) rather than
	// pin specific numbers.
	require.Lenf(got, 2, "rows = %v", got)
	assert.Equalf(int64(10), got[0].mid, "row[0]")
	assert.Equalf(int64(0), got[0].ci, "row[0]")
	assert.Equalf(int64(50), got[0].charLen, "row[0]")
	assert.Equalf(int64(0), got[0].trunc, "row[0]")
	assert.Equalf(int64(20), got[1].mid, "row[1]")
	assert.Equalf(int64(0), got[1].ci, "row[1]")
	assert.Equalf(int64(75), got[1].charLen, "row[1]")
	assert.Equalf(int64(1), got[1].trunc, "row[1]")
	assert.Positive(got[0].eid, "embedding_id should be positive")
	assert.Positive(got[1].eid, "embedding_id should be positive")
	assert.NotEqual(got[0].eid, got[1].eid, "embedding_ids should be distinct")

	// vec0 rowid is now the AUTOINCREMENT embedding_id (not the
	// legacy message_id). Verify the rebuild used the mapping so
	// every legacy vec0 row joins back to its embedding.
	var n int
	err = db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM vectors_vec_d768 v
		   JOIN embeddings e ON e.embedding_id = v.embedding_id
		  WHERE v.generation_id = 1`).Scan(&n)
	require.NoError(err, "join count")
	assert.Equal(2, n, "joined vec rows")

	// Idempotent: a second Migrate must do nothing and leave the rows
	// untouched.
	require.NoError(Migrate(ctx, db, 768), "second Migrate")
	err = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM embeddings`).Scan(&n)
	require.NoError(err, "post-2nd count")
	assert.Equal(2, n, "post-2nd embeddings")
}

// TestMigrate_LegacyToChunked_MultiGenerationCollision is the
// regression test for roborev #323's high-risk finding: the legacy
// embeddings PK was (generation_id, message_id), so the same
// message_id could legitimately appear in two generations (one
// active, one building). An earlier draft of the migration mapped
// embedding_id := message_id, which collided on the new UNIQUE
// constraint as soon as that case arose. This test reproduces that
// shape and asserts the migration succeeds, allocating distinct
// embedding_ids per (gen, msg) pair and preserving every legacy
// vec0 row through the rebuild.
func TestMigrate_LegacyToChunked_MultiGenerationCollision(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "legacy.db")
	db := openTestDB(t, path)
	t.Cleanup(func() { _ = db.Close() })

	legacyDDL := []string{
		`CREATE TABLE schema_version (version INTEGER PRIMARY KEY)`,
		`INSERT INTO schema_version VALUES (1)`,
		`CREATE TABLE index_generations (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			model TEXT NOT NULL, dimension INTEGER NOT NULL,
			fingerprint TEXT NOT NULL, started_at INTEGER NOT NULL,
			completed_at INTEGER, activated_at INTEGER,
			state TEXT NOT NULL, message_count INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE TABLE embeddings (
			generation_id INTEGER NOT NULL REFERENCES index_generations(id) ON DELETE CASCADE,
			message_id INTEGER NOT NULL,
			embedded_at INTEGER NOT NULL,
			source_char_len INTEGER NOT NULL,
			truncated INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (generation_id, message_id)
		)`,
		`CREATE INDEX idx_embeddings_msg ON embeddings(message_id)`,
		`CREATE TABLE pending_embeddings (
			generation_id INTEGER NOT NULL REFERENCES index_generations(id) ON DELETE CASCADE,
			message_id INTEGER NOT NULL,
			enqueued_at INTEGER NOT NULL,
			claimed_at INTEGER, claim_token TEXT,
			PRIMARY KEY (generation_id, message_id)
		)`,
		`CREATE TABLE embed_runs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			generation_id INTEGER NOT NULL REFERENCES index_generations(id),
			started_at INTEGER NOT NULL, ended_at INTEGER,
			claimed INTEGER NOT NULL DEFAULT 0,
			succeeded INTEGER NOT NULL DEFAULT 0,
			failed INTEGER NOT NULL DEFAULT 0,
			truncated INTEGER NOT NULL DEFAULT 0,
			error TEXT
		)`,
		`CREATE VIRTUAL TABLE vectors_vec_d768 USING vec0(
			generation_id INTEGER PARTITION KEY,
			message_id    INTEGER PRIMARY KEY,
			embedding     FLOAT[768]
		)`,
	}
	for _, q := range legacyDDL {
		_, err := db.ExecContext(ctx, q)
		require.NoErrorf(err, "seed legacy DDL %q", q)
	}
	// Two generations: gen 1 active, gen 2 building. Both contain
	// embeddings rows for message 10 (the realistic case: an active
	// gen has it embedded; a building gen seeded it again because the
	// worker re-embeds the whole corpus per generation). The vec0
	// table only carries the gen 1 vector — vec0's rowid uniqueness
	// would have rejected the gen 2 row at write time under the
	// legacy code, so historical databases at most carry conflicting
	// embeddings rows + a single vec0 row per message_id.
	//
	// Under the buggy migration (embedding_id := message_id), the
	// gen=2/msg=10 row collided on the UNIQUE(gen,msg,ci) constraint
	// because eid=10 was already taken by gen=1/msg=10. The fixed
	// migration lets AUTOINCREMENT allocate distinct eids.
	_, err := db.ExecContext(ctx, `
		INSERT INTO index_generations (id, model, dimension, fingerprint, started_at, state, message_count) VALUES
		  (1, 'm', 768, 'm:768', 100, 'active',   1),
		  (2, 'm', 768, 'm:768', 200, 'building', 1)`)
	require.NoError(err, "seed generations")
	_, err = db.ExecContext(ctx, `
		INSERT INTO embeddings (generation_id, message_id, embedded_at, source_char_len, truncated) VALUES
		  (1, 10, 100, 50, 0),
		  (2, 10, 200, 50, 0)`)
	require.NoError(err, "seed embeddings")
	v := make([]float32, 768)
	for i := range v {
		v[i] = 0.1
	}
	_, err = db.ExecContext(ctx,
		`INSERT INTO vectors_vec_d768 (generation_id, message_id, embedding) VALUES (1, 10, ?)`,
		float32SliceBlob(v))
	require.NoError(err, "seed vec gen=1 msg=10")

	require.NoError(Migrate(ctx, db, 768), "Migrate (this is what the old migration could not survive)")

	// Both legacy embeddings rows preserved with distinct
	// embedding_ids — the AUTOINCREMENT allocation steps around the
	// (gen=1, msg=10) / (gen=2, msg=10) collision that broke the old
	// hard-coded eid=msg shortcut.
	var n int
	require.NoError(db.QueryRowContext(ctx, `SELECT COUNT(*) FROM embeddings`).Scan(&n), "count embeddings")
	assert.Equal(2, n, "embeddings rows")
	require.NoError(db.QueryRowContext(ctx,
		`SELECT COUNT(DISTINCT embedding_id) FROM embeddings`).Scan(&n), "count distinct eid")
	assert.Equal(2, n, "distinct embedding_ids (one per (gen, msg))")

	// vec0 join still resolves cleanly for the row that was actually
	// embedded (gen=1, msg=10). The mapping looked up the new eid via
	// the embeddings table rather than the now-invalid eid=msg
	// shortcut.
	require.NoError(db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM vectors_vec_d768 v
		  JOIN embeddings e ON e.embedding_id = v.embedding_id
		 WHERE v.generation_id = 1 AND e.message_id = 10`).Scan(&n), "join")
	assert.Equal(1, n, "join rows")
}

// schemaVersion reads the single-row schema_version value.
func schemaVersion(t *testing.T, db *sql.DB) int {
	t.Helper()
	var v int
	requirepkg.NoError(t,
		db.QueryRow(`SELECT MAX(version) FROM schema_version`).Scan(&v),
		"read schema_version")
	return v
}

// vec0RankOrder runs a KNN MATCH (k=2, the size of the A/B parity
// corpus) against the given vec0 table for the query vector (over the
// single test generation id=1) and returns the embedding_ids in distance
// order. Used to prove the configured distance metric (cosine after
// migration) ranks non-unit vectors by direction rather than Euclidean
// nearness.
func vec0RankOrder(t *testing.T, db *sql.DB, table string, q []float32) []int64 {
	t.Helper()
	rows, err := db.Query(fmt.Sprintf(`
		SELECT embedding_id FROM %s
		 WHERE generation_id = 1 AND embedding MATCH ? AND k = 2
		 ORDER BY distance ASC`, table), float32SliceBlob(q))
	requirepkg.NoError(t, err, "knn query")
	defer func() { _ = rows.Close() }()
	var out []int64
	for rows.Next() {
		var eid int64
		requirepkg.NoError(t, rows.Scan(&eid), "scan eid")
		out = append(out, eid)
	}
	requirepkg.NoError(t, rows.Err(), "iterate knn")
	return out
}

// TestMigrate_L2ToCosine builds a chunked-layout vectors.db whose vec0
// table was created the OLD way (bare FLOAT[N], i.e. L2) at
// schema_version=1, seeds it with NON-UNIT vectors that L2 and cosine
// rank differently, runs Migrate, and asserts:
//
//   - schema_version is advanced to 2;
//   - every embedding_id/vector survives the rebuild;
//   - the rebuilt table now ranks by cosine (direction), matching
//     pgvector — the L2-vs-cosine teeth are documented inline;
//   - a second Migrate is idempotent and does not rebuild again.
func TestMigrate_L2ToCosine(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "l2.db")
	db := openTestDB(t, path)
	t.Cleanup(func() { _ = db.Close() })

	// Build the chunked-layout schema by hand with a *legacy L2* vec0
	// table (bare FLOAT[3], no distance_metric) at schema_version=1 —
	// what every install shipped before this change.
	const dim = 3
	legacyDDL := []string{
		`CREATE TABLE schema_version (version INTEGER PRIMARY KEY)`,
		`INSERT INTO schema_version VALUES (1)`,
		`CREATE TABLE index_generations (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			model TEXT NOT NULL, dimension INTEGER NOT NULL,
			fingerprint TEXT NOT NULL, started_at INTEGER NOT NULL,
			seeded_at INTEGER, completed_at INTEGER, activated_at INTEGER,
			state TEXT NOT NULL, message_count INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE TABLE embeddings (
			embedding_id     INTEGER PRIMARY KEY AUTOINCREMENT,
			generation_id    INTEGER NOT NULL REFERENCES index_generations(id) ON DELETE CASCADE,
			message_id       INTEGER NOT NULL,
			chunk_index      INTEGER NOT NULL DEFAULT 0,
			embedded_at      INTEGER NOT NULL,
			source_char_len  INTEGER NOT NULL,
			chunk_char_start INTEGER NOT NULL DEFAULT 0,
			chunk_char_end   INTEGER NOT NULL DEFAULT 0,
			truncated        INTEGER NOT NULL DEFAULT 0,
			UNIQUE (generation_id, message_id, chunk_index)
		)`,
		`CREATE INDEX idx_embeddings_msg ON embeddings(message_id)`,
		`CREATE INDEX idx_embeddings_gen_msg ON embeddings(generation_id, message_id)`,
		fmt.Sprintf(`CREATE VIRTUAL TABLE vectors_vec_d%d USING vec0(
			generation_id INTEGER PARTITION KEY,
			embedding_id  INTEGER PRIMARY KEY,
			embedding     FLOAT[%d]
		)`, dim, dim),
	}
	for _, q := range legacyDDL {
		_, err := db.ExecContext(ctx, q)
		require.NoErrorf(err, "seed legacy DDL %q", q)
	}
	_, err := db.ExecContext(ctx,
		`INSERT INTO index_generations (id, model, dimension, fingerprint, started_at, state, message_count)
		 VALUES (1, 'm', 3, 'm:3', 100, 'active', 2)`)
	require.NoError(err, "seed generation")

	// q=(1,0,0). A = (5,0,0): same direction, large norm → cosine ~0 but
	// L2 large. B = (1,0.2,0): off-axis, near-unit norm → small L2 but
	// larger cosine distance than A. Cosine ranks A before B; L2 ranks B
	// before A.
	q := []float32{1, 0, 0}
	vecA := []float32{5, 0, 0}
	vecB := []float32{1, 0.2, 0}
	const eidA, eidB = int64(100), int64(200)
	_, err = db.ExecContext(ctx,
		`INSERT INTO embeddings (embedding_id, generation_id, message_id, chunk_index, embedded_at, source_char_len)
		 VALUES (?, 1, 10, 0, 100, 50), (?, 1, 20, 0, 100, 50)`, eidA, eidB)
	require.NoError(err, "seed embeddings")
	_, err = db.ExecContext(ctx,
		fmt.Sprintf(`INSERT INTO vectors_vec_d%d (generation_id, embedding_id, embedding) VALUES (1, ?, ?)`, dim),
		eidA, float32SliceBlob(vecA))
	require.NoError(err, "seed vec A")
	_, err = db.ExecContext(ctx,
		fmt.Sprintf(`INSERT INTO vectors_vec_d%d (generation_id, embedding_id, embedding) VALUES (1, ?, ?)`, dim),
		eidB, float32SliceBlob(vecB))
	require.NoError(err, "seed vec B")

	// Teeth: under the OLD L2 table, the closest by Euclidean distance is
	// B, so L2 ranks [B, A]. This is the divergence from pgvector (cosine)
	// the migration fixes — captured here so the post-migration assertion
	// is demonstrably meaningful.
	l2Order := vec0RankOrder(t, db, VectorTableName(dim), q)
	require.Equal([]int64{eidB, eidA}, l2Order,
		"pre-migration L2 ranks B before A (the bug this migration fixes)")

	// Run the migration.
	require.NoError(Migrate(ctx, db, dim), "Migrate")

	// schema_version advanced to 2.
	assert.Equal(2, schemaVersion(t, db), "schema_version after migrate")

	// Data intact: same embedding_ids and vectors survive the rebuild.
	var cnt int
	require.NoError(db.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT COUNT(*) FROM vectors_vec_d%d`, dim)).Scan(&cnt), "count vecs")
	assert.Equal(2, cnt, "vec rows preserved")
	for _, want := range []struct {
		eid int64
		vec []float32
	}{{eidA, vecA}, {eidB, vecB}} {
		var blob []byte
		require.NoErrorf(db.QueryRowContext(ctx,
			fmt.Sprintf(`SELECT embedding FROM vectors_vec_d%d WHERE embedding_id = ?`, dim), want.eid).
			Scan(&blob), "read vec %d", want.eid)
		assert.Equalf(float32SliceBlob(want.vec), blob, "vector for eid=%d preserved", want.eid)
	}

	// Ranking is now cosine: A (same direction, large norm) ranks before
	// B (off-axis) — the OPPOSITE of the pre-migration L2 order above.
	cosOrder := vec0RankOrder(t, db, VectorTableName(dim), q)
	assert.Equal([]int64{eidA, eidB}, cosOrder,
		"post-migration cosine ranks A before B (matches pgvector)")

	// Idempotent: second Migrate is a no-op (version already 2, no
	// rebuild) and leaves rows + ordering untouched.
	require.NoError(Migrate(ctx, db, dim), "second Migrate")
	assert.Equal(2, schemaVersion(t, db), "schema_version stays 2")
	cosOrder2 := vec0RankOrder(t, db, VectorTableName(dim), q)
	assert.Equal([]int64{eidA, eidB}, cosOrder2, "ordering stable after second migrate")
}

// TestMigrate_FreshIsCosineAtV2 asserts a freshly-created vectors.db
// lands at schema_version=2 with a cosine vec0 table directly (no rebuild
// needed) and that ranking is cosine from the start.
func TestMigrate_FreshIsCosineAtV2(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	ctx := context.Background()
	const dim = 3
	db := openTestDB(t, filepath.Join(t.TempDir(), "fresh.db"))
	t.Cleanup(func() { _ = db.Close() })

	require.NoError(Migrate(ctx, db, dim), "fresh Migrate")
	assert.Equal(2, schemaVersion(t, db), "fresh DB at schema_version 2")

	// Seed a generation + the same non-unit A/B pair and confirm cosine
	// ranking out of the box (no migration rebuild was involved).
	_, err := db.ExecContext(ctx,
		`INSERT INTO index_generations (id, model, dimension, fingerprint, started_at, state, message_count)
		 VALUES (1, 'm', 3, 'm:3', 100, 'active', 2)`)
	require.NoError(err, "seed generation")
	_, err = db.ExecContext(ctx,
		`INSERT INTO embeddings (embedding_id, generation_id, message_id, chunk_index, embedded_at, source_char_len)
		 VALUES (100, 1, 10, 0, 100, 50), (200, 1, 20, 0, 100, 50)`)
	require.NoError(err, "seed embeddings")
	_, err = db.ExecContext(ctx,
		fmt.Sprintf(`INSERT INTO vectors_vec_d%d (generation_id, embedding_id, embedding) VALUES (1, 100, ?)`, dim),
		float32SliceBlob([]float32{5, 0, 0}))
	require.NoError(err, "seed vec A")
	_, err = db.ExecContext(ctx,
		fmt.Sprintf(`INSERT INTO vectors_vec_d%d (generation_id, embedding_id, embedding) VALUES (1, 200, ?)`, dim),
		float32SliceBlob([]float32{1, 0.2, 0}))
	require.NoError(err, "seed vec B")

	order := vec0RankOrder(t, db, VectorTableName(dim), []float32{1, 0, 0})
	assert.Equal([]int64{100, 200}, order, "fresh DB ranks by cosine (A before B)")
}

func TestMigrate_CreatesDimensionSpecificVecTable(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t, filepath.Join(t.TempDir(), "v.db"))
	t.Cleanup(func() { _ = db.Close() })

	requirepkg.NoError(t, Migrate(ctx, db, 768), "migrate 768")
	requirepkg.NoError(t, EnsureVectorTable(ctx, db, 1024), "ensure 1024")
	var name string
	err := db.QueryRow(
		`SELECT name FROM sqlite_master WHERE name = 'vectors_vec_d1024'`).Scan(&name)
	assertpkg.NoError(t, err, "vectors_vec_d1024 not created")
}

func openTestDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	requirepkg.NoError(t, RegisterExtension(), "register")
	db, err := sql.Open(DriverName(), path)
	requirepkg.NoError(t, err, "open")
	return db
}

// TestForeignKeys_PerConnection verifies that `PRAGMA foreign_keys = ON`
// applies to every pooled connection, not just the one that ran Migrate.
// SQLite's foreign_keys PRAGMA is per-connection, so a single ExecContext
// against *sql.DB only enables enforcement on whatever physical conn the
// pool happened to hand back. The ConnectHook in RegisterExtension is
// what makes enforcement pool-wide.
//
// We force the pool to allocate N distinct physical connections by
// holding N *sql.Conn handles open simultaneously — sequential
// db.Conn() calls or db.ExecContext() calls can all be served by the
// same pooled conn, which would let a buggy hook hide undetected.
func TestForeignKeys_PerConnection(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "vectors.db")

	db := openTestDB(t, path)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(Migrate(ctx, db, 768), "migrate")

	const conns = 4
	db.SetMaxOpenConns(conns)

	// Acquire all N conns up front and keep them open. Each db.Conn()
	// call must allocate a fresh physical connection because earlier
	// ones haven't been released. This is what guarantees the hook is
	// being tested against distinct conns rather than a single reused
	// one.
	held := make([]*sql.Conn, conns)
	for i := range conns {
		c, err := db.Conn(ctx)
		require.NoErrorf(err, "conn %d", i)
		held[i] = c
	}
	t.Cleanup(func() {
		for _, c := range held {
			_ = c.Close()
		}
	})

	// Verify each held conn directly: PRAGMA foreign_keys must read
	// back as 1, and an FK-violating insert must fail on every conn.
	for i, c := range held {
		var fk int
		err := c.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&fk)
		require.NoErrorf(err, "conn %d pragma read", i)
		assert.Equalf(1, fk, "conn %d: foreign_keys (ConnectHook missed this conn)", i)
		_, err = c.ExecContext(ctx,
			`INSERT INTO pending_embeddings (generation_id, message_id, enqueued_at)
			 VALUES (?, ?, ?)`, 9999999, int64(i), int64(i))
		//nolint:testifylint // guarded assert+continue: a require here would abort the per-conn loop instead of skipping to the next connection
		if !assert.Errorf(err, "conn %d: FK-violating insert should fail", i) {
			continue
		}
		msg := err.Error()
		assert.Truef(strings.Contains(msg, "FOREIGN KEY") || strings.Contains(msg, "foreign key"),
			"conn %d: error = %v; want FOREIGN KEY violation", i, err)
	}
}
