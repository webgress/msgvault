package store_test

import (
	"database/sql"
	"os"
	"strings"
	"testing"

	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/testutil/storetest"
)

// skipUnlessPostgres skips the calling test unless MSGVAULT_TEST_DB points at
// PostgreSQL. The tsvector 1MB limit and its body cap are PostgreSQL-only —
// SQLite's FTS5 imposes no such bound, so these tests have nothing to exercise
// on SQLite.
func skipUnlessPostgres(t *testing.T) {
	t.Helper()
	testDB := os.Getenv("MSGVAULT_TEST_DB")
	if !strings.HasPrefix(testDB, "postgres://") && !strings.HasPrefix(testDB, "postgresql://") {
		t.Skip("PG-only: tsvector body cap; requires MSGVAULT_TEST_DB pointing at PostgreSQL")
	}
}

// oversizedBody builds a body of distinct tokens whose total length exceeds the
// PostgreSQL tsvector cap. Each token is unique so to_tsvector would, without a
// cap, produce a tsvector large enough to trip the 1MB "string is too long for
// tsvector" error. ~1.5MB of "tokNNNNNNN " words yields well over 100k lexemes.
func oversizedBody() string {
	var b strings.Builder
	b.Grow(1_600_000)
	i := 0
	for b.Len() < 1_500_000 {
		b.WriteString("tok")
		b.WriteString(itoaPad(i))
		b.WriteByte(' ')
		i++
	}
	return b.String()
}

// itoaPad renders n as a zero-padded 7-digit string so every token is the same
// width and reliably distinct.
func itoaPad(n int) string {
	const digits = "0123456789"
	buf := []byte("0000000")
	for i := len(buf) - 1; i >= 0 && n > 0; i-- {
		buf[i] = digits[n%10]
		n /= 10
	}
	return string(buf)
}

func nullSearchFTSCount(t *testing.T, st interface{ DB() *sql.DB }) int {
	t.Helper()
	var n int
	requirepkg.NoError(t, st.DB().QueryRow(
		"SELECT COUNT(*) FROM messages WHERE search_fts IS NULL").Scan(&n),
		"count NULL search_fts")
	return n
}

// TestPG_FTSUpsert_OversizedBodyTruncates (finding T2a) verifies that indexing
// a single message whose body exceeds the tsvector cap SUCCEEDS on PostgreSQL —
// the body is truncated (LEFT cap) rather than the UPDATE erroring — and leaves
// search_fts non-NULL. Without the cap this would fail with SQLSTATE 54000
// ("string is too long for tsvector") and leave the row permanently NULL.
func TestPG_FTSUpsert_OversizedBodyTruncates(t *testing.T) {
	skipUnlessPostgres(t)
	f := storetest.New(t)
	requirepkg.True(t, f.Store.FTS5Available(), "FTS must be available on PG")

	msgID := f.CreateMessage("oversized-upsert")

	// UpsertFTS routes through PostgreSQLDialect.FTSUpsert, where the LEFT cap
	// lives. A >1MB body must not error.
	err := f.Store.UpsertFTS(msgID, "subject line", oversizedBody(),
		"alice@example.com", "bob@example.com", "")
	requirepkg.NoError(t, err, "UpsertFTS with oversized body must succeed (truncated indexing)")

	var isNull bool
	requirepkg.NoError(t, f.Store.DB().QueryRow(
		"SELECT search_fts IS NULL FROM messages WHERE id = $1", msgID).Scan(&isNull),
		"probe search_fts")
	assertpkg.False(t, isNull, "search_fts must be non-NULL after truncated upsert")
}

// TestPG_BackfillFTS_OversizedBodyDoesNotWedge (finding T2b) seeds >5000
// messages with one oversized body in a middle batch, then runs BackfillFTS.
// It must complete WITHOUT error and every row — including those AFTER the
// oversized one — must end with non-NULL search_fts. The LEFT cap makes the
// oversized row index fine; the row-by-row retry fallback is the belt-and-
// suspenders guarantee that no single bad row can wedge later batches.
func TestPG_BackfillFTS_OversizedBodyDoesNotWedge(t *testing.T) {
	skipUnlessPostgres(t)
	f := storetest.New(t)
	requirepkg.True(t, f.Store.FTS5Available(), "FTS must be available on PG")

	// 5500 messages spans at least two 5000-row backfill batches. The oversized
	// body lands at index 2500 — inside the FIRST batch — so we can assert that
	// every row in LATER batches still gets indexed.
	const total = 5500
	const oversizedIdx = 2500
	ids := f.CreateMessages(total)
	requirepkg.Len(t, ids, total)

	requirepkg.NoError(t, f.Store.UpsertMessageBody(ids[oversizedIdx],
		sql.NullString{String: oversizedBody(), Valid: true}, sql.NullString{}),
		"attach oversized body")

	// Give a couple of normal rows after the oversized one bodies too, so the
	// "rows after the bad one are indexed" claim is concrete.
	requirepkg.NoError(t, f.Store.UpsertMessageBody(ids[total-1],
		sql.NullString{String: "final sentinel body apricot", Valid: true}, sql.NullString{}),
		"attach sentinel body")

	n, err := f.Store.BackfillFTS(nil)
	requirepkg.NoError(t, err, "BackfillFTS must complete despite an oversized body")
	assertpkg.Equal(t, int64(total), n, "every message should be indexed")

	// No row may be left NULL — not the oversized one, nor any after it.
	assertpkg.Equal(t, 0, nullSearchFTSCount(t, f.Store),
		"no message may be left with NULL search_fts after backfill")

	// The sentinel row (last id, after the oversized one) is searchable.
	_, sentinelTotal, err := f.Store.SearchMessages("apricot", 0, 10)
	requirepkg.NoError(t, err, "SearchMessages apricot")
	assertpkg.Equal(t, int64(1), sentinelTotal, "sentinel row after oversized must be searchable")
}
