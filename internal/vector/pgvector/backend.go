//go:build pgvector

// Package pgvector implements vector.Backend using the pgvector
// PostgreSQL extension, co-located with the main pgx-backed connection.
// Embeddings are stored in the same database as messages — there is no
// separate vectors.db. Build with `-tags pgvector` to enable.
package pgvector

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/vector"
)

// Compile-time check that *Backend satisfies vector.Backend.
var _ vector.Backend = (*Backend)(nil)

// Options configures Open. The same *sql.DB handle backs both the
// embedding schema and the main msgvault schema; pgvector embeddings
// live in the same Postgres database.
type Options struct {
	// DB is the pgx-backed handle to the database that contains both
	// the msgvault main schema and the pgvector embedding tables.
	DB *sql.DB
	// Dimension is the default dimension used to eagerly create the
	// per-dimension HNSW index on first migration. Optional; if zero
	// the index is created on first CreateGeneration.
	Dimension int
}

// Backend implements vector.Backend against a PostgreSQL database
// with the pgvector extension. The same *sql.DB also serves the main
// msgvault schema (messages, message_recipients, message_labels).
type Backend struct {
	db  *sql.DB
	dim int
}

// Open verifies the database is reachable, applies the embedding schema
// (creating the vector extension if necessary), and returns a Backend.
// The DB handle is shared with the main msgvault store; callers retain
// ownership and Close() is a no-op for the handle itself.
func Open(ctx context.Context, opts Options) (*Backend, error) {
	if opts.DB == nil {
		return nil, fmt.Errorf("pgvector.Open: Options.DB is required")
	}
	if err := Migrate(ctx, opts.DB, opts.Dimension); err != nil {
		return nil, fmt.Errorf("pgvector migrate: %w", err)
	}
	return &Backend{db: opts.DB, dim: opts.Dimension}, nil
}

// Close is a no-op for the pgvector backend: the *sql.DB handle is
// owned by the main store and closed there. Provided to satisfy the
// vector.Backend interface.
func (b *Backend) Close() error { return nil }

// DB returns the underlying *sql.DB. Exposed to mirror the sqlitevec
// surface; callers that need the shared pool (e.g. the embed worker)
// can retrieve it here instead of carrying the main handle separately.
func (b *Backend) DB() *sql.DB { return b.db }

// CreateGeneration allocates a new building generation and seeds
// pending_embeddings with every currently-embeddable message in
// messages. Mirrors the sqlitevec semantics (§5.1): if a building row
// with the same fingerprint already exists, returns its id so a crashed
// rebuild can resume; a mismatched fingerprint surfaces
// vector.ErrBuildingInProgress.
func (b *Backend) CreateGeneration(ctx context.Context, model string, dim int, fingerprint string) (vector.GenerationID, error) {
	if err := EnsureVectorIndex(ctx, b.db, dim); err != nil {
		return 0, err
	}
	fp := fingerprint
	if fp == "" {
		fp = fmt.Sprintf("%s:%d", model, dim)
	}
	now := time.Now().Unix()

	gen, isNew, err := b.claimOrInsertBuilding(ctx, model, dim, fp, now)
	if err != nil {
		return 0, err
	}

	if !isNew {
		seeded, err := b.isGenerationSeeded(ctx, gen)
		if err != nil {
			return 0, err
		}
		if seeded {
			return gen, nil
		}
	}
	if err := b.seedPending(ctx, gen, now); err != nil {
		return 0, err
	}
	if err := b.markGenerationSeeded(ctx, gen, now); err != nil {
		return 0, err
	}
	return gen, nil
}

func (b *Backend) isGenerationSeeded(ctx context.Context, gen vector.GenerationID) (bool, error) {
	var seededAt sql.NullInt64
	err := b.db.QueryRowContext(ctx,
		`SELECT seeded_at FROM index_generations WHERE id = $1`, int64(gen)).Scan(&seededAt)
	if err != nil {
		return false, fmt.Errorf("read seeded_at: %w", err)
	}
	return seededAt.Valid, nil
}

func (b *Backend) markGenerationSeeded(ctx context.Context, gen vector.GenerationID, now int64) error {
	if _, err := b.db.ExecContext(ctx,
		`UPDATE index_generations SET seeded_at = COALESCE(seeded_at, $1) WHERE id = $2`,
		now, int64(gen)); err != nil {
		return fmt.Errorf("mark generation seeded: %w", err)
	}
	return nil
}

// EnsureSeeded mirrors sqlitevec.EnsureSeeded: re-runs the initial seed
// pass when seeded_at is NULL so an interrupted resume cannot activate
// an empty generation.
func (b *Backend) EnsureSeeded(ctx context.Context, gen vector.GenerationID) error {
	var state string
	err := b.db.QueryRowContext(ctx,
		`SELECT state FROM index_generations WHERE id = $1`, int64(gen)).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: %d", vector.ErrUnknownGeneration, gen)
	}
	if err != nil {
		return fmt.Errorf("lookup generation %d: %w", gen, err)
	}
	if state != string(vector.GenerationBuilding) {
		return fmt.Errorf("%w: generation %d state=%q", vector.ErrGenerationNotBuilding, gen, state)
	}
	seeded, err := b.isGenerationSeeded(ctx, gen)
	if err != nil {
		return err
	}
	if seeded {
		return nil
	}
	now := time.Now().Unix()
	if err := b.seedPending(ctx, gen, now); err != nil {
		return err
	}
	return b.markGenerationSeeded(ctx, gen, now)
}

// claimOrInsertBuilding returns (id, isNew, err). See sqlitevec for
// rationale — same race-recovery shape, translated to pgx error codes.
func (b *Backend) claimOrInsertBuilding(ctx context.Context, model string, dim int, fp string, now int64) (vector.GenerationID, bool, error) {
	if id, existingFP, ok, err := b.lookupBuilding(ctx); err != nil {
		return 0, false, err
	} else if ok {
		if existingFP != fp {
			return 0, false, fmt.Errorf("%w: existing building fingerprint=%q, requested=%q — activate or retire it before starting a new rebuild",
				vector.ErrBuildingInProgress, existingFP, fp)
		}
		return id, false, nil
	}

	var newID int64
	err := b.db.QueryRowContext(ctx,
		`INSERT INTO index_generations
		 (model, dimension, fingerprint, started_at, state)
		 VALUES ($1, $2, $3, $4, 'building')
		 RETURNING id`,
		model, dim, fp, now).Scan(&newID)
	if err != nil {
		if isUniqueViolation(err) {
			id, existingFP, ok, lookupErr := b.lookupBuilding(ctx)
			if lookupErr != nil {
				return 0, false, fmt.Errorf("lookup after insert race: %w", lookupErr)
			}
			if !ok {
				return 0, false, fmt.Errorf("insert generation: %w", err)
			}
			if existingFP != fp {
				return 0, false, fmt.Errorf("%w: existing building fingerprint=%q, requested=%q — activate or retire it before starting a new rebuild",
					vector.ErrBuildingInProgress, existingFP, fp)
			}
			return id, false, nil
		}
		return 0, false, fmt.Errorf("insert generation: %w", err)
	}
	return vector.GenerationID(newID), true, nil
}

func (b *Backend) lookupBuilding(ctx context.Context) (vector.GenerationID, string, bool, error) {
	var (
		id int64
		fp string
	)
	err := b.db.QueryRowContext(ctx,
		`SELECT id, fingerprint FROM index_generations WHERE state = 'building'`).
		Scan(&id, &fp)
	switch {
	case err == nil:
		return vector.GenerationID(id), fp, true, nil
	case errors.Is(err, sql.ErrNoRows):
		return 0, "", false, nil
	default:
		return 0, "", false, fmt.Errorf("lookup building generation: %w", err)
	}
}

// isUniqueViolation matches PostgreSQL's SQLSTATE 23505 via pgconn's
// typed error so locale-dependent message text cannot break detection.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	return pgErr.Code == "23505"
}

// seedPending inserts one pending_embeddings row per live message in
// the main schema. Uses ON CONFLICT DO NOTHING for idempotency on
// retries and to deduplicate against rows already added by the
// concurrent Enqueuer path (parallel to sqlitevec's INSERT OR IGNORE).
//
// Because messages and pending_embeddings live in the same Postgres
// database, this can be done in a single INSERT … SELECT rather than
// streaming rows through Go like the SQLite backend does.
func (b *Backend) seedPending(ctx context.Context, gen vector.GenerationID, now int64) error {
	tx, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin seed tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	stmt := fmt.Sprintf(`
		INSERT INTO pending_embeddings (generation_id, message_id, enqueued_at)
		SELECT $1, id, $2
		  FROM messages
		 WHERE %s
		ON CONFLICT (generation_id, message_id) DO NOTHING`,
		store.LiveMessagesWhere("", true))
	if _, err := tx.ExecContext(ctx, stmt, int64(gen), now); err != nil {
		return fmt.Errorf("seed pending: %w", err)
	}
	return tx.Commit()
}

// ActivateGeneration atomically retires the current active generation
// (if any) and promotes gen to active.
func (b *Backend) ActivateGeneration(ctx context.Context, gen vector.GenerationID) error {
	now := time.Now().Unix()
	tx, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx,
		`UPDATE index_generations
		    SET state = 'retired', completed_at = COALESCE(completed_at, $1)
		  WHERE state = 'active'`, now); err != nil {
		return fmt.Errorf("retire previous active: %w", err)
	}
	res, err := tx.ExecContext(ctx,
		`UPDATE index_generations
		    SET state = 'active', activated_at = $1, completed_at = COALESCE(completed_at, $2)
		  WHERE id = $3 AND state = 'building'`, now, now, int64(gen))
	if err != nil {
		return fmt.Errorf("activate: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("generation %d not in 'building' state", gen)
	}
	return tx.Commit()
}

// RetireGeneration marks the given generation as retired.
func (b *Backend) RetireGeneration(ctx context.Context, gen vector.GenerationID) error {
	_, err := b.db.ExecContext(ctx,
		`UPDATE index_generations SET state = 'retired' WHERE id = $1`, int64(gen))
	return err
}

// ActiveGeneration returns the current active generation, or
// vector.ErrNoActiveGeneration if none exists.
func (b *Backend) ActiveGeneration(ctx context.Context) (vector.Generation, error) {
	return b.generationByState(ctx, vector.GenerationActive)
}

// BuildingGeneration returns the current building generation, or nil
// if none exists.
func (b *Backend) BuildingGeneration(ctx context.Context) (*vector.Generation, error) {
	g, err := b.generationByState(ctx, vector.GenerationBuilding)
	if errors.Is(err, vector.ErrNoActiveGeneration) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &g, nil
}

func (b *Backend) generationByState(ctx context.Context, state vector.GenerationState) (vector.Generation, error) {
	var g vector.Generation
	var startedAt int64
	var completedAt, activatedAt sql.NullInt64
	err := b.db.QueryRowContext(ctx,
		`SELECT id, model, dimension, fingerprint, state,
		        started_at, completed_at, activated_at, message_count
		   FROM index_generations WHERE state = $1`, string(state)).Scan(
		&g.ID, &g.Model, &g.Dimension, &g.Fingerprint, &g.State,
		&startedAt, &completedAt, &activatedAt, &g.MessageCount)
	if errors.Is(err, sql.ErrNoRows) {
		return vector.Generation{}, vector.ErrNoActiveGeneration
	}
	if err != nil {
		return vector.Generation{}, err
	}
	g.StartedAt = time.Unix(startedAt, 0)
	if completedAt.Valid {
		t := time.Unix(completedAt.Int64, 0)
		g.CompletedAt = &t
	}
	if activatedAt.Valid {
		t := time.Unix(activatedAt.Int64, 0)
		g.ActivatedAt = &t
	}
	return g, nil
}

// Upsert writes chunks to the given generation. Transactional.
// Dimension is verified per-chunk against the generation's recorded
// dimension. Mirrors sqlitevec.Upsert semantics: ErrUnknownGeneration
// when gen is missing, ErrDimensionMismatch when any chunk's length
// disagrees with the generation's dimension.
func (b *Backend) Upsert(ctx context.Context, gen vector.GenerationID, chunks []vector.Chunk) error {
	if len(chunks) == 0 {
		return nil
	}

	var dim int
	err := b.db.QueryRowContext(ctx,
		`SELECT dimension FROM index_generations WHERE id = $1`, int64(gen)).Scan(&dim)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: %d", vector.ErrUnknownGeneration, gen)
	}
	if err != nil {
		return fmt.Errorf("lookup generation %d: %w", gen, err)
	}
	for _, c := range chunks {
		if len(c.Vector) != dim {
			return fmt.Errorf("%w: chunk for msg %d has %d dims, gen has %d",
				vector.ErrDimensionMismatch, c.MessageID, len(c.Vector), dim)
		}
	}

	tx, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin upsert tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	chunkIDs := make([]int64, len(chunks))
	for i, c := range chunks {
		chunkIDs[i] = c.MessageID
	}
	preexisting, err := countExistingEmbeddingsTx(ctx, tx, gen, chunkIDs)
	if err != nil {
		return err
	}

	now := time.Now().Unix()
	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO embeddings
		  (generation_id, message_id, embedded_at, source_char_len, truncated, dimension, embedding)
		VALUES ($1, $2, $3, $4, $5, $6, $7::vector)
		ON CONFLICT (generation_id, message_id) DO UPDATE
		   SET embedded_at     = EXCLUDED.embedded_at,
		       source_char_len = EXCLUDED.source_char_len,
		       truncated       = EXCLUDED.truncated,
		       dimension       = EXCLUDED.dimension,
		       embedding       = EXCLUDED.embedding`)
	if err != nil {
		return fmt.Errorf("prepare embeddings upsert: %w", err)
	}
	defer func() { _ = stmt.Close() }()

	for _, c := range chunks {
		if _, err := stmt.ExecContext(ctx,
			int64(gen), c.MessageID, now, c.SourceCharLen, c.Truncated, dim,
			vectorLiteral(c.Vector),
		); err != nil {
			return fmt.Errorf("upsert embedding for msg %d: %w", c.MessageID, err)
		}
	}

	delta := len(chunks) - preexisting
	if err := applyMessageCountDeltaTx(ctx, tx, gen, delta); err != nil {
		return err
	}
	return tx.Commit()
}

func applyMessageCountDeltaTx(ctx context.Context, tx *sql.Tx, gen vector.GenerationID, delta int) error {
	if delta == 0 {
		return nil
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE index_generations SET message_count = message_count + $1 WHERE id = $2`,
		delta, int64(gen)); err != nil {
		return fmt.Errorf("update message_count: %w", err)
	}
	return nil
}

func countExistingEmbeddingsTx(ctx context.Context, tx *sql.Tx, gen vector.GenerationID, ids []int64) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	var n int
	err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM embeddings
		  WHERE generation_id = $1 AND message_id = ANY($2::bigint[])`,
		int64(gen), int64Array(ids)).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count existing embeddings: %w", err)
	}
	return n, nil
}

// vectorLiteral formats a float32 slice as pgvector's text input
// format, e.g. "[1.0,2.5,-3.14]". Bound via $N::vector this works
// without the pgvector-go binding, which keeps the dependency surface
// minimal — pgx already ships in this repo.
func vectorLiteral(v []float32) string {
	var sb strings.Builder
	sb.Grow(len(v) * 8)
	sb.WriteByte('[')
	for i, f := range v {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(strconv.FormatFloat(float64(f), 'g', -1, 32))
	}
	sb.WriteByte(']')
	return sb.String()
}

// int64Array formats an int64 slice as the PostgreSQL array literal
// "{1,2,3}". Bound via $N::bigint[].
func int64Array(ids []int64) string {
	var sb strings.Builder
	sb.Grow(len(ids) * 8)
	sb.WriteByte('{')
	for i, id := range ids {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(strconv.FormatInt(id, 10))
	}
	sb.WriteByte('}')
	return sb.String()
}

// parseVectorLiteral decodes pgvector's text output ("[1,2,3]") back
// into a []float32 of length dim. Returns an error if the row reports
// a different number of components — guards against accidentally
// loading a vector from a different generation.
func parseVectorLiteral(s string, dim int) ([]float32, error) {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "[") || !strings.HasSuffix(s, "]") {
		return nil, fmt.Errorf("malformed vector literal: %q", s)
	}
	body := strings.TrimSpace(s[1 : len(s)-1])
	if body == "" {
		if dim != 0 {
			return nil, fmt.Errorf("vector is empty, want %d dims", dim)
		}
		return nil, nil
	}
	parts := strings.Split(body, ",")
	if len(parts) != dim {
		return nil, fmt.Errorf("vector has %d dims, want %d", len(parts), dim)
	}
	out := make([]float32, dim)
	for i, p := range parts {
		f, err := strconv.ParseFloat(strings.TrimSpace(p), 32)
		if err != nil {
			return nil, fmt.Errorf("parse vector component %d: %w", i, err)
		}
		out[i] = float32(f)
	}
	return out, nil
}

// LoadVector returns the embedding for messageID in the active
// generation. Mirrors sqlitevec.LoadVector.
func (b *Backend) LoadVector(ctx context.Context, messageID int64) ([]float32, error) {
	active, err := b.ActiveGeneration(ctx)
	if err != nil {
		return nil, err
	}
	var lit string
	err = b.db.QueryRowContext(ctx,
		`SELECT embedding::text FROM embeddings
		  WHERE generation_id = $1 AND message_id = $2`,
		int64(active.ID), messageID).Scan(&lit)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("no embedding for message %d in generation %d", messageID, active.ID)
	}
	if err != nil {
		return nil, fmt.Errorf("load vector for message %d: %w", messageID, err)
	}
	return parseVectorLiteral(lit, active.Dimension)
}

// Search runs an ANN query against the given generation and returns
// the top-k hits (optionally intersected with a structured filter).
// Uses pgvector's cosine-distance operator (<=>), which returns 0..2;
// hits are emitted with Score = 1 - distance to align with the
// sqlitevec convention.
func (b *Backend) Search(ctx context.Context, gen vector.GenerationID, queryVec []float32, k int, filter vector.Filter) ([]vector.Hit, error) {
	if len(queryVec) == 0 {
		return nil, fmt.Errorf("search: empty query vector")
	}
	var dim int
	err := b.db.QueryRowContext(ctx,
		`SELECT dimension FROM index_generations WHERE id = $1`, int64(gen)).Scan(&dim)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: %d", vector.ErrUnknownGeneration, gen)
	}
	if err != nil {
		return nil, fmt.Errorf("lookup generation %d: %w", gen, err)
	}
	if len(queryVec) != dim {
		return nil, fmt.Errorf("%w: query has %d dims, gen has %d",
			vector.ErrDimensionMismatch, len(queryVec), dim)
	}

	// Filter resolution. Unlike sqlitevec, embeddings live in the same
	// database as messages — we can express the live-message and
	// structured filters as a single SQL query against both tables
	// without ATTACHing or json_each tricks. Empty filter still benefits
	// from a slim fast path that elides the join.
	queryVecLit := vectorLiteral(queryVec)
	if filter.IsEmpty() {
		// Fast path: ANN order with an inline live-message check via
		// EXISTS to skip soft-deleted rows without dragging the whole
		// recipients/labels machinery in.
		stmt := fmt.Sprintf(`
			SELECT e.message_id,
			       (e.embedding::vector(%d)) <=> $1::vector AS distance
			  FROM embeddings e
			 WHERE e.generation_id = $2
			   AND EXISTS (
			        SELECT 1 FROM messages m
			         WHERE m.id = e.message_id AND %s)
			 ORDER BY (e.embedding::vector(%d)) <=> $1::vector
			 LIMIT $3`, dim, store.LiveMessagesWhere("m", true), dim)
		return b.scanHits(ctx, stmt, queryVecLit, int64(gen), k)
	}

	ids, err := b.filteredMessageIDs(ctx, filter)
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, nil
	}
	stmt := fmt.Sprintf(`
		SELECT e.message_id,
		       (e.embedding::vector(%d)) <=> $1::vector AS distance
		  FROM embeddings e
		 WHERE e.generation_id = $2
		   AND e.message_id = ANY($3::bigint[])
		 ORDER BY (e.embedding::vector(%d)) <=> $1::vector
		 LIMIT $4`, dim, dim)
	return b.scanHits(ctx, stmt, queryVecLit, int64(gen), int64Array(ids), k)
}

func (b *Backend) scanHits(ctx context.Context, query string, args ...any) ([]vector.Hit, error) {
	rows, err := b.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("ann query: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var hits []vector.Hit
	for i := 1; rows.Next(); i++ {
		var id int64
		var dist float64
		if err := rows.Scan(&id, &dist); err != nil {
			return nil, fmt.Errorf("scan hit: %w", err)
		}
		hits = append(hits, vector.Hit{
			MessageID: id,
			Score:     1.0 - dist,
			Rank:      i,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate hits: %w", err)
	}
	return hits, nil
}

// filteredMessageIDs resolves a structured filter against the main
// schema, returning matching message IDs. Mirrors the sqlitevec
// resolveFilter shape but uses ANY($1::bigint[]) in place of
// json_each. Date bounds bind time.Time directly because messages.sent_at
// is TIMESTAMPTZ in schema_pg.sql.
func (b *Backend) filteredMessageIDs(ctx context.Context, f vector.Filter) ([]int64, error) {
	clauses := []string{store.LiveMessagesWhere("m", true)}
	var args []any
	// bind appends v as a query argument and returns the matching $N
	// placeholder. Using a counter-based helper keeps the conditional
	// WHERE assembly tidy without juggling positional indexes by hand.
	bind := func(v any) string {
		args = append(args, v)
		return fmt.Sprintf("$%d", len(args))
	}

	if len(f.SourceIDs) > 0 {
		clauses = append(clauses, fmt.Sprintf("m.source_id = ANY(%s::bigint[])", bind(int64Array(f.SourceIDs))))
	}
	for _, group := range f.SenderGroups {
		if len(group) == 0 {
			continue
		}
		clauses = append(clauses, fmt.Sprintf(
			`EXISTS (
				SELECT 1 FROM message_recipients mr
				 WHERE mr.message_id = m.id
				   AND mr.recipient_type = 'from'
				   AND mr.participant_id = ANY(%s::bigint[])
			)`, bind(int64Array(group))))
	}
	addRecipientGroups := func(recipientType string, groups [][]int64) {
		for _, ids := range groups {
			if len(ids) == 0 {
				continue
			}
			clauses = append(clauses, fmt.Sprintf(
				`EXISTS (
					SELECT 1 FROM message_recipients mr
					 WHERE mr.message_id = m.id
					   AND mr.recipient_type = '%s'
					   AND mr.participant_id = ANY(%s::bigint[])
				)`, recipientType, bind(int64Array(ids))))
		}
	}
	addRecipientGroups("to", f.ToGroups)
	addRecipientGroups("cc", f.CcGroups)
	addRecipientGroups("bcc", f.BccGroups)

	if f.HasAttachment != nil {
		clauses = append(clauses, fmt.Sprintf("m.has_attachments = %s", bind(*f.HasAttachment)))
	}
	if f.After != nil {
		clauses = append(clauses, fmt.Sprintf("m.sent_at >= %s", bind(*f.After)))
	}
	if f.Before != nil {
		clauses = append(clauses, fmt.Sprintf("m.sent_at < %s", bind(*f.Before)))
	}
	if f.LargerThan != nil {
		clauses = append(clauses, fmt.Sprintf("m.size_estimate > %s", bind(*f.LargerThan)))
	}
	if f.SmallerThan != nil {
		clauses = append(clauses, fmt.Sprintf("m.size_estimate < %s", bind(*f.SmallerThan)))
	}
	for _, term := range f.SubjectSubstrings {
		clauses = append(clauses, fmt.Sprintf(
			`m.subject LIKE %s ESCAPE '\'`,
			bind("%"+escapeLikeSubject(term)+"%")))
	}
	for _, ids := range f.LabelGroups {
		if len(ids) == 0 {
			continue
		}
		clauses = append(clauses, fmt.Sprintf(
			`EXISTS (SELECT 1 FROM message_labels ml
			          WHERE ml.message_id = m.id
			            AND ml.label_id = ANY(%s::bigint[]))`,
			bind(int64Array(ids))))
	}

	query := `SELECT m.id FROM messages m WHERE ` + strings.Join(clauses, " AND ")
	rows, err := b.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("filter query: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan filter id: %w", err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate filter ids: %w", err)
	}
	return out, nil
}

// escapeLikeSubject escapes SQL LIKE special characters so they match
// literally. Mirrors the sqlitevec helper of the same name.
func escapeLikeSubject(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `%`, `\%`)
	s = strings.ReplaceAll(s, `_`, `\_`)
	return s
}

// Delete removes the given messages from the specified generation in
// one transaction. Empty messageIDs is a no-op.
func (b *Backend) Delete(ctx context.Context, gen vector.GenerationID, messageIDs []int64) error {
	if len(messageIDs) == 0 {
		return nil
	}
	var dim int
	err := b.db.QueryRowContext(ctx,
		`SELECT dimension FROM index_generations WHERE id = $1`, int64(gen)).Scan(&dim)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: %d", vector.ErrUnknownGeneration, gen)
	}
	if err != nil {
		return fmt.Errorf("lookup generation %d: %w", gen, err)
	}

	tx, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin delete tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	willDelete, err := countExistingEmbeddingsTx(ctx, tx, gen, messageIDs)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM embeddings
		  WHERE generation_id = $1 AND message_id = ANY($2::bigint[])`,
		int64(gen), int64Array(messageIDs)); err != nil {
		return fmt.Errorf("delete embeddings: %w", err)
	}
	if err := applyMessageCountDeltaTx(ctx, tx, gen, -willDelete); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit delete tx: %w", err)
	}
	return nil
}

// Stats returns counts for the given generation. When gen == 0,
// counts are aggregated across all generations. StorageBytes is
// reported by the underlying database (pg_relation_size of the
// embeddings table) — a single value across generations, which the
// caller can interpret with that caveat.
func (b *Backend) Stats(ctx context.Context, gen vector.GenerationID) (vector.Stats, error) {
	var s vector.Stats
	where := "WHERE generation_id = $1"
	args := []any{int64(gen)}
	if gen == 0 {
		where, args = "", nil
	} else {
		var exists int
		err := b.db.QueryRowContext(ctx,
			`SELECT 1 FROM index_generations WHERE id = $1`, int64(gen)).Scan(&exists)
		if errors.Is(err, sql.ErrNoRows) {
			return s, fmt.Errorf("%w: %d", vector.ErrUnknownGeneration, gen)
		}
		if err != nil {
			return s, fmt.Errorf("lookup generation %d: %w", gen, err)
		}
	}

	if err := b.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM embeddings `+where, args...).Scan(&s.EmbeddingCount); err != nil {
		return s, fmt.Errorf("count embeddings: %w", err)
	}
	if err := b.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pending_embeddings `+where, args...).Scan(&s.PendingCount); err != nil {
		return s, fmt.Errorf("count pending: %w", err)
	}
	return s, nil
}
