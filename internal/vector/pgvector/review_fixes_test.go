//go:build pgvector

package pgvector

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/vector"
)

// indexNames returns the names of every index on the embeddings table in
// the connection's own (per-test) schema. Used by the V3 and V5 tests to
// assert which indexes Migrate did/did not create. The query resolves the
// embeddings table through the search_path (to_regclass) and scopes
// pg_index to THAT relation, so indexes belonging to sibling test schemas'
// embeddings tables never leak into the result.
func indexNames(t *testing.T, db *sql.DB) map[string]bool {
	t.Helper()
	rows, err := db.Query(`
		SELECT ic.relname
		  FROM pg_index i
		  JOIN pg_class ic ON ic.oid = i.indexrelid
		 WHERE i.indrelid = to_regclass('embeddings')`)
	require.NoError(t, err, "query pg_index")
	defer func() { _ = rows.Close() }()
	out := map[string]bool{}
	for rows.Next() {
		var name string
		require.NoError(t, rows.Scan(&name), "scan indexname")
		out[name] = true
	}
	require.NoError(t, rows.Err(), "iterate pg_index")
	return out
}

// TestMigrate_DropsRedundantGenMsgIndex (V3) asserts that after Migrate the
// redundant idx_embeddings_gen_msg index is ABSENT (it is a pure
// leading-prefix of the PK), while the embeddings primary key and the
// still-needed idx_embeddings_msg index are PRESENT.
func TestMigrate_DropsRedundantGenMsgIndex(t *testing.T) {
	db := openPGTestDB(t)
	ctx := context.Background()
	require.NoError(t, Migrate(ctx, db, 768, false), "Migrate")

	idx := indexNames(t, db)
	assert.False(t, idx["idx_embeddings_gen_msg"],
		"idx_embeddings_gen_msg must be absent (redundant PK prefix); got %v", idx)
	assert.True(t, idx["idx_embeddings_msg"],
		"idx_embeddings_msg must be present; got %v", idx)
	assert.True(t, idx["embeddings_pkey"],
		"embeddings primary key index must be present; got %v", idx)
}

// TestMigrate_DropsPreExistingGenMsgIndex (V3) seeds the legacy index by
// hand (as an old DB would have it) and asserts Migrate sheds it via the
// DROP INDEX IF EXISTS step.
func TestMigrate_DropsPreExistingGenMsgIndex(t *testing.T) {
	db := openPGTestDB(t)
	ctx := context.Background()
	// First migrate to create the embeddings table, then recreate the
	// legacy index to simulate a DB provisioned before V3.
	require.NoError(t, Migrate(ctx, db, 0, false), "first Migrate")
	_, err := db.ExecContext(ctx,
		`CREATE INDEX idx_embeddings_gen_msg ON embeddings(generation_id, message_id)`)
	require.NoError(t, err, "recreate legacy index")
	require.True(t, indexNames(t, db)["idx_embeddings_gen_msg"], "legacy index should exist before re-migrate")

	// Re-running Migrate must drop it.
	require.NoError(t, Migrate(ctx, db, 0, false), "second Migrate")
	assert.False(t, indexNames(t, db)["idx_embeddings_gen_msg"],
		"re-migrate must drop the legacy idx_embeddings_gen_msg")
}

// TestMigrate_SkipExtension (V5) asserts that with skipExtension=true the
// CREATE EXTENSION step is skipped while the schema tables and indexes are
// still created. CI runs as superuser with the vector extension already
// installed in the shared public schema (openPGTestDB puts it on the
// search_path), so the schema apply succeeds; the assertion is that the
// embedding objects exist after a skip-extension migrate.
func TestMigrate_SkipExtension(t *testing.T) {
	db := openPGTestDB(t)
	ctx := context.Background()
	require.NoError(t, Migrate(ctx, db, 768, true), "Migrate(skipExtension=true)")

	// Schema tables exist.
	for _, table := range []string{"index_generations", "embeddings", "pending_embeddings", "embed_runs"} {
		var reg sql.NullString
		require.NoError(t, db.QueryRowContext(ctx,
			`SELECT to_regclass($1)::text`, table).Scan(&reg),
			"to_regclass %s", table)
		assert.Truef(t, reg.Valid, "table %s must exist after skip-extension migrate", table)
	}
	// Indexes exist (and the redundant one is still dropped).
	idx := indexNames(t, db)
	assert.True(t, idx["idx_embeddings_msg"], "idx_embeddings_msg must exist; got %v", idx)
	assert.True(t, idx["embeddings_pkey"], "PK must exist; got %v", idx)
	assert.False(t, idx["idx_embeddings_gen_msg"], "redundant index must be absent; got %v", idx)
	// HNSW index for the eager dimension exists.
	assert.Truef(t, idx[VectorIndexName(768)], "eager HNSW index %s must exist; got %v", VectorIndexName(768), idx)
}

// TestOpen_SkipExtensionWiring (V5) pins the Options.SkipExtension wiring:
// Open with SkipExtension:true must succeed and produce a working backend
// (schema created without running CREATE EXTENSION). Distinct from
// SkipMigrate, which suppresses all DDL.
func TestOpen_SkipExtensionWiring(t *testing.T) {
	db := openPGTestDB(t)
	ctx := context.Background()
	b, err := Open(ctx, Options{DB: db, Dimension: 4, SkipExtension: true})
	require.NoError(t, err, "Open(SkipExtension)")
	t.Cleanup(func() { _ = b.Close() })

	// Backend is usable end-to-end: create a generation, upsert, search.
	seedOneMessage(t, db)
	gen := seedAndEmbed(t, b, db, map[int64][]float32{1: unitVec(4, 0)})
	require.NoError(t, b.ActivateGeneration(ctx, gen, true), "Activate")
	hits, err := b.Search(ctx, gen, unitVec(4, 0), 10, vector.Filter{})
	require.NoError(t, err, "Search")
	require.Len(t, hits, 1, "expected the one embedded message")
	assert.Equal(t, int64(1), hits[0].MessageID)
}

// TestFusedSearch_EmptyFilterParity (V1) asserts the empty-filter fused
// path returns the same top-k as the equivalent explicit-filter path that
// admits every message. With the empty filter the `filtered` CTE is elided
// and liveness is inlined; the result must be byte-identical in ordering.
func TestFusedSearch_EmptyFilterParity(t *testing.T) {
	f := seedThree(t)

	for _, tc := range []struct {
		name string
		req  vector.FusedRequest
	}{
		{
			name: "hybrid_fts_and_ann",
			req: vector.FusedRequest{
				FTSQuery:   "quantum",
				QueryVec:   unitVec(4, 1),
				Generation: f.gen,
				KPerSignal: 10,
				Limit:      10,
				RRFK:       60,
			},
		},
		{
			name: "ann_only",
			req: vector.FusedRequest{
				QueryVec:   unitVec(4, 0),
				Generation: f.gen,
				KPerSignal: 10,
				Limit:      10,
				RRFK:       60,
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Empty filter (elides the `filtered` CTE, inlines liveness).
			emptyReq := tc.req
			emptyReq.Filter = vector.Filter{}
			emptyHits, _, err := f.b.FusedSearch(f.ctx, emptyReq)
			require.NoError(t, err, "FusedSearch empty filter")

			// Explicit all-admitting filter (forces the materialized
			// `filtered` CTE path). All three seeded messages have a
			// source_id in {10, 20}, so this admits exactly the same set.
			allReq := tc.req
			allReq.Filter = vector.Filter{SourceIDs: []int64{10, 20}}
			allHits, _, err := f.b.FusedSearch(f.ctx, allReq)
			require.NoError(t, err, "FusedSearch all-admitting filter")

			assert.Equal(t, fusedIDs(allHits), fusedIDs(emptyHits),
				"empty-filter path must return the same ordered hit set as the all-admitting filter path")
		})
	}
}

func fusedIDs(hits []vector.FusedHit) []int64 {
	out := make([]int64, len(hits))
	for i, h := range hits {
		out[i] = h.MessageID
	}
	return out
}

// TestSearch_FilteredInlineExists (V2) covers the rewritten filtered-ANN
// path that keeps the filter in SQL (inline correlated EXISTS) instead of
// shipping a bigint[] of matching ids. Each case asserts the filtered
// result equals the expected set, including a broad filter that admits
// every message (the case the old id-array shape made expensive).
func TestSearch_FilteredInlineExists(t *testing.T) {
	b, ctx, db := newBackendForTest(t)
	gen := seedAndEmbed(t, b, db, map[int64][]float32{
		1: unitVec(4, 0),
		2: unitVec(4, 1),
		3: unitVec(4, 2),
	})

	base := time.Date(2025, 1, 15, 12, 0, 0, 0, time.UTC)
	_, err := db.ExecContext(ctx, `
		UPDATE messages
		   SET source_id = CASE id WHEN 1 THEN 10 WHEN 2 THEN 20 ELSE 30 END,
		       has_attachments = (id = 2),
		       sent_at = CASE id
		           WHEN 1 THEN $1::timestamptz
		           WHEN 2 THEN $2::timestamptz
		           ELSE $3::timestamptz
		       END
		 WHERE id IN (1, 2, 3)`,
		base, base.Add(time.Hour), base.Add(2*time.Hour))
	require.NoError(t, err, "seed filter columns")

	yes := true
	for _, tc := range []struct {
		name   string
		filter vector.Filter
		query  []float32
		want   []int64
	}{
		{
			name:   "source filter selects one",
			filter: vector.Filter{SourceIDs: []int64{20}},
			query:  unitVec(4, 1),
			want:   []int64{2},
		},
		{
			name:   "attachment filter selects one",
			filter: vector.Filter{HasAttachment: &yes},
			query:  unitVec(4, 1),
			want:   []int64{2},
		},
		{
			name:   "broad source filter admits all, ranked by ANN",
			filter: vector.Filter{SourceIDs: []int64{10, 20, 30}},
			query:  unitVec(4, 0), // closest to msg 1
			want:   []int64{1, 2, 3},
		},
		{
			name:   "no match sentinel",
			filter: vector.Filter{SourceIDs: []int64{999}},
			query:  unitVec(4, 0),
			want:   nil,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hits, err := b.Search(ctx, gen, tc.query, 10, tc.filter)
			require.NoError(t, err, "Search")
			got := hitMessageIDs(hits)
			if len(tc.want) == 0 {
				assert.Empty(t, got)
				return
			}
			// For the broad case assert the full ordered set (ANN order:
			// msg 1 closest to the axis-0 query); for selective filters the
			// single expected id.
			if tc.name == "broad source filter admits all, ranked by ANN" {
				assert.Equal(t, tc.want, got, "broad filter must return all messages in ANN order")
			} else {
				assert.Equal(t, tc.want, got)
			}
		})
	}
}

// TestSearch_FilteredInlineExists_MultiChunk (V2) guards that the rewritten
// filtered path still widens correctly across a multi-chunk filtered
// universe — the ceiling recompute uses the same EXISTS predicate, so the
// inner LIMIT loop reaches k distinct messages rather than short-returning.
func TestSearch_FilteredInlineExists_MultiChunk(t *testing.T) {
	b, ctx, db := newBackendForTest(t)
	for _, id := range []int64{1, 2} {
		_, err := db.ExecContext(ctx,
			`INSERT INTO messages (id, source_id) VALUES ($1, 10) ON CONFLICT (id) DO UPDATE SET source_id = 10`, id)
		require.NoErrorf(t, err, "seed msg %d", id)
	}
	// msg 1 contributes two chunks (one close, one far); msg 2 single chunk.
	gen, err := b.CreateGeneration(ctx, "m", 4, "")
	require.NoError(t, err, "CreateGeneration")
	require.NoError(t, b.Upsert(ctx, gen, []vector.Chunk{
		{MessageID: 1, ChunkIndex: 0, Vector: unitVec(4, 0)},
		{MessageID: 1, ChunkIndex: 1, Vector: unitVec(4, 2)},
		{MessageID: 2, ChunkIndex: 0, Vector: unitVec(4, 1)},
	}), "Upsert")
	_, err = b.db.ExecContext(ctx, `DELETE FROM pending_embeddings WHERE generation_id = $1`, int64(gen))
	require.NoError(t, err, "clear pending")

	hits, err := b.Search(ctx, gen, unitVec(4, 0), 10, vector.Filter{SourceIDs: []int64{10}})
	require.NoError(t, err, "Search")
	got := hitMessageIDs(hits)
	// Both messages surface exactly once; msg 1 wins on its close chunk.
	require.Len(t, got, 2, "want both messages once each; got %v", got)
	assert.Equal(t, int64(1), got[0], "msg 1 ranks first on its close chunk")
	assert.ElementsMatch(t, []int64{1, 2}, got, "both filtered messages must appear")
}
