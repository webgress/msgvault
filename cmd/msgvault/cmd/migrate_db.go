package cmd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/wesm/msgvault/internal/store"
)

var (
	migrateFromFlag      string
	migrateToFlag        string
	migrateBatchSize     int
	migrateDryRun        bool
	migrateNoFTS         bool
	migrateAllowNonEmpty bool
)

var migrateDBCmd = &cobra.Command{
	Use:   "migrate-db",
	Short: "Copy all msgvault data from one database to another",
	Long: `Copy every row from a source database into a destination database.

Both arguments accept either a SQLite file path or a PostgreSQL URL
(postgres://user:pass@host:port/dbname?sslmode=...). Any combination is
supported — SQLite→Postgres, Postgres→SQLite, or same-dialect copies for
relocation or backup restores.

The destination schema is initialized automatically. If the destination has
existing data in core tables (sources, messages, conversations, participants),
the command refuses to run unless --allow-non-empty is passed.

Row IDs are preserved so attachment storage paths, OAuth token lookups, and
external references (e.g., deletion manifests) remain valid. On PostgreSQL
destinations, identity sequences are reset after the copy so subsequent
auto-generated IDs continue after the max migrated value.

The FTS index is rebuilt on the destination after the copy (unless
--skip-fts-rebuild is set). Attachment files on disk are NOT moved —
copy ~/.msgvault/attachments to the new host yourself.

Examples:

  # SQLite → PostgreSQL
  msgvault migrate-db \
      --from ~/.msgvault/msgvault.db \
      --to postgres://msgvault:pw@localhost:5432/msgvault?sslmode=disable

  # PostgreSQL → SQLite (e.g., for local offline use)
  msgvault migrate-db \
      --from postgres://msgvault:pw@db.local:5432/msgvault?sslmode=disable \
      --to ~/.msgvault-offline/msgvault.db

  # Dry run — prints per-table source row counts without writing anything
  msgvault migrate-db --from ... --to ... --dry-run
`,
	RunE: runMigrateDB,
}

func runMigrateDB(cmd *cobra.Command, args []string) error {
	if migrateFromFlag == "" || migrateToFlag == "" {
		return fmt.Errorf("both --from and --to are required")
	}
	if migrateFromFlag == migrateToFlag {
		return fmt.Errorf("--from and --to must be different databases")
	}

	from := store.RedactPassword(migrateFromFlag)
	to := store.RedactPassword(migrateToFlag)

	src, err := store.OpenReadOnly(migrateFromFlag)
	if err != nil {
		return fmt.Errorf("open source %s: %w", from, err)
	}
	defer func() { _ = src.Close() }()

	if migrateDryRun {
		return runMigrateDryRun(src, from, to)
	}

	dst, err := store.Open(migrateToFlag)
	if err != nil {
		return fmt.Errorf("open destination %s: %w", to, err)
	}
	defer func() { _ = dst.Close() }()

	if err := dst.InitSchema(); err != nil {
		return fmt.Errorf("init destination schema: %w", err)
	}

	fmt.Fprintf(os.Stderr, "Migrating %s → %s\n", from, to)
	logger.Info("migrate-db starting", "from", from, "to", to)

	var lastPrinted time.Time
	opts := store.MigrateOptions{
		BatchSize:                migrateBatchSize,
		SkipFTSBackfill:          migrateNoFTS,
		AllowNonEmptyDestination: migrateAllowNonEmpty,
		Progress: func(table string, rows int64) {
			if time.Since(lastPrinted) < 500*time.Millisecond {
				return
			}
			lastPrinted = time.Now()
			fmt.Fprintf(os.Stderr, "  %s: %d rows...\r", table, rows)
		},
	}

	ctx, cancel := context.WithCancel(cmd.Context())
	defer cancel()

	stats, err := store.Migrate(ctx, src, dst, opts)
	if err != nil {
		return fmt.Errorf("migrate: %w", err)
	}

	printMigrateStats(stats)

	if hint := attachmentsHint(migrateFromFlag, migrateToFlag); hint != "" {
		fmt.Fprintln(os.Stderr, hint)
	}
	return nil
}

// runMigrateDryRun prints per-table source row counts and does no writes.
// It is only valid for a readable source; the destination is not opened.
func runMigrateDryRun(src *store.Store, from, to string) error {
	fmt.Fprintf(os.Stderr, "[dry-run] Source:      %s\n", from)
	fmt.Fprintf(os.Stderr, "[dry-run] Destination: %s (not opened)\n\n", to)

	stats, err := src.GetStats()
	if err != nil {
		return fmt.Errorf("source stats: %w", err)
	}
	fmt.Printf("  sources:       %d\n", stats.SourceCount)
	fmt.Printf("  conversations: %d\n", stats.ThreadCount)
	fmt.Printf("  messages:      %d\n", stats.MessageCount)
	fmt.Printf("  attachments:   %d\n", stats.AttachmentCount)
	fmt.Printf("  labels:        %d\n", stats.LabelCount)
	fmt.Printf("  source size:   %.2f MB\n",
		float64(stats.DatabaseSize)/(1024*1024))
	fmt.Fprintln(os.Stderr,
		"\nNo changes made. Re-run without --dry-run to migrate.")
	return nil
}

// printMigrateStats writes a human-readable per-table report.
func printMigrateStats(stats *store.MigrateStats) {
	if stats == nil {
		return
	}
	names := make([]string, 0, len(stats.RowsByTable))
	for name := range stats.RowsByTable {
		names = append(names, name)
	}
	sort.Strings(names)

	fmt.Fprintln(os.Stderr, "\nMigration complete:")
	for _, name := range names {
		n := stats.RowsByTable[name]
		if n == 0 {
			continue
		}
		fmt.Fprintf(os.Stderr, "  %-28s %d rows\n", name, n)
	}
	fmt.Fprintf(os.Stderr, "  %-28s %d rows\n", "TOTAL", stats.TotalRows)
	fmt.Fprintf(os.Stderr, "  %-28s %s\n", "ELAPSED",
		stats.Elapsed.Round(time.Millisecond))
}

// attachmentsHint returns a one-line reminder if the migration likely
// changes the physical host or data-dir root and attachments need copying.
// It uses only path cues (no filesystem walking).
func attachmentsHint(from, to string) string {
	fromDir := sqlitePathDir(from)
	toDir := sqlitePathDir(to)
	// If the local SQLite is one side and the other is Postgres (or lives in a
	// different directory), the attachments dir probably needs to move.
	if fromDir == toDir && fromDir != "" {
		return ""
	}
	return "\nReminder: attachments live on disk (~/.msgvault/attachments by default) " +
		"and are NOT copied by this command. If you're moving to a new host or " +
		"a new MSGVAULT_HOME, copy that directory separately."
}

// sqlitePathDir returns the parent directory of a SQLite file path, or ""
// for Postgres URLs and special paths like ":memory:".
func sqlitePathDir(path string) string {
	if strings.HasPrefix(path, "postgres://") ||
		strings.HasPrefix(path, "postgresql://") {
		return ""
	}
	if path == ":memory:" {
		return ""
	}
	return filepath.Dir(path)
}

func init() {
	migrateDBCmd.Flags().StringVar(&migrateFromFlag, "from", "",
		"source database (SQLite file path or postgres:// URL)")
	migrateDBCmd.Flags().StringVar(&migrateToFlag, "to", "",
		"destination database (SQLite file path or postgres:// URL)")
	migrateDBCmd.Flags().IntVar(&migrateBatchSize, "batch-size", 0,
		"rows per INSERT batch (0 = auto: 200 rows, clamped under SQLite param limit)")
	migrateDBCmd.Flags().BoolVar(&migrateDryRun, "dry-run", false,
		"print source row counts and exit without writing to the destination")
	migrateDBCmd.Flags().BoolVar(&migrateNoFTS, "skip-fts-rebuild", false,
		"skip the full-text index rebuild on the destination "+
			"(run 'msgvault repair-encoding' or resync to populate later)")
	migrateDBCmd.Flags().BoolVar(&migrateAllowNonEmpty, "allow-non-empty", false,
		"migrate into a destination that already has data (unsafe — "+
			"IDs and unique constraints are likely to collide)")
	_ = migrateDBCmd.MarkFlagRequired("from")
	_ = migrateDBCmd.MarkFlagRequired("to")
	rootCmd.AddCommand(migrateDBCmd)
}
