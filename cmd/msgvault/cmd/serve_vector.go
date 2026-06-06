//go:build sqlite_vec

package cmd

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/embed"
	"go.kenn.io/msgvault/internal/vector/hybrid"
	"go.kenn.io/msgvault/internal/vector/pgvector"
	"go.kenn.io/msgvault/internal/vector/sqlitevec"
)

// setupVectorFeatures opens vectors.db and builds the vector backend,
// hybrid engine, embed worker, and enqueuer used by the serve daemon
// and the MCP command. Returns (nil, nil) when cfg.Vector.Enabled is
// false. The returned Close function must be called on shutdown.
//
// mainDB is the already-opened handle to msgvault.db; mainPath is the
// filesystem path used by FusedSearch to ATTACH vectors.db on a fresh
// connection.
func setupVectorFeatures(ctx context.Context, mainDB *sql.DB, mainPath string) (*vectorFeatures, error) {
	if !cfg.Vector.Enabled {
		return nil, nil //nolint:nilnil // vector disabled: callers nil-check vf; (nil, nil) means "no features, no error"
	}
	// The vector backend uses the sqlite-vec extension and `ATTACH
	// DATABASE` to fuse vectors.db onto the main store — both
	// SQLite-only. Refuse up-front on a PG DSN, BEFORE cfg.Vector.Validate(),
	// so the user gets the actionable SQLite-only message rather than a
	// generic backend-validation error. Vector support for PostgreSQL is
	// tracked under PR4 (see docs/PG_STATUS.md).
	if store.IsPostgresURL(mainPath) {
		return nil, errors.New(
			"vector features are SQLite-only; set [vector] enabled = false to use msgvault with PostgreSQL (vector support is planned for PR4)")
	}
	if err := cfg.Vector.Validate(); err != nil {
		return nil, fmt.Errorf("vector config: %w", err)
	}

	var (
		backend   vector.Backend
		vectorsDB *sql.DB
		closeFn   func() error
	)
	if isPostgresDSN(mainPath) {
		// Same database handle as the main store: pgvector embeddings
		// live alongside messages, so there is no separate vectors.db.
		pgb, err := pgvector.Open(ctx, pgvector.Options{
			DB:        mainDB,
			Dimension: cfg.Vector.Embeddings.Dimension,
		})
		if err != nil {
			return nil, fmt.Errorf("open pgvector backend: %w", err)
		}
		backend = pgb
		vectorsDB = pgb.DB()
		closeFn = pgb.Close
	} else {
		if err := sqlitevec.RegisterExtension(); err != nil {
			return nil, fmt.Errorf("register sqlite-vec: %w", err)
		}
		vecPath := cfg.Vector.DBPath
		if vecPath == "" {
			vecPath = filepath.Join(cfg.Data.DataDir, "vectors.db")
		}
		sb, err := sqlitevec.Open(ctx, sqlitevec.Options{
			Path:      vecPath,
			MainPath:  mainPath,
			Dimension: cfg.Vector.Embeddings.Dimension,
			MainDB:    mainDB,
		})
		if err != nil {
			return nil, fmt.Errorf("open vectors.db: %w", err)
		}
		backend = sb
		vectorsDB = sb.DB()
		closeFn = sb.Close
	}

	client := embed.NewClient(embed.Config{
		Endpoint:   cfg.Vector.Embeddings.Endpoint,
		APIKey:     cfg.Vector.Embeddings.APIKey(),
		Model:      cfg.Vector.Embeddings.Model,
		Dimension:  cfg.Vector.Embeddings.Dimension,
		Timeout:    cfg.Vector.Embeddings.Timeout,
		MaxRetries: cfg.Vector.Embeddings.MaxRetries,
	})

	worker := embed.NewWorker(embed.WorkerDeps{
		Backend:   backend,
		VectorsDB: vectorsDB,
		MainDB:    mainDB,
		Client:    client,
		Preprocess: embed.PreprocessConfig{
			StripQuotes:        cfg.Vector.Preprocess.StripQuotesEnabled(),
			StripSignatures:    cfg.Vector.Preprocess.StripSignaturesEnabled(),
			StripHTML:          cfg.Vector.Preprocess.StripHTMLEnabled(),
			StripBase64:        cfg.Vector.Preprocess.StripBase64Enabled(),
			StripURLTracking:   cfg.Vector.Preprocess.StripURLTrackingEnabled(),
			CollapseWhitespace: cfg.Vector.Preprocess.CollapseWhitespaceEnabled(),
		},
		MaxInputChars:   cfg.Vector.Embeddings.MaxInputChars,
		BatchSize:       cfg.Vector.Embeddings.BatchSize,
		EmbedTimeout:    cfg.Vector.Embeddings.Timeout,
		EmbedMaxRetries: cfg.Vector.Embeddings.MaxRetries,
		Log:             logger,
	})

	engine := hybrid.NewEngine(backend, mainDB, client, hybrid.Config{
		ExpectedFingerprint: cfg.Vector.GenerationFingerprint(),
		RRFK:                cfg.Vector.Search.RRFK,
		KPerSignal:          cfg.Vector.Search.KPerSignal,
		SubjectBoost:        cfg.Vector.Search.SubjectBoost,
	})

	enqueuer := embed.NewEnqueuer(vectorsDB)

	return &vectorFeatures{
		Backend:      backend,
		HybridEngine: engine,
		Enqueuer:     enqueuer,
		Worker:       worker,
		Cfg:          cfg.Vector,
		VectorsDB:    vectorsDB,
		Close:        closeFn,
	}, nil
}

// isPostgresDSN returns true when dsn looks like a PostgreSQL
// connection string. The serve path receives the dsn used to open the
// main store; we re-detect it here rather than threading a *store.Store
// through this layer because it's the cleanest seam — the main DB
// handle is already opened by the caller.
func isPostgresDSN(dsn string) bool {
	return strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://")
}
