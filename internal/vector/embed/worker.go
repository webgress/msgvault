package embed

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"go.kenn.io/msgvault/internal/mime"
	"go.kenn.io/msgvault/internal/vector"
)

// EmbeddingClient is the subset of *Client used by Worker; allowing tests
// to inject a fake.
type EmbeddingClient interface {
	Embed(ctx context.Context, inputs []string) ([][]float32, error)
}

// WorkStore is the subset of *store.Store the worker uses to find work
// and stamp coverage against the MAIN db. Defined here (rather than
// importing internal/store) to keep the embed package decoupled and the
// worker easy to fake in tests, mirroring the func-injection style the
// queue/enqueuer used. *store.Store satisfies this implicitly.
type WorkStore interface {
	// ScanForEmbedding returns up to limit live message ids needing work
	// for target (embed_gen IS NULL OR embed_gen <> target), scanning
	// forward from afterID in id order.
	ScanForEmbedding(ctx context.Context, target int64, afterID int64, limit int) ([]int64, error)
	// SetEmbedGen stamps embed_gen=target on ids (idempotent).
	SetEmbedGen(ctx context.Context, ids []int64, target int64) error
}

// WorkerDeps bundles the collaborators a Worker needs. Backend, VectorsDB,
// MainDB, Store, and Client are required; the remaining fields have
// sensible defaults when zero: BatchSize defaults to 32,
// MaxConsecutiveFailures defaults to 5, Log defaults to slog.Default().
type WorkerDeps struct {
	Backend vector.Backend
	// VectorsDB is the generation-side DB handle (vectors.db on SQLite,
	// the shared main DB on PG). Used for embed_runs and the watermark.
	VectorsDB *sql.DB
	// MainDB is the main msgvault.db handle (messages + bodies). Used by
	// embedBatch's body-fetch query.
	MainDB *sql.DB
	// Store finds work and stamps coverage against MainDB. Required.
	Store         WorkStore
	Client        EmbeddingClient
	Preprocess    PreprocessConfig
	MaxInputChars int
	BatchSize     int
	// MaxConsecutiveFailures caps the number of consecutive batch
	// failures (embed error or upsert error) before RunOnce gives up
	// and returns an error. A successful batch resets the counter.
	// Default 5.
	MaxConsecutiveFailures int
	// Rebind translates ?-placeholders to the driver's native form.
	// nil is treated as the identity (used by SQLite); pgvector callers
	// must wire in (&store.PostgreSQLDialect{}).Rebind so the embed_runs,
	// watermark, and body-fetch statements run on pgx.
	Rebind func(string) string
	Log    *slog.Logger
	// TotalPending is the work depth at run start, used by a Progress
	// callback (if any) to report percent done and ETA. Zero disables
	// the denominator — Progress still fires but leaves ETA empty.
	TotalPending int
	// Progress, if non-nil, is called after a batch is durably handled,
	// whether it produced embeddings or was intentionally skip-marked as
	// missing/empty/unembeddable. Done and BatchMsgs count handled
	// messages so they can be compared to TotalPending. Callbacks run on
	// the worker goroutine; rate-limit inside the callback if output is
	// expensive.
	Progress func(ProgressReport)
}

// ProgressReport captures RunOnce progress after a set of messages has
// been handled. Done and BatchMsgs count handled messages; BatchChars
// counts source chars for messages that actually embedded. BatchElapsed
// is end-to-end for that progress unit.
type ProgressReport struct {
	Done         int
	TotalPending int
	BatchMsgs    int
	BatchChars   int
	BatchElapsed time.Duration
	RunElapsed   time.Duration
}

// Worker drives one generation from needs-work messages to persisted
// embeddings via a scan-and-fill loop: it scans the main DB for messages
// whose embed_gen does not match the target generation, embeds them,
// upserts the vectors, then stamps embed_gen so they drop out of the next
// scan. A single Worker is safe for sequential use.
type Worker struct {
	deps WorkerDeps
	wm   *Watermark
	// rebind translates ?-placeholders to the driver's native form for
	// queries the worker issues directly against MainDB (embedBatch's
	// IN-clause). nil is normalized to the identity.
	rebind   func(string) string
	runStart time.Time // valid only during a RunOnce call
}

// NewWorker constructs a Worker, applying defaults for BatchSize (32),
// MaxConsecutiveFailures (5), and Log (slog.Default()).
func NewWorker(d WorkerDeps) *Worker {
	if d.Log == nil {
		d.Log = slog.Default()
	}
	if d.BatchSize == 0 {
		d.BatchSize = 32
	}
	if d.MaxConsecutiveFailures == 0 {
		d.MaxConsecutiveFailures = 5
	}
	rebind := d.Rebind
	if rebind == nil {
		rebind = func(q string) string { return q }
	}
	return &Worker{deps: d, wm: NewWatermark(d.VectorsDB, rebind), rebind: rebind}
}

// RunResult summarizes the outcome of RunOnce.
type RunResult struct {
	Claimed, Succeeded, Failed, Truncated int
}

// msgText is the per-message preprocessed input to the chunker, carried
// from fetch through ChunkText. One msgText fans out to one or more
// inputChunks below. BodyTruncated tracks whether Preprocess hit its
// MaxBodyRunes cap and silently dropped tail content; we propagate
// this onto every chunk's Truncated flag so downstream accounting
// records the message as truncated regardless of which chunk surfaces
// it.
type msgText struct {
	ID            int64
	Text          string
	Chars         int
	BodyTruncated bool
}

// inputChunk is one window into a message's preprocessed text, fed
// 1:1 to the embedding client and turned into a vector.Chunk on the
// way back. The (ID, ChunkIndex) pair is the durable key the backend
// stores. ChunkIndex is dense and 0-based.
type inputChunk struct {
	ID         int64
	ChunkIndex int
	Text       string
	Chars      int
	CharStart  int
	CharEnd    int
	Trunc      bool
}

// ReclaimStale is a no-op retained to satisfy the scheduler's EmbedRunner
// interface. The scan-and-fill design has no claim leases to reclaim: a
// crashed worker leaves messages simply unstamped (embed_gen unchanged),
// and the next scan re-finds them. Always returns (0, nil).
func (w *Worker) ReclaimStale(ctx context.Context) (int, error) { return 0, nil }

// startEmbedRun inserts an embed_runs row and returns the new row's id.
// A failure is non-fatal — run tracking is observability, not correctness.
func (w *Worker) startEmbedRun(ctx context.Context, gen vector.GenerationID, now int64) int64 {
	if w.deps.VectorsDB == nil {
		return 0
	}
	var id int64
	err := w.deps.VectorsDB.QueryRowContext(ctx,
		w.rebind(`INSERT INTO embed_runs (generation_id, started_at) VALUES (?, ?) RETURNING id`),
		int64(gen), now).Scan(&id)
	if err != nil {
		w.deps.Log.Warn("embed_runs: start insert failed", "error", err)
		return 0
	}
	return id
}

// finalizeEmbedRun stamps ended_at plus result counters on the run row
// opened by startEmbedRun. A zero runID means startEmbedRun failed; skip.
func (w *Worker) finalizeEmbedRun(ctx context.Context, runID int64, res RunResult, runErr error, now int64) {
	if runID == 0 || w.deps.VectorsDB == nil {
		return
	}
	var errText *string
	if runErr != nil {
		s := runErr.Error()
		errText = &s
	}
	_, err := w.deps.VectorsDB.ExecContext(ctx,
		w.rebind(`UPDATE embed_runs
		             SET ended_at = ?, claimed = ?, succeeded = ?, failed = ?, truncated = ?, error = ?
		           WHERE id = ?`),
		now, res.Claimed, res.Succeeded, res.Failed, res.Truncated, errText, runID)
	if err != nil {
		w.deps.Log.Warn("embed_runs: finalize update failed", "error", err)
	}
}

// RunOnce scans the given generation for messages needing embedding and
// fills them in, resuming the forward scan from the persisted per-gen
// watermark. It returns when no needs-work messages remain (the scan
// returns empty) or ctx is cancelled.
//
// Cross-DB ordering (SQLite): the find-work scan and the embed_gen stamp
// run against MainDB while the embeddings upsert runs against VectorsDB,
// so they cannot be one transaction. The worker orders the steps —
// embeddings upsert FIRST, then stamp embed_gen — and relies on
// idempotency: the upsert is keyed by (gen, msg, chunk), so a crash
// between the two steps just re-does an idempotent batch on the next scan.
//
// Returns an error when consecutive batch failures reach
// MaxConsecutiveFailures, so a persistently misconfigured embedder (bad
// credentials, unreachable endpoint) surfaces quickly instead of looping
// forever. A successful batch resets the failure counter.
func (w *Worker) RunOnce(ctx context.Context, gen vector.GenerationID) (res RunResult, retErr error) {
	return w.run(ctx, gen, false)
}

// RunBackstop performs a full-scan pass that ignores the per-gen
// watermark, driving coverage to zero even for sub-watermark stragglers
// (a message that was unstamped but already swept past by the optimistic
// watermark — e.g. dropped during a transient failure, or a legacy row
// whose id sits below where a prior run advanced). It reuses the same
// scan/embed/stamp path with the scan cursor pinned at 0. Idempotent:
// already-covered rows are skipped by the scan predicate, so re-running it
// is cheap once the corpus is embedded.
func (w *Worker) RunBackstop(ctx context.Context, gen vector.GenerationID) (res RunResult, retErr error) {
	return w.run(ctx, gen, true)
}

func (w *Worker) run(ctx context.Context, gen vector.GenerationID, backstop bool) (res RunResult, retErr error) {
	consecutiveFailures := 0
	var lastErr error
	completedRows := 0
	w.runStart = time.Now()
	runID := w.startEmbedRun(ctx, gen, w.runStart.Unix())
	defer func() {
		// Finalize on a context detached from the caller's cancellation so
		// the embed_runs row is stamped even when RunOnce exits because ctx
		// was cancelled. Running the close-out UPDATE on the cancelled ctx
		// would short-circuit in database/sql and leave the row open
		// forever. A short timeout keeps shutdown from hanging on a wedged DB.
		fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		w.finalizeEmbedRun(fctx, runID, res, retErr, time.Now().Unix())
	}()

	// Seed the forward-scan cursor. The backstop ignores the watermark and
	// scans from the beginning so it catches sub-watermark stragglers.
	var afterID int64
	if !backstop {
		wm, err := w.wm.GetWatermark(ctx, gen)
		if err != nil {
			// Non-fatal: a missing/unreadable watermark just restarts the
			// scan from 0 (the scan predicate + idempotent upsert make this
			// harmless). Log and continue.
			w.deps.Log.Warn("embed: read watermark failed; scanning from start",
				"gen", gen, "error", err)
		} else {
			afterID = wm
		}
	}

	for {
		if err := ctx.Err(); err != nil {
			return res, fmt.Errorf("RunOnce: %w", err)
		}
		batchStart := time.Now()
		ids, err := w.deps.Store.ScanForEmbedding(ctx, int64(gen), afterID, w.deps.BatchSize)
		if err != nil {
			return res, fmt.Errorf("scan for embedding: %w", err)
		}
		if len(ids) == 0 {
			return res, nil
		}
		res.Claimed += len(ids)
		// batchMax is the highest id in this scan slice; once the batch is
		// stamped these rows drop out of the predicate, but advancing the
		// cursor past them avoids re-scanning the covered prefix.
		batchMax := ids[len(ids)-1]

		eb, err := w.embedBatch(ctx, ids)
		if err != nil {
			consecutiveFailures++
			lastErr = err
			w.deps.Log.Warn("embed batch failed", "gen", gen, "ids", len(ids), "error", err)

			if errors.Is(err, ErrPermanent4xx) {
				// Walk the scanned ids one at a time. Drain decides per-ID
				// whether to stamp (it embedded, or is a confirmed
				// message-specific 4xx given some sibling embedded) or leave
				// unstamped (endpoint-wide failure can't be ruled out).
				w.deps.Log.Info("embed: downshifting to BatchSize=1 to drain failing batch",
					"gen", gen, "batch_size", len(ids))
				embedded, stamped, safeAdvanceID, drainErr := w.downshiftDrain(ctx, gen, ids, &res, &completedRows)
				res.Succeeded += embedded
				if drainErr != nil {
					w.deps.Log.Info("embed: downshift drain returned error",
						"gen", gen, "batch_size", len(ids),
						"embedded", embedded, "stamped", stamped, "error", drainErr)
				} else {
					w.deps.Log.Info("embed: downshift drain complete; resuming configured batch size",
						"gen", gen, "batch_size", len(ids),
						"embedded", embedded, "stamped", stamped)
				}

				// Forward progress resets the cap and advances the cursor —
				// but ONLY past rows that are actually stamped. On a clean
				// drain every id is resolved (embedded, skip-marked, or a
				// message-specific 4xx drop) so safeAdvanceID == batchMax and
				// behavior is unchanged. On a NON-4xx (transient) drain error
				// an EARLIER singleton may have stamped while a LATER one was
				// left unstamped; safeAdvanceID is the highest CONTIGUOUSLY
				// stamped id before the failure, so the watermark never jumps
				// past the unstamped straggler and the next RunOnce re-finds
				// and retries it (idempotent). Without this, the watermark
				// would advance to batchMax and the straggler would be
				// stranded — the only recovery is the MANUAL-only backstop.
				if embedded > 0 {
					consecutiveFailures = 0
				}
				if safeAdvanceID > afterID {
					afterID = safeAdvanceID
					w.advanceWatermark(ctx, gen, safeAdvanceID, backstop)
				}

				if drainErr != nil {
					lastErr = drainErr
					// A retired generation is a benign drop, never a hard abort.
					if errors.Is(drainErr, vector.ErrGenerationRetired) {
						return res, nil
					}
					if !errors.Is(drainErr, ErrPermanent4xx) {
						return res, fmt.Errorf("downshift drain: %w", drainErr)
					}
					if consecutiveFailures >= w.deps.MaxConsecutiveFailures {
						return res, fmt.Errorf("embed worker aborting after %d consecutive failures: %w",
							consecutiveFailures, lastErr)
					}
					// Every singleton 4xx'd and nothing embedded: the rows are
					// left unstamped (so a misconfigured endpoint does not
					// silently lose work) and the cursor does NOT advance, so
					// the next scan re-finds them and the failure cap trips.
					// Avoid busy-spinning: continue lets the loop re-scan;
					// consecutiveFailures will reach the cap.
					continue
				}
				continue
			}

			// Non-4xx error: leave the batch unstamped (next scan re-finds
			// it) and do not advance the cursor, so the failure cap can
			// short-circuit the loop on a persistent fault.
			res.Failed += len(ids)
			if consecutiveFailures >= w.deps.MaxConsecutiveFailures {
				return res, fmt.Errorf("embed worker aborting after %d consecutive failures: %w",
					consecutiveFailures, lastErr)
			}
			continue
		}
		res.Truncated += eb.truncated

		// Skip-mark messages that produced no embeddable content (missing
		// from the main DB, or empty after preprocess). Stamping embed_gen
		// IS the skip-marker — it drops them out of the next scan, the
		// scan-and-fill replacement for deleting a queue row.
		skipIDs := append(append([]int64(nil), eb.missing...), eb.empty...)

		if len(eb.chunks) == 0 {
			// Nothing to embed. Stamp the skip set so the scan advances.
			if len(skipIDs) > 0 {
				if len(eb.missing) > 0 {
					w.deps.Log.Warn("messages missing from main DB", "gen", gen, "ids", eb.missing)
				}
				if len(eb.empty) > 0 {
					w.deps.Log.Warn("messages empty after preprocess", "gen", gen, "ids", eb.empty)
				}
				if serr := w.deps.Store.SetEmbedGen(ctx, skipIDs, int64(gen)); serr != nil {
					res.Failed += len(skipIDs)
					w.deps.Log.Error("stamp skip set failed", "error", serr, "gen", gen, "ids", len(skipIDs))
					consecutiveFailures++
					lastErr = serr
					if consecutiveFailures >= w.deps.MaxConsecutiveFailures {
						return res, fmt.Errorf("embed worker aborting after %d consecutive failures: %w",
							consecutiveFailures, lastErr)
					}
					continue
				}
				completedRows += len(skipIDs)
				w.reportProgress(completedRows, len(skipIDs), 0, time.Since(batchStart))
			}
			consecutiveFailures = 0
			afterID = batchMax
			w.advanceWatermark(ctx, gen, batchMax, backstop)
			continue
		}

		// Step 1: upsert embeddings (VectorsDB side).
		if err := w.deps.Backend.Upsert(ctx, gen, eb.chunks); err != nil {
			if errors.Is(err, vector.ErrGenerationRetired) {
				// The generation was retired out from under this worker. Per
				// the ErrGenerationRetired contract this is a benign "stop"
				// signal, not a hard failure: re-embedding would re-fail
				// identically. Do NOT stamp embed_gen (the retired gen is
				// going away) and end the run cleanly.
				w.deps.Log.Info("embed: generation retired mid-run; stopping", "gen", gen)
				return res, nil
			}
			res.Failed += len(eb.embeddedIDs)
			w.deps.Log.Error("upsert failed", "gen", gen, "ids", len(eb.embeddedIDs), "error", err)
			consecutiveFailures++
			lastErr = err
			if consecutiveFailures >= w.deps.MaxConsecutiveFailures {
				return res, fmt.Errorf("embed worker aborting after %d consecutive failures: %w",
					consecutiveFailures, lastErr)
			}
			continue
		}

		// Step 2: stamp embed_gen for the embedded + skip sets (MainDB
		// side). Safe to do after the upsert: the upsert is idempotent, so a
		// crash before this stamp just re-does the batch next scan. Stamp
		// the union so embedded and skip-marked rows both drop out together.
		stampIDs := append(append([]int64(nil), eb.embeddedIDs...), skipIDs...)
		if serr := w.deps.Store.SetEmbedGen(ctx, stampIDs, int64(gen)); serr != nil {
			res.Failed += len(eb.embeddedIDs)
			w.deps.Log.Error("stamp embed_gen failed", "gen", gen, "ids", len(stampIDs), "error", serr)
			consecutiveFailures++
			lastErr = serr
			if consecutiveFailures >= w.deps.MaxConsecutiveFailures {
				return res, fmt.Errorf("embed worker aborting after %d consecutive failures: %w",
					consecutiveFailures, lastErr)
			}
			// Do not advance the cursor: next scan re-finds the unstamped rows
			// (the upsert already ran, so re-embedding is idempotent).
			continue
		}

		if len(eb.missing) > 0 {
			w.deps.Log.Warn("messages missing from main DB", "gen", gen, "ids", eb.missing)
		}
		if len(eb.empty) > 0 {
			w.deps.Log.Warn("messages empty after preprocess", "gen", gen, "ids", eb.empty)
		}

		res.Succeeded += len(eb.embeddedIDs)
		consecutiveFailures = 0
		afterID = batchMax
		w.advanceWatermark(ctx, gen, batchMax, backstop)

		batchProcessed := len(eb.embeddedIDs) + len(skipIDs)
		completedRows += batchProcessed
		batchChars := 0
		for _, c := range eb.chunks {
			batchChars += c.SourceCharLen
		}
		w.reportProgress(completedRows, batchProcessed, batchChars, time.Since(batchStart))
	}
}

// advanceWatermark persists the per-gen forward-scan cursor to id after a
// batch made forward progress. The backstop never persists (it scans from
// 0 by design and must not push the optimistic watermark backward or
// forward). Failure is non-critical — the watermark is a pure
// optimization — so it is logged, not returned.
func (w *Worker) advanceWatermark(ctx context.Context, gen vector.GenerationID, id int64, backstop bool) {
	if backstop {
		return
	}
	if err := w.wm.SetWatermark(ctx, gen, id); err != nil {
		w.deps.Log.Warn("embed: advance watermark failed (non-critical)",
			"gen", gen, "id", id, "error", err)
	}
}

// embedBatchResult carries the output of embedBatch. chunks and
// embeddedIDs are aligned by position and correspond to messages that
// were actually fetched and embedded. missing lists ids from the
// input that had no row in the messages table; empty lists ids whose
// content preprocessed to empty and therefore should not be sent to
// embedders that reject blank strings.
type embedBatchResult struct {
	chunks      []vector.Chunk
	embeddedIDs []int64
	missing     []int64
	empty       []int64
	truncated   int
}

// embedBatch fetches subject/body for ids, preprocesses each, calls the
// embedding client, and assembles the resulting chunks. Messages that
// vanished between scan and fetch (e.g. the sync deleted them) are
// reported in the returned result's missing slice rather than causing
// a failure — the caller skip-marks them.
func (w *Worker) embedBatch(ctx context.Context, ids []int64) (embedBatchResult, error) {
	placeholders := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		placeholders[i] = "?"
		args[i] = id
	}
	query := w.rebind(fmt.Sprintf(`
        SELECT m.id, COALESCE(m.subject, ''), COALESCE(mb.body_text, ''), COALESCE(mb.body_html, '')
          FROM messages m
          LEFT JOIN message_bodies mb ON mb.message_id = m.id
         WHERE m.id IN (%s)`, strings.Join(placeholders, ",")))

	rows, err := w.deps.MainDB.QueryContext(ctx, query, args...)
	if err != nil {
		return embedBatchResult{}, fmt.Errorf("fetch bodies: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var msgs []msgText
	var empty []int64
	fetched := make(map[int64]struct{}, len(ids))
	for rows.Next() {
		var id int64
		var subject, bodyText, bodyHTML string
		if err := rows.Scan(&id, &subject, &bodyText, &bodyHTML); err != nil {
			return embedBatchResult{}, fmt.Errorf("scan message row: %w", err)
		}
		// Fall back to HTML-to-text when the plaintext body is empty —
		// HTML-only messages would otherwise get subject-only embeddings
		// and have materially worse semantic recall.
		body := bodyText
		if body == "" && bodyHTML != "" {
			body = mime.StripHTML(bodyHTML)
		}
		// Sized to give Preprocess a generous-but-bounded budget: the
		// chunker emits at most maxSpansPerMessage * MaxInputChars
		// runes of *post-sanitize* output, and sanitize routinely
		// strips 10x of HTML/base64 noise from polluted bodies; the
		// rawBodyMultiplier covers the worst case. Preprocess applies
		// this cap *between* its cheap pollution-removal pass (CRLF
		// normalize + base64 strip) and the heavier regex transforms,
		// so a body whose first MB is an inline base64 image still
		// gets its prose tail through the cap.
		preprocessCfg := w.deps.Preprocess
		if preprocessCfg.MaxBodyRunes == 0 && w.deps.MaxInputChars > 0 {
			preprocessCfg.MaxBodyRunes = w.deps.MaxInputChars * maxSpansPerMessage * rawBodyMultiplier
		}
		// Pass maxChars=0 so Preprocess does NOT truncate the final
		// output by character count. Chunking (below) takes the full
		// preprocessed text and divides it into windows of at most
		// MaxInputChars runes each, so output truncation would just
		// throw away tail content that ChunkText would otherwise
		// embed in a later chunk.
		txt, bodyTrunc := Preprocess(subject, body, 0, preprocessCfg)
		fetched[id] = struct{}{}
		if strings.TrimSpace(txt) == "" {
			empty = append(empty, id)
			continue
		}
		msgs = append(msgs, msgText{ID: id, Text: txt, Chars: utf8.RuneCountInString(txt), BodyTruncated: bodyTrunc})
	}
	if err := rows.Err(); err != nil {
		return embedBatchResult{}, fmt.Errorf("iterate message rows: %w", err)
	}

	// Identify scanned ids that had no row in messages; we'll report
	// them back so the caller can skip-mark them.
	var missing []int64
	for _, id := range ids {
		if _, ok := fetched[id]; !ok {
			missing = append(missing, id)
		}
	}

	if len(msgs) == 0 {
		// All scanned ids are missing/empty — return an empty result (no
		// chunks, no error). Caller skip-marks them.
		return embedBatchResult{missing: missing, empty: empty}, nil
	}

	// Chunk every message into windows of at most MaxInputChars runes.
	// Short messages produce exactly one chunk; long ones produce N.
	// Each chunk becomes one input to the embedder. The per-message
	// span cap protects the batch from pathological inputs (10+ MB
	// system error dumps, base64 blobs that survived sanitize): one
	// such message could otherwise produce thousands of chunks and
	// blow the batch past the embedder's request-time budget.
	chunkWindow := w.deps.MaxInputChars
	overlap := chunkOverlapFor(chunkWindow)
	maxSpans := maxSpansPerMessage
	var pieces []inputChunk
	var inputs []string
	for _, m := range msgs {
		spans, chunkTail := ChunkText(m.Text, chunkWindow, overlap, maxSpans)
		// A message is "truncated" if any of its content was dropped:
		//   - body hit Preprocess's MaxBodyRunes cap, OR
		//   - ChunkText dropped tail past maxSpans (regardless of
		//     whether the last emitted chunk happened to land on a
		//     soft break, in which case the per-chunk hard-cut flag
		//     wouldn't fire).
		msgTrunc := m.BodyTruncated || chunkTail
		for j, sp := range spans {
			ic := inputChunk{
				ID:         m.ID,
				ChunkIndex: j,
				Text:       sp.Text,
				Chars:      sp.CharEnd - sp.CharStart,
				CharStart:  sp.CharStart,
				CharEnd:    sp.CharEnd,
				// Trunc flags either: a hard-cut chunk where a
				// sentence may have been split across the boundary
				// (overlap exists to recover from this), or any
				// chunk of a message that was truncated upstream.
				Trunc: msgTrunc ||
					(chunkWindow > 0 && (sp.CharEnd-sp.CharStart) == chunkWindow && j < len(spans)-1),
			}
			pieces = append(pieces, ic)
			inputs = append(inputs, sp.Text)
		}
	}

	// Split chunk inputs into sub-batches of at most BatchSize so a
	// long-form message that fans out to many chunks doesn't push a
	// single embed call past the provider's per-request limit (Ollama
	// stops responding around 250 inputs; OpenAI caps at 2048; either
	// way, payload size + request-timeout grow with the input count).
	// A message completes only after every one of its chunks has been
	// embedded and upserted in this same call, so partial-failure
	// semantics are unchanged.
	embedSubBatchSize := w.deps.BatchSize
	if embedSubBatchSize <= 0 {
		embedSubBatchSize = len(inputs)
	}
	start := time.Now()
	vecs := make([][]float32, 0, len(inputs))
	for i := 0; i < len(inputs); i += embedSubBatchSize {
		end := min(i+embedSubBatchSize, len(inputs))
		got, err := w.deps.Client.Embed(ctx, inputs[i:end])
		if err != nil {
			return embedBatchResult{}, fmt.Errorf("embed: %w", err)
		}
		if len(got) != end-i {
			return embedBatchResult{}, fmt.Errorf(
				"embedder returned %d vectors for %d inputs in sub-batch [%d:%d)",
				len(got), end-i, i, end)
		}
		vecs = append(vecs, got...)
	}
	w.deps.Log.Debug("embed batch",
		"messages", len(msgs), "chunks", len(pieces),
		"chars", totalPieceChars(pieces),
		"sub_batches", (len(inputs)+embedSubBatchSize-1)/embedSubBatchSize,
		"duration_ms", time.Since(start).Milliseconds())

	if len(vecs) != len(pieces) {
		return embedBatchResult{}, fmt.Errorf("embedder returned %d vectors for %d chunk inputs", len(vecs), len(pieces))
	}

	truncated := 0
	chunks := make([]vector.Chunk, 0, len(vecs))
	embeddedIDs := make([]int64, 0, len(msgs))
	seenMsg := make(map[int64]struct{}, len(msgs))
	truncatedMsg := make(map[int64]struct{}, len(msgs))
	for i, p := range pieces {
		chunks = append(chunks, vector.Chunk{
			MessageID:      p.ID,
			ChunkIndex:     p.ChunkIndex,
			Vector:         vecs[i],
			SourceCharLen:  p.Chars,
			ChunkCharStart: p.CharStart,
			ChunkCharEnd:   p.CharEnd,
			Truncated:      p.Trunc,
		})
		if _, ok := seenMsg[p.ID]; !ok {
			seenMsg[p.ID] = struct{}{}
			embeddedIDs = append(embeddedIDs, p.ID)
		}
		// Count each truncated message once, not once per truncated
		// chunk. truncated feeds RunResult.Truncated which the caller
		// compares against Succeeded (a per-message count): keeping
		// both metrics in the same units lets "what fraction was
		// truncated" actually be a fraction.
		if p.Trunc {
			if _, seen := truncatedMsg[p.ID]; !seen {
				truncatedMsg[p.ID] = struct{}{}
				truncated++
			}
		}
	}
	return embedBatchResult{
		chunks:      chunks,
		embeddedIDs: embeddedIDs,
		missing:     missing,
		empty:       empty,
		truncated:   truncated,
	}, nil
}

// downshiftDrain handles a non-retryable 4xx on a scanned batch by walking
// the same ids one at a time. Singletons that embed are upserted and
// stamped immediately; singletons that 4xx are deferred and the drop
// decision is made at end-of-drain based on whether anything embedded
// (message-specific 4xx → stamp-drop; endpoint-wide failure → leave
// unstamped so a misconfigured endpoint does not silently lose work).
//
// Returns:
//   - embedded: count of singletons that successfully embedded.
//   - stamped:  count of ids whose embed_gen was stamped (embedded +
//     confirmed message-specific drops). When this is > 0 the caller may
//     advance the scan cursor; when it is 0 the deferred ids are left
//     unstamped and the cursor must not advance.
//   - safeAdvanceID: the highest scanned id the caller may advance the
//     watermark past WITHOUT stranding an unstamped row. On a clean drain
//     every id is resolved (stamped or message-specific drop) so this is
//     the batch's max id. On a NON-4xx error return it is the highest
//     CONTIGUOUSLY-stamped id reached before the failure — so the watermark
//     does not jump past a later unstamped straggler that a transient fault
//     left behind. The next RunOnce re-finds the straggler (id >
//     safeAdvanceID) and retries it idempotently; the failure cap still
//     bounds repeated transient failures. (On the all-drop 4xx return
//     nothing is stamped so this stays 0.)
//   - err: nil on a clean drain; ErrPermanent4xx (wrapped) when every
//     singleton 4xx'd with no embeds (deferred ids left unstamped);
//     ErrGenerationRetired (wrapped) when the generation was retired
//     mid-drain (benign); or any other error (transient-after-retries,
//     upsert, stamp) that should fail the run.
func (w *Worker) downshiftDrain(
	ctx context.Context,
	gen vector.GenerationID,
	ids []int64,
	res *RunResult,
	completedRows *int,
) (embedded int, stamped int, safeAdvanceID int64, err error) {
	var deferredDrops []int64
	var lastDeferredErr error
	// contiguousStampedID tracks the highest id with an unbroken
	// stamped-from-the-start prefix. The first time an id is left unresolved
	// (a deferred 4xx, or a non-4xx error return) brokeContiguity latches
	// and we stop advancing it — everything from that id on is unsafe to
	// skip past.
	var contiguousStampedID int64
	brokeContiguity := false

	for _, id := range ids {
		select {
		case <-ctx.Done():
			return embedded, stamped, contiguousStampedID, ctx.Err()
		default:
		}

		batchStart := time.Now()
		eb, e := w.embedBatch(ctx, []int64{id})
		if e != nil {
			if errors.Is(e, ErrPermanent4xx) {
				// Defer the drop decision. See function-level comment. A
				// deferred id breaks the contiguous-stamped prefix: even if it
				// is stamped at end-of-drain, the watermark must not skip past
				// it on an error return.
				deferredDrops = append(deferredDrops, id)
				lastDeferredErr = e
				brokeContiguity = true
				continue
			}
			// Non-4xx (transient) error: this id is left UNSTAMPED. Return the
			// contiguous-stamped id so the caller does not advance the
			// watermark past it; the next RunOnce re-finds it.
			return embedded, stamped, contiguousStampedID, e
		}
		if len(eb.chunks) == 0 {
			// Missing/empty singleton — skip-mark it.
			skip := append(append([]int64(nil), eb.missing...), eb.empty...)
			if len(skip) > 0 {
				if serr := w.deps.Store.SetEmbedGen(ctx, skip, int64(gen)); serr != nil {
					res.Failed += len(skip)
					return embedded, stamped, contiguousStampedID, fmt.Errorf("stamp skip: %w", serr)
				}
				stamped += len(skip)
				*completedRows += len(skip)
				w.reportProgress(*completedRows, len(skip), 0, time.Since(batchStart))
			}
			if !brokeContiguity {
				contiguousStampedID = id
			}
			continue
		}
		if uerr := w.deps.Backend.Upsert(ctx, gen, eb.chunks); uerr != nil {
			if errors.Is(uerr, vector.ErrGenerationRetired) {
				// Generation retired mid-drain. Stop draining and surface the
				// benign sentinel; remaining singletons would observe the same
				// state. Do not stamp (the gen is going away).
				w.deps.Log.Info("embed: generation retired mid-drain; stopping", "gen", gen, "id", id)
				return embedded, stamped, contiguousStampedID, fmt.Errorf("upsert: %w", uerr)
			}
			return embedded, stamped, contiguousStampedID, fmt.Errorf("upsert: %w", uerr)
		}
		if serr := w.deps.Store.SetEmbedGen(ctx, eb.embeddedIDs, int64(gen)); serr != nil {
			return embedded, stamped, contiguousStampedID, fmt.Errorf("stamp embed_gen: %w", serr)
		}
		res.Truncated += eb.truncated
		embedded += len(eb.embeddedIDs)
		stamped += len(eb.embeddedIDs)
		*completedRows += len(eb.embeddedIDs)
		if !brokeContiguity {
			contiguousStampedID = id
		}
		batchChars := 0
		for _, c := range eb.chunks {
			batchChars += c.SourceCharLen
		}
		w.reportProgress(*completedRows, len(eb.embeddedIDs), batchChars, time.Since(batchStart))
	}

	// Drain finished cleanly: every id is resolved (stamped, skip-marked, or
	// a deferred 4xx about to be stamped below), so the whole scanned batch
	// is safe to advance past.
	safeAdvanceID = ids[len(ids)-1]

	// Decide deferred-drop fate.
	if len(deferredDrops) == 0 {
		return embedded, stamped, safeAdvanceID, nil
	}
	if embedded > 0 {
		// Endpoint works for some messages, so the 4xxs are
		// message-specific (oversize input, malformed input, etc.).
		// Stamp them so they drop out of future scans.
		for _, id := range deferredDrops {
			w.deps.Log.Warn("stamping (dropping) message after singleton 4xx",
				"gen", gen, "id", id, "error", lastDeferredErr)
		}
		dropStart := time.Now()
		if serr := w.deps.Store.SetEmbedGen(ctx, deferredDrops, int64(gen)); serr != nil {
			res.Failed += len(deferredDrops)
			// Some deferred drops are now unstamped; do not advance past the
			// contiguous-stamped prefix.
			return embedded, stamped, contiguousStampedID, fmt.Errorf("stamp drop: %w", serr)
		}
		stamped += len(deferredDrops)
		*completedRows += len(deferredDrops)
		w.reportProgress(*completedRows, len(deferredDrops), 0, time.Since(dropStart))
		// All deferred drops are now stamped; the entire batch is resolved.
		return embedded, stamped, safeAdvanceID, nil
	}
	// embedded == 0. We can't distinguish endpoint-wide failure from a
	// batch where every message just happened to be unembeddable. Leave
	// the deferred ids UNSTAMPED so a misconfigured endpoint does not
	// silently drop work, and return the wrapped 4xx so the caller
	// surfaces it. The unstamped ids are re-found on the next scan; if the
	// underlying problem persists, the consecutive-failure cap eventually
	// trips with the same 4xx body. Advance only past the contiguous
	// stamped prefix (the leading missing/empty skips, if any) so the
	// unstamped deferred ids are not skipped.
	return embedded, stamped, contiguousStampedID, fmt.Errorf("downshift all-drop: every singleton returned non-retryable 4xx (left %d row(s) unstamped): %w",
		len(deferredDrops), lastDeferredErr)
}

func (w *Worker) reportProgress(done, batchMsgs, batchChars int, batchElapsed time.Duration) {
	if w.deps.Progress == nil {
		return
	}
	w.deps.Progress(ProgressReport{
		Done:         done,
		TotalPending: w.deps.TotalPending,
		BatchMsgs:    batchMsgs,
		BatchChars:   batchChars,
		BatchElapsed: batchElapsed,
		RunElapsed:   time.Since(w.runStart),
	})
}

// totalPieceChars sums the rune counts of every chunk in the batch, for
// debug logging — distinct from totalChars because a long message
// contributes one msgText row but several inputChunk rows.
func totalPieceChars(pieces []inputChunk) int {
	n := 0
	for _, p := range pieces {
		n += p.Chars
	}
	return n
}

// rawBodyMultiplier is how much pre-sanitize body the worker is
// willing to feed into Preprocess relative to what the chunker can
// ultimately emit. The chunker keeps at most maxSpansPerMessage *
// MaxInputChars runes of *post-sanitize* output; sanitize strips a
// large but not unbounded fraction of HTML/base64/URL noise — 10x
// is the empirical ceiling for HTML-heavy newsletters with inline
// images. 16x gives comfortable headroom for the long tail without
// letting a pathological 100 MB body burn CPU on regex passes that
// would never produce additional retrievable content.
const rawBodyMultiplier = 16

// maxSpansPerMessage caps the number of chunks emitted for any single
// message. Picked empirically: typical email is under 5 chunks at a
// 4 KB window; long-form prose (50–100 KB) tops out at 20–25; the cap
// at 64 covers every legitimate case but stops 10+ MB system error
// dumps and stack-trace forwards from generating thousands of chunks
// that would push a single embed call past the API timeout. Set
// statically rather than via config because the failure mode it
// addresses is universal across embedding backends, not a tuning
// knob users are expected to touch.
const maxSpansPerMessage = 64

// chunkOverlapFor returns the rune count of overlap between consecutive
// chunks. The overlap exists so a sentence or phrase that straddles a
// window boundary survives in at least one chunk verbatim — without it,
// a query term that lives on a cut would be invisible to ANN search.
//
// A fixed-fraction overlap (≈3% of the window, floored at 0 for small
// windows) gives roughly one sentence of margin at typical chunk sizes
// without materially padding the corpus. Hardcoded here rather than
// pulled from config because the overlap is an implementation detail
// of how chunks recover boundary content — making it tunable would let
// users degrade recall in exchange for marginal storage savings, and
// nobody has asked for that knob yet.
func chunkOverlapFor(maxRunes int) int {
	if maxRunes <= 0 {
		return 0
	}
	if maxRunes < 200 {
		// For very small windows the proportional overlap (~3%) is
		// under a couple of dozen runes — not enough to recover a
		// sentence — so fall back to "no overlap" rather than pretend.
		return 0
	}
	return maxRunes / 30
}
