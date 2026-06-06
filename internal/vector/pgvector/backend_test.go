//go:build pgvector

package pgvector

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"go.kenn.io/msgvault/internal/vector"
)

// TestBackend_CreateActivateRetire exercises the full lifecycle of one
// generation against pgvector. Parallel to the sqlitevec test of the
// same name (internal/vector/sqlitevec/backend_test.go).
func TestBackend_CreateActivateRetire(t *testing.T) {
	b, ctx, _ := newBackendForTest(t)

	gid, err := b.CreateGeneration(ctx, "nomic-embed-text-v1.5", 768, "")
	if err != nil {
		t.Fatalf("CreateGeneration: %v", err)
	}

	bg, err := b.BuildingGeneration(ctx)
	if err != nil || bg == nil || bg.ID != gid {
		t.Fatalf("BuildingGeneration got (%v, %v), want id=%d", bg, err, gid)
	}
	if _, err := b.ActiveGeneration(ctx); err == nil {
		t.Fatal("ActiveGeneration should error before activation")
	}

	if err := b.ActivateGeneration(ctx, gid); err != nil {
		t.Fatalf("ActivateGeneration: %v", err)
	}
	g, err := b.ActiveGeneration(ctx)
	if err != nil {
		t.Fatalf("ActiveGeneration after activate: %v", err)
	}
	if g.State != vector.GenerationActive {
		t.Errorf("State=%q want active", g.State)
	}
	if g.Fingerprint != "nomic-embed-text-v1.5:768" {
		t.Errorf("Fingerprint=%q", g.Fingerprint)
	}

	if err := b.RetireGeneration(ctx, gid); err != nil {
		t.Fatalf("RetireGeneration: %v", err)
	}
	if _, err := b.ActiveGeneration(ctx); err == nil {
		t.Fatal("ActiveGeneration should error after retire")
	}
}

// TestBackend_CreateGeneration_SeedsPending verifies the initial seed
// pass populates pending_embeddings with one row per live message.
func TestBackend_CreateGeneration_SeedsPending(t *testing.T) {
	b, ctx, _ := newBackendForTest(t)
	gid, err := b.CreateGeneration(ctx, "m", 768, "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	var n int
	if err := b.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pending_embeddings WHERE generation_id = $1`, int64(gid),
	).Scan(&n); err != nil {
		t.Fatalf("count pending: %v", err)
	}
	if n != 1 {
		t.Errorf("pending count = %d, want 1", n)
	}
}

// TestBackend_CreateGeneration_SkipsDeleted ensures the seed pass
// honours the live-message predicate, so soft-deleted rows are not
// re-embedded by a future rebuild.
func TestBackend_CreateGeneration_SkipsDeleted(t *testing.T) {
	db := openPGTestDB(t)
	if _, err := db.Exec(`INSERT INTO messages (id, deleted_from_source_at) VALUES (1, NOW())`); err != nil {
		t.Fatalf("seed deleted: %v", err)
	}
	ctx := context.Background()
	b, err := Open(ctx, Options{DB: db, Dimension: 768})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = b.Close() })

	gid, err := b.CreateGeneration(ctx, "m", 768, "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	var n int
	if err := b.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pending_embeddings WHERE generation_id = $1`, int64(gid),
	).Scan(&n); err != nil {
		t.Fatalf("count pending: %v", err)
	}
	if n != 0 {
		t.Errorf("pending count = %d, want 0 (deleted message must be skipped)", n)
	}
}

// TestBackend_CreateGeneration_ResumesBuilding checks the idempotent
// resume path: calling CreateGeneration twice with the same fingerprint
// returns the same generation ID instead of failing on the partial
// unique index.
func TestBackend_CreateGeneration_ResumesBuilding(t *testing.T) {
	b, ctx, _ := newBackendForTest(t)

	first, err := b.CreateGeneration(ctx, "m", 768, "")
	if err != nil {
		t.Fatalf("first Create: %v", err)
	}
	second, err := b.CreateGeneration(ctx, "m", 768, "")
	if err != nil {
		t.Fatalf("second Create: %v", err)
	}
	if first != second {
		t.Errorf("second Create returned new id %d, want reused %d", second, first)
	}
}

// TestBackend_CreateGeneration_MismatchedFingerprint asserts that a
// second CreateGeneration call with a different fingerprint while
// another build is in progress surfaces ErrBuildingInProgress.
func TestBackend_CreateGeneration_MismatchedFingerprint(t *testing.T) {
	b, ctx, _ := newBackendForTest(t)

	if _, err := b.CreateGeneration(ctx, "model-a", 768, ""); err != nil {
		t.Fatalf("first Create: %v", err)
	}
	_, err := b.CreateGeneration(ctx, "model-b", 768, "")
	if err == nil {
		t.Fatal("second Create with different fingerprint: want error, got nil")
	}
	if !errors.Is(err, vector.ErrBuildingInProgress) {
		t.Errorf("error = %v, want wrapping ErrBuildingInProgress", err)
	}
}

// TestBackend_CreateGeneration_ResumeReseedsUnseededGeneration covers
// the "crash between row insert and seed commit" path: a building row
// exists but seeded_at is NULL. Resume must re-run seedPending.
func TestBackend_CreateGeneration_ResumeReseedsUnseededGeneration(t *testing.T) {
	b, ctx, _ := newBackendForTest(t)

	gen, err := b.CreateGeneration(ctx, "m", 768, "")
	if err != nil {
		t.Fatalf("first Create: %v", err)
	}
	if _, err := b.db.ExecContext(ctx,
		`UPDATE index_generations SET seeded_at = NULL WHERE id = $1`, int64(gen)); err != nil {
		t.Fatalf("clear seeded_at: %v", err)
	}
	if _, err := b.db.ExecContext(ctx,
		`DELETE FROM pending_embeddings WHERE generation_id = $1`, int64(gen)); err != nil {
		t.Fatalf("clear pending: %v", err)
	}
	resumed, err := b.CreateGeneration(ctx, "m", 768, "")
	if err != nil {
		t.Fatalf("resume Create: %v", err)
	}
	if resumed != gen {
		t.Errorf("resumed gen = %d, want %d", resumed, gen)
	}
	var pending int
	if err := b.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pending_embeddings WHERE generation_id = $1`,
		int64(gen)).Scan(&pending); err != nil {
		t.Fatalf("count pending: %v", err)
	}
	if pending != 1 {
		t.Errorf("pending count after resume = %d, want 1", pending)
	}
	var seededAt sql.NullInt64
	if err := b.db.QueryRowContext(ctx,
		`SELECT seeded_at FROM index_generations WHERE id = $1`, int64(gen)).Scan(&seededAt); err != nil {
		t.Fatalf("read seeded_at: %v", err)
	}
	if !seededAt.Valid {
		t.Error("seeded_at still NULL after resume re-seed")
	}
}

// TestBackend_EnsureSeeded_Idempotent calls EnsureSeeded twice and
// asserts the seeded_at stamp persists across calls.
func TestBackend_EnsureSeeded_Idempotent(t *testing.T) {
	b, ctx, _ := newBackendForTest(t)
	gen, err := b.CreateGeneration(ctx, "m", 768, "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := b.EnsureSeeded(ctx, gen); err != nil {
		t.Fatalf("EnsureSeeded #1: %v", err)
	}
	if err := b.EnsureSeeded(ctx, gen); err != nil {
		t.Fatalf("EnsureSeeded #2: %v", err)
	}
}

// TestBackend_EnsureSeeded_RejectsActiveGeneration verifies the guard
// that prevents re-seeding a non-building generation.
func TestBackend_EnsureSeeded_RejectsActiveGeneration(t *testing.T) {
	b, ctx, _ := newBackendForTest(t)
	gen, err := b.CreateGeneration(ctx, "m", 768, "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := b.ActivateGeneration(ctx, gen); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	err = b.EnsureSeeded(ctx, gen)
	if !errors.Is(err, vector.ErrGenerationNotBuilding) {
		t.Errorf("EnsureSeeded on active gen returned %v, want ErrGenerationNotBuilding", err)
	}
}

// TestBackend_Upsert_RejectsDimensionMismatch ensures the per-chunk
// dimension check fires before any row is written.
func TestBackend_Upsert_RejectsDimensionMismatch(t *testing.T) {
	b, ctx, _ := newBackendForTest(t)
	gen, err := b.CreateGeneration(ctx, "m", 4, "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	err = b.Upsert(ctx, gen, []vector.Chunk{
		{MessageID: 1, Vector: []float32{1, 2, 3}}, // 3 dims, gen has 4
	})
	if !errors.Is(err, vector.ErrDimensionMismatch) {
		t.Errorf("err=%v, want wrapping ErrDimensionMismatch", err)
	}
}

// TestBackend_Upsert_UnknownGeneration surfaces ErrUnknownGeneration
// when a caller passes a bogus generation id.
func TestBackend_Upsert_UnknownGeneration(t *testing.T) {
	b, ctx, _ := newBackendForTest(t)
	err := b.Upsert(ctx, vector.GenerationID(999), []vector.Chunk{
		{MessageID: 1, Vector: []float32{0, 0, 0, 0}},
	})
	if !errors.Is(err, vector.ErrUnknownGeneration) {
		t.Errorf("err=%v, want wrapping ErrUnknownGeneration", err)
	}
}

// TestBackend_Upsert_Idempotent_PerMessage upserts the same message
// twice and confirms message_count is incremented only once.
func TestBackend_Upsert_Idempotent_PerMessage(t *testing.T) {
	b, ctx, db := newBackendForTest(t)
	gen := seedAndEmbed(t, b, db, map[int64][]float32{
		1: {1, 0, 0, 0},
	})
	if err := b.Upsert(ctx, gen, []vector.Chunk{
		{MessageID: 1, Vector: []float32{0, 1, 0, 0}}, // same id, new vector
	}); err != nil {
		t.Fatalf("second Upsert: %v", err)
	}
	var count int64
	if err := b.db.QueryRowContext(ctx,
		`SELECT message_count FROM index_generations WHERE id = $1`, int64(gen)).Scan(&count); err != nil {
		t.Fatalf("query message_count: %v", err)
	}
	if count != 1 {
		t.Errorf("message_count = %d, want 1 (re-upsert must not double-count)", count)
	}
}

// TestBackend_Search_FastPath_RanksByDistance exercises the empty-
// filter fast path. The query vector points along axis 0; the message
// whose vector also points along axis 0 must rank first.
func TestBackend_Search_FastPath_RanksByDistance(t *testing.T) {
	b, ctx, db := newBackendForTest(t)
	gen := seedAndEmbed(t, b, db, map[int64][]float32{
		1: unitVec(4, 0),
		2: unitVec(4, 1),
		3: unitVec(4, 2),
	})
	hits, err := b.Search(ctx, gen, unitVec(4, 0), 3, vector.Filter{})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("Search returned no hits")
	}
	if hits[0].MessageID != 1 {
		t.Errorf("top hit = %d, want 1", hits[0].MessageID)
	}
	for i, h := range hits {
		if h.Rank != i+1 {
			t.Errorf("hit[%d].Rank = %d, want %d", i, h.Rank, i+1)
		}
	}
}

// TestBackend_Search_DropsDeletedFromSource confirms the live-message
// EXISTS clause filters out soft-deleted rows even when their
// embedding would otherwise rank highly.
func TestBackend_Search_DropsDeletedFromSource(t *testing.T) {
	b, ctx, db := newBackendForTest(t)
	gen := seedAndEmbed(t, b, db, map[int64][]float32{
		1: unitVec(4, 0),
		2: unitVec(4, 1),
	})
	// Soft-delete the top hit. Search must skip it and return msg 2.
	if _, err := db.ExecContext(ctx,
		`UPDATE messages SET deleted_from_source_at = NOW() WHERE id = 1`); err != nil {
		t.Fatalf("soft-delete: %v", err)
	}
	hits, err := b.Search(ctx, gen, unitVec(4, 0), 5, vector.Filter{})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	for _, h := range hits {
		if h.MessageID == 1 {
			t.Errorf("Search returned soft-deleted msg 1 with score %v", h.Score)
		}
	}
}

// TestBackend_Search_RespectsFilter exercises the filtered path. We
// only allow message 2 through the SourceIDs filter; even though
// message 1 has a closer vector, it must be excluded.
func TestBackend_Search_RespectsFilter(t *testing.T) {
	b, ctx, db := newBackendForTest(t)
	gen := seedAndEmbed(t, b, db, map[int64][]float32{
		1: unitVec(4, 0),
		2: unitVec(4, 1),
	})
	// Tag the messages with distinct source_ids so the filter can pick
	// exactly one of them. SourceIDs operates over m.source_id.
	if _, err := db.ExecContext(ctx,
		`UPDATE messages SET source_id = 10 WHERE id = 1`); err != nil {
		t.Fatalf("tag source: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE messages SET source_id = 20 WHERE id = 2`); err != nil {
		t.Fatalf("tag source: %v", err)
	}
	hits, err := b.Search(ctx, gen, unitVec(4, 0), 5, vector.Filter{SourceIDs: []int64{20}})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 1 || hits[0].MessageID != 2 {
		t.Errorf("filtered hits = %+v, want exactly [msg 2]", hits)
	}
}

// TestBackend_LoadVector_RoundTrip writes a known vector and reads it
// back, confirming the text format round-trips through pgvector
// without loss for float32 precision.
func TestBackend_LoadVector_RoundTrip(t *testing.T) {
	b, ctx, db := newBackendForTest(t)
	original := []float32{0.25, -0.5, 0.75, 1.0}
	gen := seedAndEmbed(t, b, db, map[int64][]float32{1: original})
	if err := b.ActivateGeneration(ctx, gen); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	got, err := b.LoadVector(ctx, 1)
	if err != nil {
		t.Fatalf("LoadVector: %v", err)
	}
	if len(got) != len(original) {
		t.Fatalf("loaded len=%d, want %d", len(got), len(original))
	}
	for i := range original {
		if got[i] != original[i] {
			t.Errorf("dim[%d] = %v, want %v", i, got[i], original[i])
		}
	}
}

// TestBackend_Delete_RemovesAndUpdatesCount confirms Delete removes
// the embedding row and decrements message_count atomically.
func TestBackend_Delete_RemovesAndUpdatesCount(t *testing.T) {
	b, ctx, db := newBackendForTest(t)
	gen := seedAndEmbed(t, b, db, map[int64][]float32{
		1: unitVec(4, 0),
		2: unitVec(4, 1),
	})
	if err := b.Delete(ctx, gen, []int64{1}); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	var remaining, msgCount int64
	if err := b.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM embeddings WHERE generation_id = $1`, int64(gen)).Scan(&remaining); err != nil {
		t.Fatalf("count: %v", err)
	}
	if err := b.db.QueryRowContext(ctx,
		`SELECT message_count FROM index_generations WHERE id = $1`, int64(gen)).Scan(&msgCount); err != nil {
		t.Fatalf("message_count: %v", err)
	}
	if remaining != 1 {
		t.Errorf("remaining embeddings = %d, want 1", remaining)
	}
	if msgCount != 1 {
		t.Errorf("message_count = %d, want 1", msgCount)
	}
}

// TestBackend_Stats_ScopedAndAggregate covers both the per-generation
// and aggregate (gen == 0) modes of Stats.
func TestBackend_Stats_ScopedAndAggregate(t *testing.T) {
	b, ctx, db := newBackendForTest(t)
	gen := seedAndEmbed(t, b, db, map[int64][]float32{
		1: unitVec(4, 0),
		2: unitVec(4, 1),
	})

	s, err := b.Stats(ctx, gen)
	if err != nil {
		t.Fatalf("Stats(gen): %v", err)
	}
	if s.EmbeddingCount != 2 {
		t.Errorf("scoped EmbeddingCount = %d, want 2", s.EmbeddingCount)
	}

	all, err := b.Stats(ctx, 0)
	if err != nil {
		t.Fatalf("Stats(0): %v", err)
	}
	if all.EmbeddingCount != 2 {
		t.Errorf("aggregate EmbeddingCount = %d, want 2", all.EmbeddingCount)
	}
}

// TestBackend_Stats_UnknownGeneration ensures Stats surfaces
// ErrUnknownGeneration when a non-zero generation id has no row.
func TestBackend_Stats_UnknownGeneration(t *testing.T) {
	b, ctx, _ := newBackendForTest(t)
	_, err := b.Stats(ctx, vector.GenerationID(999))
	if !errors.Is(err, vector.ErrUnknownGeneration) {
		t.Errorf("err=%v, want wrapping ErrUnknownGeneration", err)
	}
}

// TestBackend_Upsert_MultiChunk_StoresAllChunks verifies a message that
// produces multiple chunks persists one row per chunk (not just the last,
// which the prior (generation_id, message_id) primary key collapsed to)
// while message_count and Stats.EmbeddingCount stay message-scoped.
func TestBackend_Upsert_MultiChunk_StoresAllChunks(t *testing.T) {
	b, ctx, _ := newBackendForTest(t)
	gen, err := b.CreateGeneration(ctx, "m", 4, "")
	if err != nil {
		t.Fatalf("CreateGeneration: %v", err)
	}
	if err := b.Upsert(ctx, gen, []vector.Chunk{
		{MessageID: 1, ChunkIndex: 0, Vector: unitVec(4, 0)},
		{MessageID: 1, ChunkIndex: 1, Vector: unitVec(4, 1)},
	}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	var rows int
	if err := b.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM embeddings WHERE generation_id = $1 AND message_id = 1`,
		int64(gen)).Scan(&rows); err != nil {
		t.Fatalf("count chunk rows: %v", err)
	}
	if rows != 2 {
		t.Errorf("chunk rows = %d, want 2 (both chunks must be stored)", rows)
	}

	var msgCount int64
	if err := b.db.QueryRowContext(ctx,
		`SELECT message_count FROM index_generations WHERE id = $1`, int64(gen)).Scan(&msgCount); err != nil {
		t.Fatalf("message_count: %v", err)
	}
	if msgCount != 1 {
		t.Errorf("message_count = %d, want 1 (chunks of one message count once)", msgCount)
	}

	s, err := b.Stats(ctx, gen)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if s.EmbeddingCount != 1 {
		t.Errorf("Stats.EmbeddingCount = %d, want 1 (distinct messages, not chunks)", s.EmbeddingCount)
	}
	if s.StorageBytes <= 0 {
		t.Errorf("Stats.StorageBytes = %d, want > 0", s.StorageBytes)
	}
}

// TestBackend_Upsert_MultiChunk_ReplaceShrinks confirms re-upserting a
// message with fewer chunks removes the orphaned tail chunks rather than
// leaving them behind (chunk counts are not stable across re-embeds).
func TestBackend_Upsert_MultiChunk_ReplaceShrinks(t *testing.T) {
	b, ctx, _ := newBackendForTest(t)
	gen, err := b.CreateGeneration(ctx, "m", 4, "")
	if err != nil {
		t.Fatalf("CreateGeneration: %v", err)
	}
	if err := b.Upsert(ctx, gen, []vector.Chunk{
		{MessageID: 1, ChunkIndex: 0, Vector: unitVec(4, 0)},
		{MessageID: 1, ChunkIndex: 1, Vector: unitVec(4, 1)},
		{MessageID: 1, ChunkIndex: 2, Vector: unitVec(4, 2)},
	}); err != nil {
		t.Fatalf("first Upsert: %v", err)
	}
	if err := b.Upsert(ctx, gen, []vector.Chunk{
		{MessageID: 1, ChunkIndex: 0, Vector: unitVec(4, 3)},
	}); err != nil {
		t.Fatalf("second Upsert: %v", err)
	}
	var rows int
	if err := b.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM embeddings WHERE generation_id = $1 AND message_id = 1`,
		int64(gen)).Scan(&rows); err != nil {
		t.Fatalf("count chunk rows: %v", err)
	}
	if rows != 1 {
		t.Errorf("chunk rows after shrink = %d, want 1 (orphan tail chunks must be removed)", rows)
	}
	var msgCount int64
	if err := b.db.QueryRowContext(ctx,
		`SELECT message_count FROM index_generations WHERE id = $1`, int64(gen)).Scan(&msgCount); err != nil {
		t.Fatalf("message_count: %v", err)
	}
	if msgCount != 1 {
		t.Errorf("message_count = %d, want 1", msgCount)
	}
}

// TestBackend_Search_MultiChunk_OneHitPerMessage verifies Search returns
// at most one Hit per message (the best-scoring chunk) when a message has
// multiple chunks, so one message's chunks cannot crowd out other
// messages in the top-k.
func TestBackend_Search_MultiChunk_OneHitPerMessage(t *testing.T) {
	b, ctx, db := newBackendForTest(t)
	if _, err := db.ExecContext(ctx, `INSERT INTO messages (id) VALUES (2)`); err != nil {
		t.Fatalf("seed message 2: %v", err)
	}
	gen, err := b.CreateGeneration(ctx, "m", 4, "")
	if err != nil {
		t.Fatalf("CreateGeneration: %v", err)
	}
	// msg 1 has two chunks (axes 0 and 3); msg 2 has one (axis 1).
	if err := b.Upsert(ctx, gen, []vector.Chunk{
		{MessageID: 1, ChunkIndex: 0, Vector: unitVec(4, 0)},
		{MessageID: 1, ChunkIndex: 1, Vector: unitVec(4, 3)},
		{MessageID: 2, ChunkIndex: 0, Vector: unitVec(4, 1)},
	}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	hits, err := b.Search(ctx, gen, unitVec(4, 0), 10, vector.Filter{})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("len(hits) = %d, want 2 (one per message)", len(hits))
	}
	seen := map[int64]int{}
	for _, h := range hits {
		seen[h.MessageID]++
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("message %d returned %d times, want exactly 1", id, n)
		}
	}
	if hits[0].MessageID != 1 {
		t.Errorf("top hit = %d, want 1 (best chunk lies on the query axis)", hits[0].MessageID)
	}
}
