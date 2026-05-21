//go:build pgvector

package embed

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
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/vector/pgvector"
)

func openPGQueueDB(t *testing.T, n int) *sql.DB {
	t.Helper()
	url := os.Getenv("MSGVAULT_TEST_DB")
	if !(strings.HasPrefix(url, "postgres://") || strings.HasPrefix(url, "postgresql://")) {
		t.Skip("pgvector queue tests require MSGVAULT_TEST_DB to point at a PostgreSQL DSN")
	}

	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		t.Fatalf("random schema name: %v", err)
	}
	schemaName := "embed_q_test_" + hex.EncodeToString(buf)

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

	ctx := context.Background()
	if err := pgvector.Migrate(ctx, db, 0); err != nil {
		t.Fatalf("pgvector.Migrate: %v", err)
	}

	if _, err := db.ExecContext(ctx, `
		INSERT INTO index_generations (id, model, dimension, fingerprint, started_at, state)
		OVERRIDING SYSTEM VALUE
		VALUES (1, 'm', 768, 'm:768', 0, 'building')`); err != nil {
		t.Fatalf("insert generation: %v", err)
	}
	for i := 1; i <= n; i++ {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO pending_embeddings (generation_id, message_id, enqueued_at) VALUES (1, $1, 0)`,
			i); err != nil {
			t.Fatalf("insert pending: %v", err)
		}
	}
	return db
}

func pgCountAvailable(t *testing.T, db *sql.DB, gen int64) int {
	t.Helper()
	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM pending_embeddings WHERE generation_id = $1 AND claimed_at IS NULL`,
		gen).Scan(&n); err != nil {
		t.Fatalf("countAvailable: %v", err)
	}
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
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if len(ids) != 3 || token == "" {
		t.Fatalf("claimed ids=%v token=%q, want 3 ids and non-empty token", ids, token)
	}

	more, token2, err := q.Claim(ctx, 1, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(more) != 2 || token2 == token {
		t.Errorf("second claim got %d ids (want 2) / token collision=%v", len(more), token2 == token)
	}

	if err := q.Release(ctx, 1, token, ids); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if got := pgCountAvailable(t, db, 1); got != 3 {
		t.Errorf("available after release = %d, want 3", got)
	}

	if err := q.Complete(ctx, 1, token2, more); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	var total int
	if err := db.QueryRow(`SELECT COUNT(*) FROM pending_embeddings`).Scan(&total); err != nil {
		t.Fatalf("total: %v", err)
	}
	if total != 3 {
		t.Errorf("pending total after complete = %d, want 3 (5 - 2)", total)
	}
}

func TestQueuePG_Claim_EmptyBatchIsNoop(t *testing.T) {
	ctx := context.Background()
	db := openPGQueueDB(t, 1)
	q := NewQueue(db, pgRebind())
	ids, token, err := q.Claim(ctx, 1, 0)
	if err != nil {
		t.Fatalf("Claim(0): %v", err)
	}
	if len(ids) != 0 || token != "" {
		t.Errorf("expected empty ids and token, got ids=%v token=%q", ids, token)
	}
}

func TestQueuePG_Claim_NoAvailableReturnsEmpty(t *testing.T) {
	ctx := context.Background()
	db := openPGQueueDB(t, 0)
	q := NewQueue(db, pgRebind())
	ids, token, err := q.Claim(ctx, 1, 10)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if len(ids) != 0 || token != "" {
		t.Errorf("expected empty ids and token with no available, got %v %q", ids, token)
	}
}

func TestQueuePG_Complete_WrongTokenNoop(t *testing.T) {
	ctx := context.Background()
	db := openPGQueueDB(t, 2)
	q := NewQueue(db, pgRebind())
	ids, _, err := q.Claim(ctx, 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	if err := q.Complete(ctx, 1, "deadbeef", ids); err != nil {
		t.Fatalf("Complete with wrong token: %v", err)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM pending_embeddings`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("rows remaining = %d, want 2 (Complete should not delete on token mismatch)", n)
	}
}

func TestQueuePG_Release_WrongTokenNoop(t *testing.T) {
	ctx := context.Background()
	db := openPGQueueDB(t, 2)
	q := NewQueue(db, pgRebind())
	ids, _, err := q.Claim(ctx, 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	if err := q.Release(ctx, 1, "deadbeef", ids); err != nil {
		t.Fatalf("Release with wrong token: %v", err)
	}
	if got := pgCountAvailable(t, db, 1); got != 0 {
		t.Errorf("available after wrong-token release = %d, want 0 (still claimed)", got)
	}
}

func TestQueuePG_ReclaimStale(t *testing.T) {
	ctx := context.Background()
	db := openPGQueueDB(t, 2)
	q := NewQueue(db, pgRebind())
	_, _, err := q.Claim(ctx, 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE pending_embeddings SET claimed_at = $1 WHERE generation_id = 1`,
		time.Now().Add(-20*time.Minute).Unix()); err != nil {
		t.Fatal(err)
	}
	n, err := q.ReclaimStale(ctx, 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("reclaimed %d, want 2", n)
	}
	if got := pgCountAvailable(t, db, 1); got != 2 {
		t.Errorf("available after reclaim = %d, want 2", got)
	}
}

func TestQueuePG_Complete_EmptyIDsIsNoop(t *testing.T) {
	ctx := context.Background()
	db := openPGQueueDB(t, 1)
	q := NewQueue(db, pgRebind())
	if err := q.Complete(ctx, 1, "token", nil); err != nil {
		t.Errorf("Complete(nil): %v", err)
	}
}

func TestQueuePG_Claim_ReturnsIDsAscending(t *testing.T) {
	ctx := context.Background()
	db := openPGQueueDB(t, 10)
	q := NewQueue(db, pgRebind())

	ids, _, err := q.Claim(ctx, 1, 10)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if len(ids) != 10 {
		t.Fatalf("len(ids) = %d, want 10", len(ids))
	}
	if !sort.SliceIsSorted(ids, func(i, j int) bool { return ids[i] < ids[j] }) {
		t.Errorf("ids not ascending: %v", ids)
	}
}

func TestQueuePG_Complete_AfterReclaim_PreservesNewClaim(t *testing.T) {
	ctx := context.Background()
	db := openPGQueueDB(t, 2)
	q := NewQueue(db, pgRebind())

	idsA, tokenA, err := q.Claim(ctx, 1, 2)
	if err != nil {
		t.Fatalf("Claim A: %v", err)
	}
	if len(idsA) != 2 {
		t.Fatalf("Claim A ids=%v, want 2", idsA)
	}

	if _, err := db.ExecContext(ctx,
		`UPDATE pending_embeddings SET claimed_at = $1 WHERE generation_id = 1`,
		time.Now().Add(-20*time.Minute).Unix()); err != nil {
		t.Fatal(err)
	}
	if n, err := q.ReclaimStale(ctx, 10*time.Minute); err != nil || n != 2 {
		t.Fatalf("ReclaimStale: n=%d err=%v, want n=2 err=nil", n, err)
	}

	idsB, tokenB, err := q.Claim(ctx, 1, 2)
	if err != nil {
		t.Fatalf("Claim B: %v", err)
	}
	if len(idsB) != 2 || tokenB == tokenA {
		t.Fatalf("Claim B ids=%v token=%q (A=%q)", idsB, tokenB, tokenA)
	}

	if err := q.Complete(ctx, 1, tokenA, idsA); err != nil {
		t.Fatalf("Complete(stale tokenA): %v", err)
	}
	var remaining int
	if err := db.QueryRow(`SELECT COUNT(*) FROM pending_embeddings`).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 2 {
		t.Fatalf("pending rows after stale Complete = %d, want 2 (stale token must not delete)", remaining)
	}

	var claimed int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM pending_embeddings WHERE claim_token = $1`, tokenB).Scan(&claimed); err != nil {
		t.Fatal(err)
	}
	if claimed != 2 {
		t.Errorf("rows still holding B's token = %d, want 2", claimed)
	}

	if err := q.Complete(ctx, 1, tokenB, idsB); err != nil {
		t.Fatalf("Complete(tokenB): %v", err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM pending_embeddings`).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 0 {
		t.Errorf("pending rows after B's Complete = %d, want 0", remaining)
	}
}
