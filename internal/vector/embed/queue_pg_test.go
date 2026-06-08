//go:build pgvector

package embed

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/vector/pgvector"
)

// openPGQueueDB stands up a per-test schema on MSGVAULT_TEST_DB, applies
// the pgvector schema, and seeds one building generation with n pending
// rows. It returns the *sql.DB; cleanup drops the schema via t.Cleanup.
func openPGQueueDB(t *testing.T, n int) *sql.DB {
	t.Helper()
	url := os.Getenv("MSGVAULT_TEST_DB")
	if !strings.HasPrefix(url, "postgres://") && !strings.HasPrefix(url, "postgresql://") {
		t.Skip("pgvector queue tests require MSGVAULT_TEST_DB to point at a PostgreSQL DSN")
	}

	buf := make([]byte, 8)
	_, err := rand.Read(buf)
	require.NoError(t, err, "random schema name")
	schemaName := "embed_q_test_" + hex.EncodeToString(buf)

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

	ctx := context.Background()
	require.NoError(t, pgvector.Migrate(ctx, db, 0), "pgvector.Migrate")

	_, err = db.ExecContext(ctx, `
		INSERT INTO index_generations (id, model, dimension, fingerprint, started_at, state)
		OVERRIDING SYSTEM VALUE
		VALUES (1, 'm', 768, 'm:768', 0, 'building')`)
	require.NoError(t, err, "insert generation")
	for i := 1; i <= n; i++ {
		_, err := db.ExecContext(ctx,
			`INSERT INTO pending_embeddings (generation_id, message_id, enqueued_at) VALUES (1, $1, 0)`,
			i)
		require.NoError(t, err, "insert pending")
	}
	return db
}

func pgCountAvailable(t *testing.T, db *sql.DB, gen int64) int {
	t.Helper()
	var n int
	err := db.QueryRow(
		`SELECT COUNT(*) FROM pending_embeddings WHERE generation_id = $1 AND claimed_at IS NULL`,
		gen).Scan(&n)
	require.NoError(t, err, "countAvailable")
	return n
}

func pgRebind() func(string) string {
	return (&store.PostgreSQLDialect{}).Rebind
}

func TestQueuePG_ClaimReleaseComplete(t *testing.T) {
	ctx := context.Background()
	db := openPGQueueDB(t, 5)
	q := NewQueue(db, pgRebind())

	ids, token, err := q.Claim(ctx, 1, 3)
	require.NoError(t, err, "Claim")
	require.Len(t, ids, 3)
	require.NotEmpty(t, token)

	more, token2, err := q.Claim(ctx, 1, 10)
	require.NoError(t, err)
	assert.Len(t, more, 2)
	assert.NotEqual(t, token, token2, "second claim must use a fresh token")

	require.NoError(t, q.Release(ctx, 1, token, ids), "Release")
	assert.Equal(t, 3, pgCountAvailable(t, db, 1), "available after release")

	require.NoError(t, q.Complete(ctx, 1, token2, more), "Complete")
	var total int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM pending_embeddings`).Scan(&total))
	assert.Equal(t, 3, total, "pending total after complete = 5 - 2")
}

func TestQueuePG_Claim_EmptyBatchIsNoop(t *testing.T) {
	ctx := context.Background()
	db := openPGQueueDB(t, 1)
	q := NewQueue(db, pgRebind())
	ids, token, err := q.Claim(ctx, 1, 0)
	require.NoError(t, err, "Claim(0)")
	assert.Empty(t, ids)
	assert.Empty(t, token)
}

func TestQueuePG_Claim_NoAvailableReturnsEmpty(t *testing.T) {
	ctx := context.Background()
	db := openPGQueueDB(t, 0)
	q := NewQueue(db, pgRebind())
	ids, token, err := q.Claim(ctx, 1, 10)
	require.NoError(t, err, "Claim")
	assert.Empty(t, ids)
	assert.Empty(t, token)
}

func TestQueuePG_Complete_WrongTokenNoop(t *testing.T) {
	ctx := context.Background()
	db := openPGQueueDB(t, 2)
	q := NewQueue(db, pgRebind())
	ids, _, err := q.Claim(ctx, 1, 2)
	require.NoError(t, err)
	require.NoError(t, q.Complete(ctx, 1, "deadbeef", ids), "Complete with wrong token")
	var n int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM pending_embeddings`).Scan(&n))
	assert.Equal(t, 2, n, "Complete should not delete on token mismatch")
}

func TestQueuePG_Release_WrongTokenNoop(t *testing.T) {
	ctx := context.Background()
	db := openPGQueueDB(t, 2)
	q := NewQueue(db, pgRebind())
	ids, _, err := q.Claim(ctx, 1, 2)
	require.NoError(t, err)
	require.NoError(t, q.Release(ctx, 1, "deadbeef", ids), "Release with wrong token")
	assert.Equal(t, 0, pgCountAvailable(t, db, 1), "available after wrong-token release (still claimed)")
}

func TestQueuePG_ReclaimStale(t *testing.T) {
	ctx := context.Background()
	db := openPGQueueDB(t, 2)
	q := NewQueue(db, pgRebind())
	_, _, err := q.Claim(ctx, 1, 2)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx,
		`UPDATE pending_embeddings SET claimed_at = $1 WHERE generation_id = 1`,
		time.Now().Add(-20*time.Minute).Unix())
	require.NoError(t, err)
	n, err := q.ReclaimStale(ctx, 10*time.Minute)
	require.NoError(t, err)
	assert.Equal(t, 2, n, "reclaimed")
	assert.Equal(t, 2, pgCountAvailable(t, db, 1), "available after reclaim")
}

func TestQueuePG_Complete_EmptyIDsIsNoop(t *testing.T) {
	ctx := context.Background()
	db := openPGQueueDB(t, 1)
	q := NewQueue(db, pgRebind())
	assert.NoError(t, q.Complete(ctx, 1, "token", nil), "Complete(nil)")
}

func TestQueuePG_Claim_ReturnsIDsAscending(t *testing.T) {
	ctx := context.Background()
	db := openPGQueueDB(t, 10)
	q := NewQueue(db, pgRebind())

	ids, _, err := q.Claim(ctx, 1, 10)
	require.NoError(t, err, "Claim")
	require.Len(t, ids, 10)
	assert.True(t, sort.SliceIsSorted(ids, func(i, j int) bool { return ids[i] < ids[j] }),
		"ids not ascending: %v", ids)
}

func TestQueuePG_Complete_AfterReclaim_PreservesNewClaim(t *testing.T) {
	ctx := context.Background()
	db := openPGQueueDB(t, 2)
	q := NewQueue(db, pgRebind())

	idsA, tokenA, err := q.Claim(ctx, 1, 2)
	require.NoError(t, err, "Claim A")
	require.Len(t, idsA, 2)

	_, err = db.ExecContext(ctx,
		`UPDATE pending_embeddings SET claimed_at = $1 WHERE generation_id = 1`,
		time.Now().Add(-20*time.Minute).Unix())
	require.NoError(t, err)
	n, err := q.ReclaimStale(ctx, 10*time.Minute)
	require.NoError(t, err, "ReclaimStale")
	require.Equal(t, 2, n, "ReclaimStale count")

	idsB, tokenB, err := q.Claim(ctx, 1, 2)
	require.NoError(t, err, "Claim B")
	require.Len(t, idsB, 2)
	require.NotEqual(t, tokenA, tokenB)

	require.NoError(t, q.Complete(ctx, 1, tokenA, idsA), "Complete(stale tokenA)")
	var remaining int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM pending_embeddings`).Scan(&remaining))
	require.Equal(t, 2, remaining, "stale token must not delete")

	var claimed int
	require.NoError(t, db.QueryRow(
		`SELECT COUNT(*) FROM pending_embeddings WHERE claim_token = $1`, tokenB).Scan(&claimed))
	assert.Equal(t, 2, claimed, "rows still holding B's token")

	require.NoError(t, q.Complete(ctx, 1, tokenB, idsB), "Complete(tokenB)")
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM pending_embeddings`).Scan(&remaining))
	assert.Equal(t, 0, remaining, "pending rows after B's Complete")
}

// TestQueuePG_ConcurrentClaim_SkipLocked verifies that FOR UPDATE SKIP LOCKED
// prevents two concurrent claimers from double-claiming the same rows. Each
// claimer runs on a separate *sql.DB (independent connection pool) so the
// claims are genuinely concurrent at the database level.
//
// The test inserts N pending rows, then fires two goroutines each calling
// Claim(N) concurrently. Because SKIP LOCKED makes the two transactions see
// disjoint available sets, the union of their claimed IDs must equal exactly
// the N inserted rows with no overlaps.
func TestQueuePG_ConcurrentClaim_SkipLocked(t *testing.T) {
	const n = 20
	ctx := context.Background()

	url := os.Getenv("MSGVAULT_TEST_DB")
	if !strings.HasPrefix(url, "postgres://") && !strings.HasPrefix(url, "postgresql://") {
		t.Skip("pgvector queue tests require MSGVAULT_TEST_DB to point at a PostgreSQL DSN")
	}

	// Create an isolated schema for this test.
	buf := make([]byte, 8)
	_, err := rand.Read(buf)
	require.NoError(t, err, "random schema name")
	schemaName := "embed_conc_test_" + hex.EncodeToString(buf)

	setup, err := sql.Open("pgx", url)
	require.NoError(t, err, "open setup")
	defer func() { _ = setup.Close() }()
	_, err = setup.Exec("CREATE SCHEMA " + schemaName)
	require.NoError(t, err, "create schema")

	sep := "?"
	if strings.Contains(url, "?") {
		sep = "&"
	}
	testURL := url + sep + "search_path=" + schemaName + ",public"

	db1, err := sql.Open("pgx", testURL)
	require.NoError(t, err, "open db1")
	t.Cleanup(func() {
		_ = db1.Close()
		cleanup, cerr := sql.Open("pgx", url)
		if cerr != nil {
			return
		}
		defer func() { _ = cleanup.Close() }()
		_, _ = cleanup.Exec("DROP SCHEMA " + schemaName + " CASCADE")
	})

	require.NoError(t, pgvector.Migrate(ctx, db1, 0), "pgvector.Migrate")

	_, err = db1.ExecContext(ctx, `
		INSERT INTO index_generations (id, model, dimension, fingerprint, started_at, state)
		OVERRIDING SYSTEM VALUE
		VALUES (1, 'm', 768, 'm:768', 0, 'building')`)
	require.NoError(t, err, "insert generation")
	for i := 1; i <= n; i++ {
		_, err := db1.ExecContext(ctx,
			`INSERT INTO pending_embeddings (generation_id, message_id, enqueued_at) VALUES (1, $1, 0)`, i)
		require.NoError(t, err, "insert pending row %d", i)
	}

	// Open a second independent connection so the two Queue instances use
	// separate connection pools and their transactions do not share state.
	db2, err := sql.Open("pgx", testURL)
	require.NoError(t, err, "open db2")
	t.Cleanup(func() { _ = db2.Close() })

	q1 := NewQueue(db1, pgRebind())
	q2 := NewQueue(db2, pgRebind())

	type result struct {
		ids   []int64
		token string
		err   error
	}
	ch := make(chan result, 2)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		ids, tok, err := q1.Claim(ctx, 1, n)
		ch <- result{ids, tok, err}
	}()
	go func() {
		defer wg.Done()
		ids, tok, err := q2.Claim(ctx, 1, n)
		ch <- result{ids, tok, err}
	}()
	wg.Wait()
	close(ch)

	var allIDs []int64
	for res := range ch {
		require.NoError(t, res.err, "Claim must not error")
		allIDs = append(allIDs, res.ids...)
	}

	// The union of claimed IDs must equal exactly {1..n} with no duplicates.
	sort.Slice(allIDs, func(i, j int) bool { return allIDs[i] < allIDs[j] })
	require.Len(t, allIDs, n, "total claimed rows must equal n (no rows unclaimed and no duplicates)")
	for i, id := range allIDs {
		assert.Equal(t, int64(i+1), id, "claimed ID at position %d", i)
	}
}
