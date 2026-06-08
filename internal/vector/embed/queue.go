package embed

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/vector"
)

// Queue wraps pending_embeddings with a crash-safe claim-mark-complete
// pattern. A claim atomically marks up to N available rows with a token
// and the current timestamp; Complete deletes the rows (on success) and
// Release clears the claim (on failure). Rows whose claims are older
// than a configurable cutoff can be reclaimed via ReclaimStale, so a
// crashed worker does not strand pending work.
type Queue struct {
	db     *sql.DB
	rebind func(string) string
	// isPG is true when the underlying driver is PostgreSQL. When set,
	// Claim uses FOR UPDATE SKIP LOCKED in the inner SELECT to prevent
	// concurrent workers from claiming the same pending rows.
	isPG bool
}

// NewQueue returns a Queue bound to db. The caller retains ownership of
// db; Queue does not close it. rebind translates ?-placeholders to the
// driver's native form; pass an identity function (or nil) for SQLite
// and the PostgreSQL dialect's Rebind for pgx.
//
// The Queue detects whether the backend is PostgreSQL by probing rebind:
// if rebind("?") == "$1" the driver is pgx and Claim will use
// FOR UPDATE SKIP LOCKED to prevent concurrent workers from double-claiming.
func NewQueue(db *sql.DB, rebind func(string) string) *Queue {
	if rebind == nil {
		rebind = func(q string) string { return q }
	}
	return &Queue{db: db, rebind: rebind, isPG: rebind("?") == "$1"}
}

// Claim marks up to batch pending rows for gen as claimed by a fresh
// token, returning the message IDs in ascending order alongside the
// token to present to Complete or Release.
//
// If batch <= 0, or no rows are available, Claim returns (nil, "", nil).
// Returning an empty token for "no work" avoids asking callers to hold a
// dead token.
func (q *Queue) Claim(ctx context.Context, gen vector.GenerationID, batch int) ([]int64, string, error) {
	if batch <= 0 {
		return nil, "", nil
	}
	token, err := newToken()
	if err != nil {
		return nil, "", fmt.Errorf("new token: %w", err)
	}
	now := time.Now().Unix()

	tx, err := q.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, "", fmt.Errorf("begin claim tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// claimSQL selects the candidate rows for the UPDATE. PostgreSQL uses
	// FOR UPDATE SKIP LOCKED so that concurrent workers each see a disjoint
	// slice of available rows; without it two workers can select the same
	// rows in their subquery snapshots and the later UPDATE will simply
	// overwrite the earlier claim token, causing duplicate work.
	// SQLite serializes writers at the file level so no advisory locking is
	// needed there (and it does not support the FOR UPDATE syntax).
	claimSubquery := `
               SELECT generation_id, message_id
                 FROM pending_embeddings
                WHERE generation_id = ?
                  AND claimed_at IS NULL
                ORDER BY message_id
                LIMIT ?`
	if q.isPG {
		claimSubquery += `
                FOR UPDATE SKIP LOCKED`
	}
	claimSQL := `
        UPDATE pending_embeddings
           SET claimed_at = ?, claim_token = ?
         WHERE (generation_id, message_id) IN (` + claimSubquery + `)
        RETURNING message_id`

	ids, err := func() ([]int64, error) {
		rows, err := tx.QueryContext(ctx, q.rebind(claimSQL),
			now, token, int64(gen), batch)
		if err != nil {
			return nil, fmt.Errorf("claim query: %w", err)
		}
		defer func() { _ = rows.Close() }()
		var out []int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				return nil, fmt.Errorf("scan claimed id: %w", err)
			}
			out = append(out, id)
		}
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("claim rows: %w", err)
		}
		return out, nil
	}()
	if err != nil {
		return nil, "", err
	}
	if err := tx.Commit(); err != nil {
		return nil, "", fmt.Errorf("commit claim: %w", err)
	}
	if len(ids) == 0 {
		return nil, "", nil
	}
	// The subquery's ORDER BY decides WHICH rows get claimed, but
	// RETURNING does not guarantee order. Sort explicitly so callers
	// can rely on ascending ids (matters for deterministic test
	// assertions and for pairing ids with fetched message bodies by
	// position).
	slices.Sort(ids)
	return ids, token, nil
}

// Complete deletes the claimed rows from the queue. Only rows whose
// claim_token matches token are removed; any row that was reclaimed or
// re-claimed under a different token is left in place. A nil or empty
// ids slice is a no-op.
func (q *Queue) Complete(ctx context.Context, gen vector.GenerationID, token string, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	in := inPlaceholders(len(ids))
	args := make([]any, 0, 2+len(ids))
	args = append(args, int64(gen), token)
	for _, id := range ids {
		args = append(args, id)
	}
	query := q.rebind(`
        DELETE FROM pending_embeddings
         WHERE generation_id = ?
           AND claim_token   = ?
           AND message_id IN ` + in)
	if _, err := q.db.ExecContext(ctx, query, args...); err != nil {
		return fmt.Errorf("delete pending: %w", err)
	}
	return nil
}

// Release returns claimed rows to the pool so another worker can pick
// them up (for embedding failures). Only rows whose claim_token matches
// token are released. A nil or empty ids slice is a no-op.
func (q *Queue) Release(ctx context.Context, gen vector.GenerationID, token string, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	in := inPlaceholders(len(ids))
	args := make([]any, 0, 2+len(ids))
	args = append(args, int64(gen), token)
	for _, id := range ids {
		args = append(args, id)
	}
	query := q.rebind(`
        UPDATE pending_embeddings
           SET claimed_at = NULL, claim_token = NULL
         WHERE generation_id = ?
           AND claim_token   = ?
           AND message_id IN ` + in)
	if _, err := q.db.ExecContext(ctx, query, args...); err != nil {
		return fmt.Errorf("release: %w", err)
	}
	return nil
}

// ReclaimStale clears the claim on any pending row whose claimed_at is
// older than olderThan. Returns the number of rows reclaimed.
func (q *Queue) ReclaimStale(ctx context.Context, olderThan time.Duration) (int, error) {
	cutoff := time.Now().Add(-olderThan).Unix()
	res, err := q.db.ExecContext(ctx, q.rebind(`
        UPDATE pending_embeddings
           SET claimed_at = NULL, claim_token = NULL
         WHERE claimed_at IS NOT NULL AND claimed_at < ?`), cutoff)
	if err != nil {
		return 0, fmt.Errorf("reclaim stale: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("rows affected: %w", err)
	}
	return int(n), nil
}

// inPlaceholders returns "(?,?,...)" with n placeholders, for building
// IN clauses dynamically. The output uses ? regardless of dialect; the
// caller is expected to run the surrounding query through rebind.
func inPlaceholders(n int) string {
	ph := make([]string, n)
	for i := range ph {
		ph[i] = "?"
	}
	return "(" + strings.Join(ph, ",") + ")"
}

// newToken returns 16 hex characters backed by 8 bytes of crypto/rand.
func newToken() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("read random: %w", err)
	}
	return hex.EncodeToString(b), nil
}
