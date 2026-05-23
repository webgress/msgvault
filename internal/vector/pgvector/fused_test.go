//go:build pgvector

package pgvector

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"testing"
	"time"

	"go.kenn.io/msgvault/internal/vector"
)

// fusedFixture wires up a per-test schema with a richer messages
// table than backend_testhelpers_test.go's stripped-down version —
// fused search needs subject + search_fts + sent_at — and seeds a
// small synthetic corpus plus a matching set of embeddings.
type fusedFixture struct {
	b   *Backend
	ctx context.Context
	db  *sql.DB
	gen vector.GenerationID
}

func newFusedFixture(t *testing.T) *fusedFixture {
	t.Helper()
	db := openPGTestDB(t)
	if _, err := db.Exec(`ALTER TABLE messages ADD COLUMN IF NOT EXISTS search_fts TSVECTOR`); err != nil {
		t.Fatalf("add search_fts: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS attachments (
        id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
        message_id BIGINT NOT NULL,
        filename TEXT
    )`); err != nil {
		t.Fatalf("create attachments: %v", err)
	}
	ctx := context.Background()
	b, err := Open(ctx, Options{DB: db, Dimension: 4})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = b.Close() })
	return &fusedFixture{b: b, ctx: ctx, db: db}
}

// seed inserts a row in messages, optionally an attachment, and
// updates search_fts using the same 'simple' config the fused query
// uses on its side.
func (f *fusedFixture) seedMsg(t *testing.T, id int64, subject, body string, sourceID int64, sentAt time.Time, hasAttachment bool) {
	t.Helper()
	if _, err := f.db.ExecContext(f.ctx,
		`INSERT INTO messages (id, source_id, subject, sent_at, has_attachments)
         VALUES ($1, $2, $3, $4, $5)`,
		id, sourceID, subject, sentAt, hasAttachment); err != nil {
		t.Fatalf("insert msg %d: %v", id, err)
	}
	if _, err := f.db.ExecContext(f.ctx,
		`UPDATE messages SET search_fts =
            setweight(to_tsvector('simple', COALESCE($2, '')), 'A') ||
            to_tsvector('simple', COALESCE($3, ''))
         WHERE id = $1`,
		id, subject, body); err != nil {
		t.Fatalf("update search_fts %d: %v", id, err)
	}
	if hasAttachment {
		if _, err := f.db.ExecContext(f.ctx,
			`INSERT INTO attachments (message_id, filename) VALUES ($1, 'doc.pdf')`,
			id); err != nil {
			t.Fatalf("insert attachment %d: %v", id, err)
		}
	}
}

// embedAll creates a generation sized to the first vector and
// upserts every supplied chunk. The fixture-seeded message ids must
// exist before this is called.
func (f *fusedFixture) embedAll(t *testing.T, vecs map[int64][]float32) {
	t.Helper()
	gen, err := f.b.CreateGeneration(f.ctx, "m", 4)
	if err != nil {
		t.Fatalf("CreateGeneration: %v", err)
	}
	f.gen = gen
	chunks := make([]vector.Chunk, 0, len(vecs))
	for id, v := range vecs {
		chunks = append(chunks, vector.Chunk{MessageID: id, Vector: v})
	}
	if err := f.b.Upsert(f.ctx, gen, chunks); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if err := f.b.ActivateGeneration(f.ctx, gen); err != nil {
		t.Fatalf("Activate: %v", err)
	}
}

// seedThree wires up a 3-message corpus with distinct vectors and
// distinct keyword content so each test below can mix-and-match
// signal/filter combinations on the same data shape.
func seedThree(t *testing.T) *fusedFixture {
	t.Helper()
	f := newFusedFixture(t)
	base := time.Date(2025, 1, 15, 12, 0, 0, 0, time.UTC)
	f.seedMsg(t, 1, "alpha quantum project update", "discussing the quantum roadmap", 10, base, false)
	f.seedMsg(t, 2, "beta vector indexing notes", "notes about hybrid search and ranking", 20, base.AddDate(0, 0, 1), true)
	f.seedMsg(t, 3, "gamma project retrospective", "retro covering the quantum milestone", 10, base.AddDate(0, 0, 2), false)
	f.embedAll(t, map[int64][]float32{
		1: unitVec(4, 0),
		2: unitVec(4, 1),
		3: unitVec(4, 2),
	})
	return f
}

func TestFusedSearch_FTSOnly(t *testing.T) {
	f := seedThree(t)
	hits, saturated, err := f.b.FusedSearch(f.ctx, vector.FusedRequest{
		FTSQuery:   "quantum",
		Generation: f.gen,
		KPerSignal: 10,
		Limit:      10,
		RRFK:       60,
	})
	if err != nil {
		t.Fatalf("FusedSearch: %v", err)
	}
	if saturated {
		t.Errorf("saturated = true, want false (pool size 2 < KPerSignal 10)")
	}
	if len(hits) != 2 {
		t.Fatalf("hits len = %d, want 2 (msgs 1 and 3 mention 'quantum'); hits=%+v", len(hits), hits)
	}
	seen := map[int64]bool{}
	for i, h := range hits {
		seen[h.MessageID] = true
		if !math.IsNaN(h.VectorScore) {
			t.Errorf("hit[%d].VectorScore = %v, want NaN (FTS-only)", i, h.VectorScore)
		}
		if math.IsNaN(h.BM25Score) {
			t.Errorf("hit[%d].BM25Score = NaN, want a number (FTS-only)", i)
		}
		if h.RRFScore <= 0 {
			t.Errorf("hit[%d].RRFScore = %v, want > 0", i, h.RRFScore)
		}
	}
	if !seen[1] || !seen[3] {
		t.Errorf("expected msgs 1 and 3, got %v", seen)
	}
	// Hits ordered by RRFScore descending.
	for i := 1; i < len(hits); i++ {
		if hits[i-1].RRFScore < hits[i].RRFScore {
			t.Errorf("RRF not descending at %d: %v then %v", i, hits[i-1].RRFScore, hits[i].RRFScore)
		}
	}
}

func TestFusedSearch_ANNOnly(t *testing.T) {
	f := seedThree(t)
	hits, saturated, err := f.b.FusedSearch(f.ctx, vector.FusedRequest{
		QueryVec:   unitVec(4, 0),
		Generation: f.gen,
		KPerSignal: 10,
		Limit:      10,
		RRFK:       60,
	})
	if err != nil {
		t.Fatalf("FusedSearch: %v", err)
	}
	if saturated {
		t.Errorf("saturated = true, want false")
	}
	if len(hits) == 0 {
		t.Fatal("expected hits, got none")
	}
	if hits[0].MessageID != 1 {
		t.Errorf("top hit = %d, want 1 (query points along axis 0)", hits[0].MessageID)
	}
	for i, h := range hits {
		if !math.IsNaN(h.BM25Score) {
			t.Errorf("hit[%d].BM25Score = %v, want NaN (ANN-only)", i, h.BM25Score)
		}
		if math.IsNaN(h.VectorScore) {
			t.Errorf("hit[%d].VectorScore = NaN, want a number (ANN-only)", i)
		}
	}
}

func TestFusedSearch_Hybrid(t *testing.T) {
	f := seedThree(t)
	hits, saturated, err := f.b.FusedSearch(f.ctx, vector.FusedRequest{
		FTSQuery:   "quantum",
		QueryVec:   unitVec(4, 1), // points at msg 2 (no 'quantum')
		Generation: f.gen,
		KPerSignal: 10,
		Limit:      10,
		RRFK:       60,
	})
	if err != nil {
		t.Fatalf("FusedSearch: %v", err)
	}
	if saturated {
		t.Errorf("saturated = true, want false")
	}
	// Expect: msg 2 via ANN; msgs 1 and 3 via FTS — union of 3.
	if len(hits) != 3 {
		t.Fatalf("hits len = %d, want 3; hits=%+v", len(hits), hits)
	}
	for i := 1; i < len(hits); i++ {
		if hits[i-1].RRFScore < hits[i].RRFScore {
			t.Errorf("RRF not descending at %d: %v then %v", i, hits[i-1].RRFScore, hits[i].RRFScore)
		}
	}
}

func TestFusedSearch_Saturated(t *testing.T) {
	f := seedThree(t)
	hits, saturated, err := f.b.FusedSearch(f.ctx, vector.FusedRequest{
		FTSQuery:   "quantum",
		Generation: f.gen,
		KPerSignal: 1, // smaller than the 2-row FTS pool → saturates
		Limit:      10,
		RRFK:       60,
	})
	if err != nil {
		t.Fatalf("FusedSearch: %v", err)
	}
	if !saturated {
		t.Errorf("saturated = false, want true (KPerSignal=1, pool=2)")
	}
	if len(hits) != 1 {
		t.Errorf("hits len = %d, want 1 (trimmed to KPerSignal)", len(hits))
	}
}

func TestFusedSearch_FilterBySource(t *testing.T) {
	f := seedThree(t)
	hits, _, err := f.b.FusedSearch(f.ctx, vector.FusedRequest{
		FTSQuery:   "quantum",
		Generation: f.gen,
		KPerSignal: 10,
		Limit:      10,
		RRFK:       60,
		Filter:     vector.Filter{SourceIDs: []int64{20}},
	})
	if err != nil {
		t.Fatalf("FusedSearch: %v", err)
	}
	// SourceIDs={20} only allows msg 2 through, which doesn't match 'quantum'.
	if len(hits) != 0 {
		t.Errorf("hits = %+v, want empty (source 20 has no quantum match)", hits)
	}

	hits, _, err = f.b.FusedSearch(f.ctx, vector.FusedRequest{
		FTSQuery:   "quantum",
		Generation: f.gen,
		KPerSignal: 10,
		Limit:      10,
		RRFK:       60,
		Filter:     vector.Filter{SourceIDs: []int64{10}},
	})
	if err != nil {
		t.Fatalf("FusedSearch (source 10): %v", err)
	}
	if len(hits) != 2 {
		t.Errorf("hits len = %d, want 2 (msgs 1+3 in source 10 match quantum)", len(hits))
	}
}

func TestFusedSearch_FilterByDateRange(t *testing.T) {
	f := seedThree(t)
	after := time.Date(2025, 1, 16, 0, 0, 0, 0, time.UTC) // exclude msg 1
	hits, _, err := f.b.FusedSearch(f.ctx, vector.FusedRequest{
		FTSQuery:   "quantum",
		Generation: f.gen,
		KPerSignal: 10,
		Limit:      10,
		RRFK:       60,
		Filter:     vector.Filter{After: &after},
	})
	if err != nil {
		t.Fatalf("FusedSearch: %v", err)
	}
	// After 2025-01-16: msg 2 (2025-01-16) and msg 3 (2025-01-17) survive
	// the filter, but only msg 3 mentions 'quantum'.
	if len(hits) != 1 || hits[0].MessageID != 3 {
		t.Errorf("hits = %+v, want exactly msg 3", hits)
	}
}

func TestFusedSearch_FilterByLabel(t *testing.T) {
	f := seedThree(t)
	// Tag msg 3 with label_id 42 only.
	if _, err := f.db.ExecContext(f.ctx,
		`INSERT INTO message_labels (message_id, label_id) VALUES (3, 42)`); err != nil {
		t.Fatalf("insert label: %v", err)
	}
	hits, _, err := f.b.FusedSearch(f.ctx, vector.FusedRequest{
		FTSQuery:   "quantum",
		Generation: f.gen,
		KPerSignal: 10,
		Limit:      10,
		RRFK:       60,
		Filter:     vector.Filter{LabelGroups: [][]int64{{42}}},
	})
	if err != nil {
		t.Fatalf("FusedSearch: %v", err)
	}
	if len(hits) != 1 || hits[0].MessageID != 3 {
		t.Errorf("hits = %+v, want exactly msg 3 (labeled 42)", hits)
	}
}

func TestFusedSearch_FilterBySender(t *testing.T) {
	f := seedThree(t)
	// Add a 'from' recipient row for msg 1 only, participant_id=99.
	if _, err := f.db.ExecContext(f.ctx,
		`INSERT INTO message_recipients (message_id, recipient_type, participant_id)
         VALUES (1, 'from', 99)`); err != nil {
		t.Fatalf("insert from-recipient: %v", err)
	}
	hits, _, err := f.b.FusedSearch(f.ctx, vector.FusedRequest{
		FTSQuery:   "quantum",
		Generation: f.gen,
		KPerSignal: 10,
		Limit:      10,
		RRFK:       60,
		Filter:     vector.Filter{SenderGroups: [][]int64{{99}}},
	})
	if err != nil {
		t.Fatalf("FusedSearch: %v", err)
	}
	if len(hits) != 1 || hits[0].MessageID != 1 {
		t.Errorf("hits = %+v, want exactly msg 1 (only from=99)", hits)
	}
}

func TestFusedSearch_HasAttachment(t *testing.T) {
	f := seedThree(t)
	yes := true
	hits, _, err := f.b.FusedSearch(f.ctx, vector.FusedRequest{
		QueryVec:   unitVec(4, 1),
		Generation: f.gen,
		KPerSignal: 10,
		Limit:      10,
		RRFK:       60,
		Filter:     vector.Filter{HasAttachment: &yes},
	})
	if err != nil {
		t.Fatalf("FusedSearch: %v", err)
	}
	// Only msg 2 has an attachment.
	if len(hits) != 1 || hits[0].MessageID != 2 {
		t.Errorf("hits = %+v, want exactly msg 2", hits)
	}
}

func TestFusedSearch_RejectsEmptyRequest(t *testing.T) {
	f := seedThree(t)
	_, _, err := f.b.FusedSearch(f.ctx, vector.FusedRequest{
		Generation: f.gen,
		KPerSignal: 10,
		Limit:      10,
		RRFK:       60,
	})
	if err == nil {
		t.Fatal("expected error for empty fts/vec, got nil")
	}
}

func TestFusedSearch_UnknownGeneration(t *testing.T) {
	f := newFusedFixture(t)
	_, _, err := f.b.FusedSearch(f.ctx, vector.FusedRequest{
		FTSQuery:   "anything",
		Generation: 999,
		KPerSignal: 10,
		Limit:      10,
		RRFK:       60,
	})
	if err == nil {
		t.Fatal("expected error for unknown generation")
	}
}

func TestFusedSearch_DimensionMismatch(t *testing.T) {
	f := seedThree(t)
	_, _, err := f.b.FusedSearch(f.ctx, vector.FusedRequest{
		QueryVec:   []float32{1, 2, 3}, // 3-dim, gen is 4-dim
		Generation: f.gen,
		KPerSignal: 10,
		Limit:      10,
		RRFK:       60,
	})
	if err == nil {
		t.Fatal("expected dimension-mismatch error")
	}
}

// TestFusedSearch_SubjectBoost asserts that an entry with a matching
// subject substring is re-ranked above one that would otherwise win
// on RRF alone. Msg 2 (vector match) has the boosted subject token
// "vector"; msgs 1 and 3 win on FTS but don't contain "vector".
func TestFusedSearch_SubjectBoost(t *testing.T) {
	f := seedThree(t)
	hits, _, err := f.b.FusedSearch(f.ctx, vector.FusedRequest{
		FTSQuery:     "quantum",
		QueryVec:     unitVec(4, 1),
		Generation:   f.gen,
		KPerSignal:   10,
		Limit:        10,
		RRFK:         60,
		SubjectBoost: 50.0,
		SubjectTerms: []string{"vector"},
	})
	if err != nil {
		t.Fatalf("FusedSearch: %v", err)
	}
	if len(hits) == 0 || hits[0].MessageID != 2 {
		t.Errorf("top hit = %+v, want msg 2 (subject boosted)", hits)
	}
	if !hits[0].SubjectBoosted {
		t.Errorf("hit[0].SubjectBoosted = false, want true")
	}
}

// TestFusedSearch_SkipsDeletedMessages confirms the live-message
// predicate is applied — a soft-deleted message must not appear in
// either signal's pool.
func TestFusedSearch_SkipsDeletedMessages(t *testing.T) {
	f := seedThree(t)
	if _, err := f.db.ExecContext(f.ctx,
		`UPDATE messages SET deleted_from_source_at = NOW() WHERE id = 1`); err != nil {
		t.Fatalf("soft-delete: %v", err)
	}
	hits, _, err := f.b.FusedSearch(f.ctx, vector.FusedRequest{
		FTSQuery:   "quantum",
		QueryVec:   unitVec(4, 0),
		Generation: f.gen,
		KPerSignal: 10,
		Limit:      10,
		RRFK:       60,
	})
	if err != nil {
		t.Fatalf("FusedSearch: %v", err)
	}
	for _, h := range hits {
		if h.MessageID == 1 {
			t.Errorf("hits include soft-deleted msg 1: %+v", h)
		}
	}
}

// TestFusedSearch_FilterClausesUnused — sanity-check the static
// SQL-formatting code path: build an applyFilterClauses fragment by
// hand and assert it begins with a leading " AND " (so it can be
// safely concatenated after a WHERE predicate). The clause must
// reference each placeholder it claims to bind.
func TestFusedSearch_FilterClausesUnused(t *testing.T) {
	var args []any
	bind := func(v any) string {
		args = append(args, v)
		return fmt.Sprintf("$%d", len(args))
	}
	f := vector.Filter{SourceIDs: []int64{1, 2}}
	got := applyFilterClauses(f, bind)
	if len(args) != 1 {
		t.Errorf("args bound = %d, want 1", len(args))
	}
	if got == "" || got[:5] != " AND " {
		t.Errorf("clauses = %q, want leading ' AND '", got)
	}
}
