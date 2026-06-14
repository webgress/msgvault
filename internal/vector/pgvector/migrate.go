//go:build pgvector

package pgvector

import (
	"context"
	"database/sql"
	_ "embed"
	"fmt"
)

//go:embed schema.sql
var schemaSQL string

// Migrate enables the pgvector extension and applies the embedding
// schema. Safe to run on every startup: every statement uses IF NOT
// EXISTS or its equivalent.
//
// When skipExtension is true, the `CREATE EXTENSION IF NOT EXISTS vector`
// step is skipped — the vector extension is assumed to be managed/installed
// externally (by a DBA on a locked-down/managed PostgreSQL where a
// non-superuser cannot run CREATE EXTENSION). Schema, index, and the
// redundant-index drop below still run, so a non-superuser that holds DDL
// rights on its own schema can bring up the embedding tables. This differs
// from the read-only SkipMigrate path, which suppresses ALL DDL.
//
// The defaultDim argument is informational: a per-dimension HNSW index
// will be created lazily for the first generation that exercises a new
// dimension. If defaultDim > 0, Migrate eagerly creates the index for
// that dimension so the first ANN query doesn't pay the index build.
func Migrate(ctx context.Context, db *sql.DB, defaultDim int, skipExtension bool) error {
	if !skipExtension {
		if _, err := db.ExecContext(ctx, `CREATE EXTENSION IF NOT EXISTS vector`); err != nil {
			return fmt.Errorf("create extension vector: %w", err)
		}
	}
	if _, err := db.ExecContext(ctx, schemaSQL); err != nil {
		return fmt.Errorf("apply pgvector schema: %w", err)
	}
	// Shed the redundant idx_embeddings_gen_msg index on existing DBs: it is
	// a pure leading-prefix of the embeddings primary key
	// (generation_id, message_id, chunk_index), so it only added write
	// amplification on the hottest table. New DBs never create it (the
	// CREATE was removed from schema.sql); this DROP cleans up DBs migrated
	// before that change. [V3]
	if _, err := db.ExecContext(ctx, `DROP INDEX IF EXISTS idx_embeddings_gen_msg`); err != nil {
		return fmt.Errorf("drop redundant idx_embeddings_gen_msg: %w", err)
	}
	if defaultDim > 0 {
		if err := EnsureVectorIndex(ctx, db, defaultDim); err != nil {
			return err
		}
	}
	return nil
}

// EnsureVectorIndex creates a partial HNSW cosine index restricted to
// rows where dimension = dim. The partial WHERE guard lets generations
// with different dimensions coexist in the same embeddings table — the
// expression cast `(embedding::vector(dim))` only fires for rows that
// already match, so a 4-dim row never trips a 768-dim index. Idempotent.
func EnsureVectorIndex(ctx context.Context, db *sql.DB, dim int) error {
	if dim <= 0 {
		return fmt.Errorf("invalid dimension %d", dim)
	}
	// vector_cosine_ops matches the cosine-similarity distance operator
	// (<=>) used by Search. HNSW build cost is modest for empty
	// tables, so creating eagerly on the first generation is cheap and
	// avoids paying for it on the first ANN query.
	stmt := fmt.Sprintf(
		`CREATE INDEX IF NOT EXISTS idx_embeddings_hnsw_d%d
		   ON embeddings
		USING hnsw ((embedding::vector(%d)) vector_cosine_ops)
		   WHERE dimension = %d`,
		dim, dim, dim,
	)
	if _, err := db.ExecContext(ctx, stmt); err != nil {
		return fmt.Errorf("create hnsw index for dim %d: %w", dim, err)
	}
	return nil
}

// VectorIndexName returns the dimension-specific HNSW index name.
// Exposed mainly for diagnostic purposes.
func VectorIndexName(dim int) string {
	return fmt.Sprintf("idx_embeddings_hnsw_d%d", dim)
}
