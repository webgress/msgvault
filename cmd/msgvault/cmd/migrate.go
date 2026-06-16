package cmd

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
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
	f.IntVar(&migrateBatch, "batch", store.DefaultMigrateBatch, "rows per multi-row INSERT batch")
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

	// Refuse if source and destination resolve to the SAME database. Compare
	// canonically (not raw DSN strings) so aliased identities — a relative vs
	// absolute SQLite path to one file, or two PG DSNs differing only in
	// host/port/query spelling — can never slip past this guard and let
	// --truncate-dest erase the source.
	if same, why := sameDatabase(from.dsn, to.dsn); same {
		return usageErr(cmd, fmt.Errorf(
			"--from and --to resolve to the same database (%s)", why))
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

	// Bring the source up to the current schema before reading it, exactly as
	// every other command that opens an existing vault does (serve, tui, import,
	// …: store.Open followed by InitSchema). A legacy source missing
	// newly-added tables or columns would otherwise fail mid-copy or produce a
	// false verify mismatch against the freshly-initialized destination's
	// defaults. InitSchema is the normal, idempotent open path users already get
	// whenever they touch this vault, so applying it here is not a surprising
	// mutation. (The destination is deliberately NOT init'd until after the
	// populated-dest refusal gate — see below — because its one-shot migrations
	// are destructive; the source has no such gate because it is the vault the
	// user is reading from, not one we might refuse and leave half-migrated.)
	if err := src.InitSchema(); err != nil {
		if src.IsBusyError(err) {
			return errors.New("database is busy — stop 'msgvault serve' and any other clients, then retry")
		}
		return fmt.Errorf("initialize source schema: %w", err)
	}

	// Guard: source must have at least one account.
	var sourceCount int64
	if err := src.DB().QueryRowContext(ctx, "SELECT COUNT(*) FROM sources").Scan(&sourceCount); err != nil {
		return fmt.Errorf("count source sources: %w", err)
	}
	if sourceCount == 0 {
		return errors.New("source database has zero accounts (sources table is empty) — nothing to migrate")
	}

	// Dry run: read source counts and print the plan WITHOUT opening,
	// initializing, or mutating the destination in any way.
	if migrateDryRun {
		fmt.Fprintf(os.Stderr, "Dry run: %s -> %s (no destination changes)\n",
			backendLabel(from.dsn), backendLabel(to.dsn))
		result, err := store.MigrateVault(ctx, src, nil, store.MigrateOptions{
			Batch:    migrateBatch,
			DryRun:   true,
			Progress: migrateProgress(),
		})
		if err != nil {
			if src.IsBusyError(err) {
				return errors.New("database is busy — stop 'msgvault serve' and any other clients, then retry")
			}
			return fmt.Errorf("migrate (dry run): %w", err)
		}
		printMigrateSummary(result)
		return nil
	}

	// Validate ALL preconditions BEFORE any destination mutation (before
	// InitSchema and prepareDest) so a misconfiguration — e.g. an unresolvable
	// attachments dir — can never leave a truncated/initialized destination
	// behind. Attachment-dir resolution is a precondition only when blobs are
	// being copied.
	if copyAttachments {
		if from.attachDir == "" {
			return errors.New("cannot resolve SOURCE attachments directory: pass --from-home, " +
				"point --from at the configured vault, or use --no-attachments to skip blobs")
		}
		if to.attachDir == "" {
			return errors.New("cannot resolve DESTINATION attachments directory: pass --to-home, " +
				"point --to at the configured vault, or use --no-attachments to skip blobs")
		}
	}

	dst, err := store.Open(to.dsn)
	if err != nil {
		return fmt.Errorf("open destination %s: %w", to.dsn, err)
	}
	defer func() { _ = dst.Close() }()

	// Refuse a populated destination BEFORE InitSchema. InitSchema runs
	// DESTRUCTIVE one-shot migrations on a non-empty DB (attachment dedupe
	// DELETE, phone-unique dedupe/merge, default-collection seed), so a
	// destination that will ultimately be refused must never reach it — checking
	// only in prepareDest (post-init) would silently mutate the data we then
	// refuse to overwrite. This probe tolerates a not-yet-created schema (a fresh
	// DB has no tables == empty). prepareDest still handles the post-init
	// baseline-clear / --truncate-dest path; the refusal logic is NOT duplicated
	// there — this gate owns it, prepareDest's matching refusal only ever sees an
	// already-gated (or freshly-initialized) destination.
	if !migrateResume && !migrateTruncateDest {
		nonEmpty, table, err := store.DestHasRealDataPreInit(ctx, dst)
		if err != nil {
			if dst.IsBusyError(err) {
				return errors.New("database is busy — stop 'msgvault serve' and any other clients, then retry")
			}
			return fmt.Errorf("check destination state: %w", err)
		}
		if nonEmpty {
			return usageErr(cmd, fmt.Errorf(
				"destination already has data (table %q is non-empty); "+
					"use --resume to conflict-skip or --truncate-dest to overwrite", table))
		}
	}

	if err := dst.InitSchema(); err != nil {
		return fmt.Errorf("initialize destination schema: %w", err)
	}

	if err := prepareDest(ctx, cmd, dst); err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "Migrating %s -> %s\n", backendLabel(from.dsn), backendLabel(to.dsn))

	result, err := store.MigrateVault(ctx, src, dst, store.MigrateOptions{
		Batch:    migrateBatch,
		Resume:   migrateResume,
		DryRun:   false,
		Progress: migrateProgress(),
	})
	if err != nil {
		if src.IsBusyError(err) || dst.IsBusyError(err) {
			return errors.New("database is busy — stop 'msgvault serve' and any other clients, then retry")
		}
		return fmt.Errorf("migrate: %w", err)
	}

	printMigrateSummary(result)

	// Rebuild FTS on the destination (FTS is backend-specific, never copied).
	if rebuildFTS {
		// RebuildFTS predates context plumbing and runs on context.Background
		// internally (see internal/store/messages.go), so a cancellation that
		// arrives mid-rebuild is NOT honored — a known limitation tracked as a
		// follow-up (refactoring RebuildFTS's signature is out of scope for this
		// feature branch). Guard the common case by aborting BEFORE we start if
		// the context is already cancelled, so a user who Ctrl-C'd during the
		// copy does not then sit through a full FTS rebuild.
		if err := ctx.Err(); err != nil {
			return err
		}
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
		if ar.Missing > 0 {
			fmt.Fprintf(os.Stderr,
				"  WARNING: %d referenced attachment blob(s) missing from source%s\n",
				ar.Missing, sampleSuffix(ar.MissingSample, ar.Missing))
		}
		if ar.Rejected > 0 {
			fmt.Fprintf(os.Stderr,
				"  WARNING: %d attachment path(s) rejected as unsafe%s\n",
				ar.Rejected, sampleSuffix(ar.RejectedSample, ar.Rejected))
		}
	}

	// Vectors: never copied across backends. Surface the re-embed instruction.
	if migrateVectors == "reembed" {
		fmt.Fprintln(os.Stderr,
			"Vectors: run 'msgvault embeddings build' against the destination to re-embed.")
	}

	if migrateVerify {
		fmt.Fprintln(os.Stderr, "Verifying migration...")
		// expectFTS mirrors whether we rebuilt FTS above, so verify only asserts
		// FTS readiness when a rebuild was actually requested.
		vr, err := store.VerifyMigration(ctx, src, dst, rebuildFTS)
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

// sameDatabase reports whether two DSNs canonically resolve to the same
// database, returning a human description for the error message. It is the
// load-bearing guard against --truncate-dest erasing the source via an aliased
// identity (relative vs absolute SQLite path; differently-spelled PG DSNs).
//
//   - Both SQLite: strip any file: scheme + query string, then
//     filepath.Abs + filepath.EvalSymlinks; equal resolved paths mean the same
//     file. As a belt-and-suspenders, when both files exist, os.SameFile on
//     their FileInfos catches hardlinks / case-insensitive FS aliases the
//     string compare would miss.
//   - Both PostgreSQL: parse both URLs, normalize scheme to postgres, lowercase
//     host, default the port to 5432, and compare (host, port, dbname) while
//     ignoring other query params (sslmode, search_path, etc.).
//   - Mixed backends are never the same database.
func sameDatabase(a, b string) (bool, string) {
	aPG, bPG := store.IsPostgresURL(a), store.IsPostgresURL(b)
	if aPG != bPG {
		return false, ""
	}
	if aPG {
		return samePostgres(a, b)
	}
	return sameSQLite(a, b)
}

// sameSQLite canonicalizes two SQLite DSNs to absolute, symlink-resolved paths
// and compares them, also using os.SameFile when both files exist.
func sameSQLite(a, b string) (bool, string) {
	pa := canonicalSQLitePath(a)
	pb := canonicalSQLitePath(b)
	if pa == pb {
		return true, pa
	}
	// Belt-and-suspenders: identical inode even if the resolved strings differ
	// (e.g. case-insensitive filesystem, hardlink).
	if ia, err := os.Stat(pa); err == nil {
		if ib, err := os.Stat(pb); err == nil && os.SameFile(ia, ib) {
			return true, pa
		}
	}
	return false, ""
}

// canonicalSQLitePath reduces a SQLite DSN to a canonical filesystem path:
// resolve any file: URI to its filesystem path, drop the query string, make
// absolute, then resolve symlinks where possible. Best-effort: on any error it
// falls back to the most-resolved form it has so far (never returns "").
//
// file: DSNs are parsed as proper URIs (the SQLite driver opens them with
// SQLITE_OPEN_URI), so every spelling that addresses one file canonicalizes
// equal:
//   - file:relative           -> relative (opaque path, no authority)
//   - file:/abs               -> /abs
//   - file:///abs             -> /abs (empty authority)
//   - file://localhost/abs    -> /abs (localhost authority == local machine)
//
// The path is percent-decoded (so "file:/tmp/a%20b.db" addresses "/tmp/a b.db")
// and any ?query (e.g. "?mode=ro") is dropped. Without proper parsing, a
// file://host/path alias to the source would slip past the same-database guard
// and let --truncate-dest erase it. A literal plain path containing "%20" (no
// file: scheme) is NOT decoded — it stays a literal path.
func canonicalSQLitePath(dsn string) string {
	p := dsn
	if isFileURIDSN(dsn) {
		p = fileURIPath(dsn)
	}
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		p = resolved
	}
	return p
}

// isFileURIDSN reports whether a DSN uses the SQLite file: URI scheme. The
// scheme is case-insensitive per RFC 3986, matching how SQLite recognizes it.
func isFileURIDSN(dsn string) bool {
	return len(dsn) >= 5 && strings.EqualFold(dsn[:5], "file:")
}

// fileURIPath extracts the filesystem path from a SQLite file: URI, handling the
// authority ("localhost" or empty), opaque (relative) paths, percent-decoding,
// and the ?query suffix. It never returns "": on any parse failure it degrades
// to a best-effort manual strip so the caller still gets a usable path to
// compare.
func fileURIPath(dsn string) string {
	u, err := url.Parse(dsn)
	if err != nil {
		// Unparseable: fall back to a manual strip (scheme + optional authority +
		// query) so the guard still has something to compare.
		return fileURIPathManual(dsn)
	}
	// SQLite accepts an empty authority (file:///abs) or "localhost"
	// (file://localhost/abs); both denote the local machine. A relative DSN
	// (file:relative or file:./relative) parses as an opaque path in u.Opaque.
	if u.Opaque != "" {
		// file:relative — u.Opaque holds the (still percent-encoded) path.
		if dec, derr := url.PathUnescape(u.Opaque); derr == nil {
			return dec
		}
		return u.Opaque
	}
	// u.Path is already percent-decoded by url.Parse. The query (u.RawQuery) is
	// intentionally ignored.
	return u.Path
}

// fileURIPathManual is the parse-failure fallback for fileURIPath: strip the
// file: scheme, an optional //authority, and any ?query, then percent-decode.
func fileURIPathManual(dsn string) string {
	p := dsn
	for _, prefix := range []string{"file://", "file:"} {
		if rest, ok := strings.CutPrefix(p, prefix); ok {
			p = rest
			// For the //authority form, drop a leading "localhost" or empty
			// authority component up to the next slash so file://localhost/abs
			// and file:///abs both reduce to /abs.
			if prefix == "file://" {
				if slash := strings.IndexByte(p, '/'); slash >= 0 {
					p = p[slash:]
				}
			}
			break
		}
	}
	if i := strings.IndexByte(p, '?'); i >= 0 {
		p = p[:i]
	}
	if dec, err := url.PathUnescape(p); err == nil {
		p = dec
	}
	return p
}

// samePostgres compares two PostgreSQL DSNs on (host, port, dbname) after
// normalizing scheme, host case, and the default port. Other query parameters
// (sslmode, search_path, application_name, …) are ignored. Unparseable DSNs
// fall back to an exact string compare so a genuinely-identical DSN is still
// caught.
func samePostgres(a, b string) (bool, string) {
	ka, oka := pgIdentity(a)
	kb, okb := pgIdentity(b)
	if !oka || !okb {
		if a == b {
			return true, a
		}
		return false, ""
	}
	if ka == kb {
		return true, ka
	}
	return false, ""
}

// pgIdentity returns a canonical "host:port/dbname" identity for a PG DSN and
// whether it parsed. Scheme is normalized to postgres, host lowercased, port
// defaulted to 5432, and the leading slash dropped from the db name.
func pgIdentity(dsn string) (string, bool) {
	u, err := url.Parse(dsn)
	if err != nil {
		return "", false
	}
	host := strings.ToLower(u.Hostname())
	port := u.Port()
	if port == "" {
		port = "5432"
	}
	db := strings.TrimPrefix(u.Path, "/")
	return host + ":" + port + "/" + db, true
}

// sampleSuffix renders a parenthetical list of example paths for a warning
// line, indicating truncation when fewer samples than total are shown.
func sampleSuffix(sample []string, total int64) string {
	if len(sample) == 0 {
		return ""
	}
	s := " (e.g. " + strings.Join(sample, ", ")
	if int64(len(sample)) < total {
		s += ", …"
	}
	return s + ")"
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
