package cmd

import (
	"database/sql"

	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/embed"
	"go.kenn.io/msgvault/internal/vector/hybrid"
)

// vectorFeatures carries the optional vector-search components that the
// serve, mcp, sync, and sync-full commands wire into their servers and
// sync pipelines. It is populated only when cfg.Vector.Enabled is true
// AND the binary is built with -tags sqlite_vec; otherwise
// setupVectorFeatures returns (nil, nil) or a clear error.
//
// When non-nil, all fields are populated (invariant enforced by
// setupVectorFeatures). Callers only need to nil-check vf itself.
type vectorFeatures struct {
	Backend      vector.Backend
	HybridEngine *hybrid.Engine
	Worker       *embed.Worker
	Cfg          vector.Config
	// VectorsDB is the underlying vectors.db handle (vectors.db on
	// SQLite, the shared main DB on PG). The daemon's EmbedJob uses it
	// for the embed_runs/watermark tables; other consumers should prefer
	// the higher-level Backend abstraction.
	VectorsDB *sql.DB
	// Rebind translates ?-placeholders to the driver's native form for
	// raw queries run directly against VectorsDB (the daemon's EmbedJob
	// activation-gate count). Identity on SQLite, PostgreSQLDialect.Rebind
	// on PG. Callers that issue raw SQL against VectorsDB must apply it.
	Rebind func(string) string
	// Close releases the underlying vectors.db handle. Every caller
	// that receives a non-nil vectorFeatures must invoke Close during
	// shutdown so WAL checkpoints complete.
	Close func() error
}
