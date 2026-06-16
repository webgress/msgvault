package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
)

var (
	migrateFrom         string
	migrateTo           string
	migrateFromHome     string
	migrateToHome       string
	migrateAttachments  bool
	migrateNoAttach     bool
	migrateVectors      string
	migrateDryRun       bool
	migrateVerify       bool
	migrateResume       bool
	migrateBatch        int
	migrateRebuildFTS   bool
	migrateNoRebuildFTS bool
	migrateTruncateDest bool
)

var migrateCmd = &cobra.Command{
	Use:   "migrate",
	Short: "Copy a full vault between SQLite and PostgreSQL backends",
	Long: `Copy every table of a msgvault archive from one database backend to the
other, in either direction. The backend for each side is selected by its DSN:
a postgres:// or postgresql:// URL opens PostgreSQL, anything else is a SQLite
file path.

All primary keys are preserved verbatim so foreign keys stay valid without
remapping. On-disk attachment blobs are copied by default. Full-text search is
rebuilt on the destination (it is backend-specific and never copied). Vector
embeddings are NOT copied — re-embed on the destination after migrating.

Examples:
  # SQLite (default vault) -> PostgreSQL
  msgvault migrate --to "postgres://user:pass@host:5432/msgvault"

  # PostgreSQL -> a fresh SQLite vault, deriving paths from a home dir
  msgvault migrate --from "postgres://user:pass@host:5432/msgvault" \
      --to ~/backup/msgvault.db --to-home ~/backup

  # Preview without writing anything
  msgvault migrate --to "postgres://..." --dry-run

The destination schema is initialized automatically. Refuses to run if the
destination already has data unless --resume (conflict-skip) or --truncate-dest
is given.`,
	RunE: runMigrate,
}

func init() {
	f := migrateCmd.Flags()
	f.StringVar(&migrateFrom, "from", "", "source DSN (default: configured vault database)")
	f.StringVar(&migrateTo, "to", "", "destination DSN (required)")
	f.StringVar(&migrateFromHome, "from-home", "", "derive source DSN and attachments dir from this msgvault home directory")
	f.StringVar(&migrateToHome, "to-home", "", "derive destination DSN and attachments dir from this msgvault home directory")
	f.BoolVar(&migrateAttachments, "attachments", true, "copy on-disk attachment blobs")
	f.BoolVar(&migrateNoAttach, "no-attachments", false, "skip copying attachment blobs")
	f.StringVar(&migrateVectors, "vectors", "skip", "vector handling: skip|copy|reembed (copy is not implemented)")
	f.BoolVar(&migrateDryRun, "dry-run", false, "report source row counts without writing")
	f.BoolVar(&migrateVerify, "verify", false, "verify the migration after copying (row counts, ids, FK integrity, FTS)")
	f.BoolVar(&migrateResume, "resume", false, "conflict-skip rows that already exist on the destination")
	f.IntVar(&migrateBatch, "batch", 5000, "rows per multi-row INSERT batch")
	f.BoolVar(&migrateRebuildFTS, "rebuild-fts", true, "rebuild the full-text search index on the destination")
	f.BoolVar(&migrateNoRebuildFTS, "no-rebuild-fts", false, "do not rebuild the full-text search index on the destination")
	f.BoolVar(&migrateTruncateDest, "truncate-dest", false, "delete existing destination data before copying")
	migrateCmd.MarkFlagsMutuallyExclusive("attachments", "no-attachments")
	migrateCmd.MarkFlagsMutuallyExclusive("rebuild-fts", "no-rebuild-fts")
	migrateCmd.MarkFlagsMutuallyExclusive("resume", "truncate-dest")
	rootCmd.AddCommand(migrateCmd)
}

// migrateSide bundles a resolved DSN and attachments directory for one end of
// the migration.
type migrateSide struct {
	dsn       string
	attachDir string // "" when it could not be resolved
}

// resolveSide turns a flag DSN + optional home into a concrete (dsn, attachDir).
// Precedence:
//   - --X-home set: load config from that home; DSN is the flag value if given,
//     else the home's configured DSN; attachments dir comes from the home.
//   - DSN flag set, no home: use it as-is. Attachments dir resolves only if the
//     DSN equals the active config vault's DSN (then cfg.AttachmentsDir()),
//     otherwise it stays unresolved.
//   - neither: fall back to the active config vault (DSN + attachments dir).
func resolveSide(flagDSN, home string) (migrateSide, error) {
	if home != "" {
		hc, err := config.Load("", home)
		if err != nil {
			return migrateSide{}, fmt.Errorf("load home config %q: %w", home, err)
		}
		dsn := flagDSN
		if dsn == "" {
			dsn = hc.DatabaseDSN()
		}
		return migrateSide{dsn: dsn, attachDir: hc.AttachmentsDir()}, nil
	}

	if flagDSN != "" {
		side := migrateSide{dsn: flagDSN}
		// Only the active config vault has a known attachments dir for a bare
		// DSN. Match against its DSN so we never point at the wrong blobs.
		if cfg != nil && flagDSN == cfg.DatabaseDSN() {
			side.attachDir = cfg.AttachmentsDir()
		}
		return side, nil
	}

	// Default: the active config vault.
	if cfg == nil {
		return migrateSide{}, errors.New("no configuration loaded")
	}
	return migrateSide{dsn: cfg.DatabaseDSN(), attachDir: cfg.AttachmentsDir()}, nil
}

func runMigrate(cmd *cobra.Command, _ []string) error {
	if migrateTo == "" {
		return usageErr(cmd, errors.New("--to is required"))
	}
	switch migrateVectors {
	case "skip", "reembed":
		// ok
	case "copy":
		return usageErr(cmd, errors.New("--vectors copy is not implemented; use skip (default) or reembed"))
	default:
		return usageErr(cmd, fmt.Errorf("invalid --vectors %q: want skip|copy|reembed", migrateVectors))
	}

	// Resolve negation flags. MarkFlagsMutuallyExclusive prevents both being
	// set, so a true negation flag unambiguously wins.
	copyAttachments := migrateAttachments && !migrateNoAttach
	rebuildFTS := migrateRebuildFTS && !migrateNoRebuildFTS

	from, err := resolveSide(migrateFrom, migrateFromHome)
	if err != nil {
		return err
	}
	to, err := resolveSide(migrateTo, migrateToHome)
	if err != nil {
		return err
	}

	if from.dsn == to.dsn {
		return usageErr(cmd, fmt.Errorf("--from and --to resolve to the same database (%s)", from.dsn))
	}

	// cobra's Execute replaces a nil command context with context.Background;
	// guard for direct RunE invocation (tests) where that has not happened so a
	// nil context never reaches the database/sql layer.
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}

	src, err := store.Open(from.dsn)
	if err != nil {
		return fmt.Errorf("open source %s: %w", from.dsn, err)
	}
	defer func() { _ = src.Close() }()

	// Guard: source must have at least one account.
	var sourceCount int64
	if err := src.DB().QueryRowContext(ctx, "SELECT COUNT(*) FROM sources").Scan(&sourceCount); err != nil {
		return fmt.Errorf("count source sources: %w", err)
	}
	if sourceCount == 0 {
		return errors.New("source database has zero accounts (sources table is empty) — nothing to migrate")
	}

	dst, err := store.Open(to.dsn)
	if err != nil {
		return fmt.Errorf("open destination %s: %w", to.dsn, err)
	}
	defer func() { _ = dst.Close() }()

	if err := dst.InitSchema(); err != nil {
		return fmt.Errorf("initialize destination schema: %w", err)
	}

	if !migrateDryRun {
		if err := prepareDest(ctx, cmd, dst); err != nil {
			return err
		}
	}

	// Resolve attachment dirs up front so a misconfiguration fails before any
	// data is written (never silently drop blobs).
	if copyAttachments && !migrateDryRun {
		if from.attachDir == "" {
			return errors.New("cannot resolve SOURCE attachments directory: pass --from-home, " +
				"point --from at the configured vault, or use --no-attachments to skip blobs")
		}
		if to.attachDir == "" {
			return errors.New("cannot resolve DESTINATION attachments directory: pass --to-home, " +
				"point --to at the configured vault, or use --no-attachments to skip blobs")
		}
	}

	fmt.Fprintf(os.Stderr, "Migrating %s -> %s\n", backendLabel(from.dsn), backendLabel(to.dsn))

	result, err := store.MigrateVault(ctx, src, dst, store.MigrateOptions{
		Batch:    migrateBatch,
		Resume:   migrateResume,
		DryRun:   migrateDryRun,
		Progress: migrateProgress(),
	})
	if err != nil {
		if src.IsBusyError(err) || dst.IsBusyError(err) {
			return errors.New("database is busy — stop 'msgvault serve' and any other clients, then retry")
		}
		return fmt.Errorf("migrate: %w", err)
	}

	printMigrateSummary(result)

	if migrateDryRun {
		return nil
	}

	// Rebuild FTS on the destination (FTS is backend-specific, never copied).
	if rebuildFTS {
		fmt.Fprintln(os.Stderr, "Rebuilding full-text search index on destination...")
		if _, err := dst.RebuildFTS(ftsProgressBar()); err != nil {
			fmt.Fprintln(os.Stderr)
			return fmt.Errorf("rebuild FTS on destination: %w", err)
		}
		fmt.Fprintln(os.Stderr)
	}

	// Copy attachment blobs.
	if copyAttachments {
		fmt.Fprintln(os.Stderr, "Copying attachment blobs...")
		ar, err := store.CopyAttachments(ctx, src, from.attachDir, to.attachDir)
		if err != nil {
			return fmt.Errorf("copy attachments: %w", err)
		}
		fmt.Fprintf(os.Stderr, "  attachments: %d copied, %d skipped (%d bytes)\n",
			ar.Copied, ar.Skipped, ar.Bytes)
	}

	// Vectors: never copied across backends. Surface the re-embed instruction.
	if migrateVectors == "reembed" {
		fmt.Fprintln(os.Stderr,
			"Vectors: run 'msgvault embeddings build' against the destination to re-embed.")
	}

	if migrateVerify {
		fmt.Fprintln(os.Stderr, "Verifying migration...")
		vr, err := store.VerifyMigration(ctx, src, dst)
		if err != nil {
			return fmt.Errorf("verify: %w", err)
		}
		printVerifyResult(vr)
		if !vr.OK() {
			return fmt.Errorf("verification failed: %d problem(s)", len(vr.Problems))
		}
		fmt.Fprintln(os.Stderr, "Verification passed.")
	}

	return nil
}

// prepareDest enforces the non-empty-destination guard and clears the
// destination before a fresh copy.
//
//   - With real data present and neither --resume nor --truncate-dest: refuse.
//   - With --truncate-dest: clear all data tables (and reset PG sequences).
//   - On a clean (non-resume) run: clear the pristine baseline (the auto-seeded
//     default "All" collection) so the source's preserved-id collections land
//     without colliding with the freshly seeded one.
//   - With --resume: leave the destination as-is; conflict-skip handles overlap.
func prepareDest(ctx context.Context, cmd *cobra.Command, dst *store.Store) error {
	nonEmpty, table, err := store.DestHasData(ctx, dst)
	if err != nil {
		return fmt.Errorf("check destination state: %w", err)
	}
	if nonEmpty && !migrateResume && !migrateTruncateDest {
		return usageErr(cmd, fmt.Errorf(
			"destination already has data (table %q is non-empty); "+
				"use --resume to conflict-skip or --truncate-dest to overwrite", table))
	}
	if migrateTruncateDest {
		fmt.Fprintln(os.Stderr, "Truncating destination data tables...")
		if err := store.TruncateDest(ctx, dst); err != nil {
			return fmt.Errorf("truncate destination: %w", err)
		}
		return nil
	}
	if !migrateResume {
		// Clear the InitSchema baseline (default collection) so preserved ids
		// from the source land cleanly. The empty-guard above already proved
		// there is no real data to lose here.
		if err := store.TruncateDest(ctx, dst); err != nil {
			return fmt.Errorf("clear destination baseline: %w", err)
		}
	}
	return nil
}

// backendLabel returns a short human label for a DSN's backend.
func backendLabel(dsn string) string {
	if store.IsPostgresURL(dsn) {
		return "PostgreSQL"
	}
	return "SQLite(" + dsn + ")"
}

// migrateProgress returns a per-table progress callback that prints a single
// updating line to stderr.
func migrateProgress() func(table string, done, total int64) {
	return func(table string, done, total int64) {
		if total <= 0 {
			return
		}
		if done > total {
			done = total
		}
		pct := int(done * 100 / total)
		fmt.Fprintf(os.Stderr, "\r  %-26s %3d%% (%d/%d)        ", table, pct, done, total)
		if done >= total {
			fmt.Fprintln(os.Stderr)
		}
	}
}

// ftsProgressBar returns the same stderr progress bar rebuild-fts uses.
func ftsProgressBar() func(done, total int64) {
	return func(done, total int64) {
		if total <= 0 {
			return
		}
		if done > total {
			done = total
		}
		pct := int(done * 100 / total)
		barWidth := 30
		filled := barWidth * pct / 100
		bar := strings.Repeat("=", filled) + strings.Repeat(" ", barWidth-filled)
		fmt.Fprintf(os.Stderr, "\r  [%s] %3d%%", bar, pct)
	}
}

func printMigrateSummary(r *store.MigrateResult) {
	if r.DryRun {
		fmt.Fprintln(os.Stderr, "Dry run — source row counts:")
	} else {
		fmt.Fprintln(os.Stderr, "Copy complete:")
	}
	for _, t := range r.Tables {
		fmt.Fprintf(os.Stderr, "  %-26s %d\n", t.Table, t.Rows)
	}
	fmt.Fprintf(os.Stderr, "  %-26s %d rows", "TOTAL", r.RowsCopied())
	if !r.DryRun {
		fmt.Fprintf(os.Stderr, " in %s", r.Elapsed.Round(1e6))
	}
	fmt.Fprintln(os.Stderr)
}

func printVerifyResult(vr *store.VerifyResult) {
	for _, t := range vr.Tables {
		status := "ok"
		if !t.OK {
			status = "MISMATCH: " + t.Mismatch
		}
		fmt.Fprintf(os.Stderr, "  %-26s src=%d dst=%d %s\n", t.Table, t.SrcRows, t.DstRows, status)
	}
	if len(vr.Problems) > 0 {
		fmt.Fprintln(os.Stderr, "Problems:")
		for _, p := range vr.Problems {
			fmt.Fprintf(os.Stderr, "  - %s\n", p)
		}
	}
}
