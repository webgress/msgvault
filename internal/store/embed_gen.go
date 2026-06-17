package store

import (
	"context"
	"fmt"
	"strings"
)

// embedGenStampChunkRows caps how many message ids go into a single
// SetEmbedGen UPDATE. Each statement binds one placeholder per id plus
// one for the target generation, so 500 ids = 501 bound parameters —
// comfortably under SQLite's historical 999 (and the store's 900-param
// convention; see insertInChunks) and PostgreSQL's 65,535. Mirrors the
// store's existing chunking discipline so an oversized embed batch never
// blows the driver bind ceiling. A var (not const) only so tests can
// lower it to exercise the chunk boundary; production never reassigns it.
var embedGenStampChunkRows = 500

// ScanForEmbedding returns up to limit live message ids that still need
// embedding for the target generation — i.e. rows whose embed_gen does
// not already equal target — scanning forward from afterID in id order.
//
// The portable predicate (embed_gen IS NULL OR embed_gen <> ?) covers
// both never-embedded rows (NULL) and rows stamped for a different
// generation, and avoids any IS DISTINCT FROM driver-version doubt. The
// forward bound (id > afterID) lets the caller resume from a per-gen
// watermark; pass 0 for a full scan (the backstop). Results are ordered
// by id so the caller can advance the watermark to the batch's max id.
//
// This runs against the MAIN db (messages + embed_gen live there on both
// backends). On SQLite the embeddings themselves live in vectors.db, so
// this find-work query and the SetEmbedGen stamp cannot share a tx with
// the embeddings upsert — the worker orders the steps (upsert, then
// stamp) and relies on idempotency, see internal/vector/embed/worker.go.
func (s *Store) ScanForEmbedding(ctx context.Context, target int64, afterID int64, limit int) ([]int64, error) {
	if limit <= 0 {
		return nil, nil
	}
	q := `SELECT id FROM messages
	       WHERE (embed_gen IS NULL OR embed_gen <> ?)
	         AND ` + LiveMessagesWhere("", true) + `
	         AND id > ?
	       ORDER BY id
	       LIMIT ?`
	rows, err := s.db.QueryContext(ctx, q, target, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("scan for embedding: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan message id: %w", err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate message ids: %w", err)
	}
	return out, nil
}

// SetEmbedGen stamps embed_gen = target on the given message ids,
// marking them covered for that generation. Used by the embed worker
// after a successful upsert (the rows now have embeddings for target) or
// to skip-mark rows that are missing/empty and will never produce an
// embedding. Idempotent: re-stamping an already-stamped row is a no-op.
//
// The ids are processed in chunks (see embedGenStampChunkRows) to stay
// under the driver's bind limit; chunks are not wrapped in a single
// transaction because each chunk's UPDATE is independently idempotent and
// the cross-DB worker contract already tolerates a partial stamp (the
// next scan re-finds any unstamped rows and re-runs an idempotent batch).
func (s *Store) SetEmbedGen(ctx context.Context, ids []int64, target int64) error {
	if len(ids) == 0 {
		return nil
	}
	for start := 0; start < len(ids); start += embedGenStampChunkRows {
		end := min(start+embedGenStampChunkRows, len(ids))
		chunk := ids[start:end]

		placeholders := make([]string, len(chunk))
		args := make([]any, 0, 1+len(chunk))
		args = append(args, target)
		for i, id := range chunk {
			placeholders[i] = "?"
			args = append(args, id)
		}
		q := `UPDATE messages SET embed_gen = ? WHERE id IN (` +
			strings.Join(placeholders, ",") + `)`
		if _, err := s.db.ExecContext(ctx, q, args...); err != nil {
			return fmt.Errorf("set embed_gen: %w", err)
		}
	}
	return nil
}

// CoverageCounts reports embedding coverage for activeGen, computed from
// the MAIN db (messages + embed_gen) so it is a single-DB query on both
// backends and needs no access to the embeddings store.
//
//   - live:     total live messages (the embedding universe).
//   - embedded: live messages stamped embed_gen = activeGen.
//   - missing:  live messages still needing work for activeGen
//     (embed_gen IS NULL OR embed_gen <> activeGen). live = embedded +
//     missing exactly.
//   - skipped:  always 0 here. Distinguishing "stamped but has zero
//     embedding rows" (a missing/empty skip-marker) from "stamped and
//     embedded" requires joining the embeddings table, which lives in a
//     separate DB on SQLite. Per the design this is a best-effort/optional
//     number; we report 0 rather than pay the cross-DB cost. Callers that
//     need a true skipped count can derive it from backend Stats.
//
// activeGen == 0 means "no active/target generation"; then everything
// live is missing and embedded is 0.
func (s *Store) CoverageCounts(ctx context.Context, activeGen int64) (live, embedded, skipped, missing int64, err error) {
	live, err = s.countLiveMessages(ctx)
	if err != nil {
		return 0, 0, 0, 0, err
	}
	if activeGen != 0 {
		q := `SELECT COUNT(*) FROM messages
		       WHERE embed_gen = ? AND ` + LiveMessagesWhere("", true)
		if err := s.db.QueryRowContext(ctx, q, activeGen).Scan(&embedded); err != nil {
			return 0, 0, 0, 0, fmt.Errorf("count embedded: %w", err)
		}
	}
	missing = live - embedded
	if missing < 0 {
		missing = 0
	}
	return live, embedded, 0, missing, nil
}

// countLiveMessages returns the total live-message count. Shared by
// CoverageCounts; kept separate so the live-predicate stays in one place.
func (s *Store) countLiveMessages(ctx context.Context) (int64, error) {
	var n int64
	q := `SELECT COUNT(*) FROM messages WHERE ` + LiveMessagesWhere("", true)
	if err := s.db.QueryRowContext(ctx, q).Scan(&n); err != nil {
		return 0, fmt.Errorf("count live messages: %w", err)
	}
	return n, nil
}
