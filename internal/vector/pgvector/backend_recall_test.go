//go:build pgvector

package pgvector

import (
	"context"
	"database/sql"
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/vector"
)

// recallDim is large enough to build many distinct near-query vectors so a
// single multi-chunk message can dominate the first inner ANN fetch.
const recallDim = 16

// nearQueryVec returns a unit-ish vector that points mostly along axis 0
// (the query axis) with a tiny perturbation on a second axis, so it ranks
// very close to the query but is distinct from its siblings. Smaller eps
// => closer to the query.
func nearQueryVec(perturbAxis int, eps float64) []float32 {
	v := make([]float32, recallDim)
	v[0] = float32(math.Sqrt(1 - eps*eps))
	if perturbAxis == 0 {
		perturbAxis = 1
	}
	v[perturbAxis%recallDim] = float32(eps)
	return v
}

// midQueryVec returns a vector at a moderate distance from the query: a
// 45°-ish mix of axis 0 and the given axis. These are the single-chunk
// "other" messages that must still surface once the multi-chunk message's
// chunks are deduplicated away.
func midQueryVec(axis int) []float32 {
	v := make([]float32, recallDim)
	v[0] = float32(0.6)
	v[axis%recallDim] = float32(0.8)
	return v
}

// seedRecallCorpus builds a generation where message 1 has many chunks all
// closer to the query than several single-chunk messages. This is the
// pathological multi-chunk shape that defeats a single fixed over-fetch:
// the first inner ANN LIMIT is packed by message 1's chunks, and only the
// candidate-widening loop reaches the single-chunk messages.
//
// Returns the generation id and the query vector (axis 0).
func seedRecallCorpus(t *testing.T, b *Backend, db *sql.DB, multiChunks, singles int) (vector.GenerationID, []float32) {
	t.Helper()
	ctx := context.Background()

	// Insert message rows: id 1 is the multi-chunk message; 2..singles+1 are
	// the single-chunk messages.
	total := 1 + singles
	for id := 1; id <= total; id++ {
		_, err := db.ExecContext(ctx,
			`INSERT INTO messages (id) VALUES ($1) ON CONFLICT DO NOTHING`, id)
		require.NoErrorf(t, err, "seed message %d", id)
	}

	gen, err := b.CreateGeneration(ctx, "m", recallDim, "")
	require.NoError(t, err, "CreateGeneration")

	chunks := make([]vector.Chunk, 0, multiChunks+singles)
	// Message 1: multiChunks chunks, all very close to the query.
	for i := range multiChunks {
		chunks = append(chunks, vector.Chunk{
			MessageID:  1,
			ChunkIndex: i,
			Vector:     nearQueryVec(i+1, 0.01),
		})
	}
	// Single-chunk messages at a moderate distance.
	for j := range singles {
		chunks = append(chunks, vector.Chunk{
			MessageID:  int64(2 + j),
			ChunkIndex: 0,
			Vector:     midQueryVec(j + 2),
		})
	}
	require.NoError(t, b.Upsert(ctx, gen, chunks), "Upsert")

	_, err = b.db.ExecContext(ctx,
		`DELETE FROM pending_embeddings WHERE generation_id = $1`, int64(gen))
	require.NoError(t, err, "clear pending")

	query := make([]float32, recallDim)
	query[0] = 1
	return gen, query
}

// TestBackend_Search_MultiChunkCorpus_ReturnsKDistinct asserts that Search
// returns the full k distinct messages even when one message contributes
// far more than annOverFetchFactor chunks, all of which sit ahead of the
// single-chunk messages in ANN order. Without the candidate-widening loop
// the first fixed over-fetch (k*annOverFetchFactor) collapses to a single
// distinct message after GROUP BY and Search short-returns.
func TestBackend_Search_MultiChunkCorpus_ReturnsKDistinct(t *testing.T) {
	b, ctx, db := newBackendForTest(t)

	// message 1 contributes many chunks (>> annOverFetchFactor); plus k-1
	// single-chunk messages so the full result set is exactly k distinct.
	const k = 5
	const multiChunks = 40 // far exceeds annOverFetchFactor (4)
	const singles = k - 1
	gen, query := seedRecallCorpus(t, b, db, multiChunks, singles)

	hits, err := b.Search(ctx, gen, query, k, vector.Filter{})
	require.NoError(t, err, "Search")
	require.Len(t, hits, k, "Search must return k distinct messages despite the multi-chunk message")

	seen := map[int64]int{}
	for _, h := range hits {
		seen[h.MessageID]++
	}
	require.Len(t, seen, k, "hits must be k distinct messages")
	for id, n := range seen {
		assert.Equalf(t, 1, n, "message %d returned %d times, want exactly 1", id, n)
	}
	assert.Equal(t, int64(1), hits[0].MessageID, "top hit is the multi-chunk message (its chunks are closest)")
}

// TestBackend_Search_Filtered_MultiChunkCorpus_ReturnsKDistinct asserts
// the same recall guard on the FILTERED Search path (a structured filter
// that matches every seeded row, so it exercises the filtered branch
// rather than the empty-filter fast path). All seeded messages have
// has_attachments = false (the column default), so HasAttachment=false
// matches the entire corpus. Without a chunk-count loop ceiling the
// filtered widening loop caps the inner LIMIT at the filtered MESSAGE
// count (5), saturating before the multi-chunk message's 40 chunks are
// deduplicated, and Search short-returns a single distinct message.
func TestBackend_Search_Filtered_MultiChunkCorpus_ReturnsKDistinct(t *testing.T) {
	b, ctx, db := newBackendForTest(t)

	const k = 5
	const multiChunks = 40 // far exceeds annOverFetchFactor (4)
	const singles = k - 1
	gen, query := seedRecallCorpus(t, b, db, multiChunks, singles)

	// Filter matches all rows (has_attachments defaults to false) yet is
	// non-empty, so Search takes the filtered branch.
	no := false
	hits, err := b.Search(ctx, gen, query, k, vector.Filter{HasAttachment: &no})
	require.NoError(t, err, "Search")
	require.Len(t, hits, k, "filtered Search must return k distinct messages despite the multi-chunk message")

	seen := map[int64]int{}
	for _, h := range hits {
		seen[h.MessageID]++
	}
	require.Len(t, seen, k, "filtered hits must be k distinct messages")
	for id, n := range seen {
		assert.Equalf(t, 1, n, "message %d returned %d times, want exactly 1", id, n)
	}
	assert.Equal(t, int64(1), hits[0].MessageID, "top hit is the multi-chunk message (its chunks are closest)")
}

// TestBackend_FusedSearch_MultiChunkCorpus_ReturnsKDistinct mirrors the
// recall guard on the fused (hybrid) ANN path: the ann_pool widening loop
// must reach KPerSignal+1 distinct messages even when a single message's
// chunks dominate the inner ANN scan.
func TestBackend_FusedSearch_MultiChunkCorpus_ReturnsKDistinct(t *testing.T) {
	b, ctx, db := newBackendForTest(t)

	const k = 5
	const multiChunks = 40
	const singles = k - 1
	gen, query := seedRecallCorpus(t, b, db, multiChunks, singles)
	require.NoError(t, b.ActivateGeneration(ctx, gen), "ActivateGeneration")

	hits, _, err := b.FusedSearch(ctx, vector.FusedRequest{
		QueryVec:   query,
		Generation: gen,
		KPerSignal: k,
		Limit:      k,
		RRFK:       60,
	})
	require.NoError(t, err, "FusedSearch")
	require.Len(t, hits, k, "FusedSearch must return k distinct messages despite the multi-chunk message")

	seen := map[int64]struct{}{}
	for _, h := range hits {
		seen[h.MessageID] = struct{}{}
	}
	assert.Len(t, seen, k, "fused hits must be k distinct messages")
}
