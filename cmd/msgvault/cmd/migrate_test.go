package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
)

// resetMigrateFlags restores the package-level migrate flag vars to their
// declared defaults so each test starts clean (cobra would do this per process,
// but these tests drive runMigrate directly).
func resetMigrateFlags() {
	migrateFrom = ""
	migrateTo = ""
	migrateFromHome = ""
	migrateToHome = ""
	migrateAttachments = true
	migrateNoAttach = false
	migrateVectors = "skip"
	migrateDryRun = false
	migrateVerify = false
	migrateResume = false
	migrateBatch = 5000
	migrateRebuildFTS = true
	migrateNoRebuildFTS = false
	migrateTruncateDest = false
}

func TestRunMigrateRejectsMissingTo(t *testing.T) {
	require := requirepkg.New(t)
	resetMigrateFlags()
	err := runMigrate(&cobra.Command{}, nil)
	require.Error(err, "missing --to must error")
	require.Contains(err.Error(), "--to is required")
}

func TestRunMigrateRejectsVectorCopy(t *testing.T) {
	require := requirepkg.New(t)
	resetMigrateFlags()
	migrateTo = "/tmp/does-not-matter.db"
	migrateVectors = "copy"
	err := runMigrate(&cobra.Command{}, nil)
	require.Error(err, "--vectors copy must error")
	require.Contains(err.Error(), "not implemented")
}

func TestRunMigrateRejectsSameFromTo(t *testing.T) {
	require := requirepkg.New(t)
	resetMigrateFlags()
	migrateFrom = "/tmp/same.db"
	migrateTo = "/tmp/same.db"
	err := runMigrate(&cobra.Command{}, nil)
	require.Error(err, "identical from/to must error")
	require.Contains(err.Error(), "same database")
}

// TestRunMigrateRejectsAliasedSQLitePath (H1) asserts the same-DB guard
// canonicalizes paths: a symlink alias (and a relative-vs-absolute spelling) to
// the SAME SQLite file is rejected, never run, so --truncate-dest can't erase
// the source through an alias.
func TestRunMigrateRejectsAliasedSQLitePath(t *testing.T) {
	require := requirepkg.New(t)

	dir := t.TempDir()
	realPath := filepath.Join(dir, "vault.db")
	require.NoError(os.WriteFile(realPath, []byte("x"), 0o600), "create file")
	link := filepath.Join(dir, "alias.db")
	require.NoError(os.Symlink(realPath, link), "symlink alias")

	// Symlink alias: real path vs symlink path resolve to the same inode.
	resetMigrateFlags()
	migrateFrom = realPath
	migrateTo = link
	migrateTruncateDest = true
	err := runMigrate(&cobra.Command{}, nil)
	require.Error(err, "symlink alias must be rejected")
	require.Contains(err.Error(), "same database")

	// Relative-vs-absolute spelling of the same file.
	t.Chdir(dir)
	resetMigrateFlags()
	migrateFrom = realPath // absolute
	migrateTo = "vault.db" // relative to dir
	migrateTruncateDest = true
	err = runMigrate(&cobra.Command{}, nil)
	require.Error(err, "relative/absolute alias must be rejected")
	require.Contains(err.Error(), "same database")
}

// TestRunMigrateRejectsFileURIPercentEncodedAlias (F2) asserts that a plain
// path with a space and a file: URI whose path percent-encodes that same space
// resolve to the SAME SQLite file and are rejected. The SQLite driver opens
// file: DSNs with SQLITE_OPEN_URI and URL-decodes the path, so "%20" addresses
// a literal space — without decoding in the guard, --truncate-dest could erase
// the source through the encoded alias. The guard runs BEFORE opening any file,
// so this holds even when the file does not exist.
func TestRunMigrateRejectsFileURIPercentEncodedAlias(t *testing.T) {
	require := requirepkg.New(t)

	dir := t.TempDir()
	// A path containing a space; the file need NOT exist (the guard is purely
	// string-based for this alias — os.SameFile cannot help when the encoded
	// path fails os.Stat).
	plain := filepath.Join(dir, "a b.db")
	encoded := "file:" + filepath.Join(dir, "a%20b.db")

	resetMigrateFlags()
	migrateFrom = plain
	migrateTo = encoded
	migrateTruncateDest = true
	err := runMigrate(&cobra.Command{}, nil)
	require.Error(err, "file: URI percent-encoded alias must be rejected")
	require.Contains(err.Error(), "same database")

	// And the reverse direction (encoded source, plain dest) is symmetric.
	resetMigrateFlags()
	migrateFrom = encoded
	migrateTo = plain
	migrateTruncateDest = true
	err = runMigrate(&cobra.Command{}, nil)
	require.Error(err, "reverse percent-encoded alias must be rejected")
	require.Contains(err.Error(), "same database")
}

// TestCanonicalSQLitePathDoesNotDecodePlainPath (F2) asserts a LITERAL plain
// path containing "%20" (no file: scheme) is NOT URL-decoded — only the file:
// branch decodes — so a real on-disk file literally named "a%20b.db" stays
// distinct from one named "a b.db".
func TestCanonicalSQLitePathDoesNotDecodePlainPath(t *testing.T) {
	assert := assertpkg.New(t)
	dir := t.TempDir()

	literalPercent := canonicalSQLitePath(filepath.Join(dir, "a%20b.db"))
	withSpace := canonicalSQLitePath(filepath.Join(dir, "a b.db"))
	assert.NotEqual(withSpace, literalPercent,
		"plain path with %%20 must stay literal (only file: URIs decode)")

	// The file: form of the %20 path DOES decode to the space path.
	fileURIDecoded := canonicalSQLitePath("file:" + filepath.Join(dir, "a%20b.db"))
	assert.Equal(withSpace, fileURIDecoded,
		"file: URI %%20 must decode to the literal space path")
}

// TestRunMigratePostgresAliasRejected (H1) asserts two PG DSNs that differ only
// in spelling (scheme, host case, default port, extra query params) but point
// at the same (host,port,dbname) are rejected.
func TestRunMigratePostgresAliasRejected(t *testing.T) {
	require := requirepkg.New(t)
	resetMigrateFlags()
	migrateFrom = "postgres://u:p@DB.example.com/vault?sslmode=disable"
	migrateTo = "postgresql://u:p@db.example.com:5432/vault?search_path=foo"
	err := runMigrate(&cobra.Command{}, nil)
	require.Error(err, "aliased PG DSNs must be rejected")
	require.Contains(err.Error(), "same database")
}

// TestRunMigrateEndToEnd drives a full SQLite->SQLite copy through the command
// entrypoint (with --no-attachments to avoid attachments-dir resolution) and
// asserts the destination is populated and verifies.
func TestRunMigrateEndToEnd(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	savedCfg := cfg
	defer func() { cfg = savedCfg; resetMigrateFlags() }()
	resetMigrateFlags()

	srcDir := t.TempDir()
	dstDir := t.TempDir()
	cfg = &config.Config{HomeDir: srcDir, Data: config.DataConfig{DataDir: srcDir}}

	// Populate the source vault.
	srcPath := filepath.Join(srcDir, "msgvault.db")
	src, err := store.Open(srcPath)
	require.NoError(err, "open source")
	require.NoError(src.InitSchema(), "init source schema")
	s1, err := src.GetOrCreateSource("gmail", "alice@example.com")
	require.NoError(err, "create source account")
	convID, err := src.EnsureConversation(s1.ID, "thread-1", "Hello")
	require.NoError(err, "ensure conversation")
	_, err = src.UpsertMessage(&store.Message{
		ConversationID: convID, SourceID: s1.ID,
		SourceMessageID: "m1", MessageType: "email",
	})
	require.NoError(err, "upsert message")
	require.NoError(src.Close(), "close source")

	dstPath := filepath.Join(dstDir, "msgvault.db")
	migrateFrom = srcPath
	migrateTo = dstPath
	migrateNoAttach = true
	migrateAttachments = false
	migrateVerify = true

	require.NoError(runMigrate(&cobra.Command{}, nil), "runMigrate end-to-end")

	// Destination has the copied message.
	dst, err := store.Open(dstPath)
	require.NoError(err, "open dest")
	defer func() { _ = dst.Close() }()
	var n int64
	require.NoError(dst.DB().QueryRow("SELECT COUNT(*) FROM messages").Scan(&n), "count dest messages")
	assert.Equal(int64(1), n, "destination message copied")
}

// populateSQLiteVault creates a SQLite vault at path with one account + one
// message so it passes the source non-empty guard. Returns the message count.
func populateSQLiteVault(t *testing.T, path string) {
	t.Helper()
	require := requirepkg.New(t)
	st, err := store.Open(path)
	require.NoError(err, "open vault")
	require.NoError(st.InitSchema(), "init schema")
	s1, err := st.GetOrCreateSource("gmail", "alice@example.com")
	require.NoError(err, "create source")
	convID, err := st.EnsureConversation(s1.ID, "thread-1", "Hello")
	require.NoError(err, "ensure conversation")
	_, err = st.UpsertMessage(&store.Message{
		ConversationID: convID, SourceID: s1.ID,
		SourceMessageID: "m1", MessageType: "email",
	})
	require.NoError(err, "upsert message")
	require.NoError(st.Close(), "close vault")
}

// TestRunMigrateTruncateDestAbortsBeforeMutationOnBadAttachDir (H2) asserts that
// with --truncate-dest, attachments enabled, and an UNRESOLVABLE attachments dir
// (source/dest are not the config vault), the command errors BEFORE any
// destination mutation — the pre-populated destination data must remain intact
// (never truncated).
func TestRunMigrateTruncateDestAbortsBeforeMutationOnBadAttachDir(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	savedCfg := cfg
	defer func() { cfg = savedCfg; resetMigrateFlags() }()

	srcDir := t.TempDir()
	dstDir := t.TempDir()
	srcPath := filepath.Join(srcDir, "src.db")
	dstPath := filepath.Join(dstDir, "dst.db")
	populateSQLiteVault(t, srcPath)
	populateSQLiteVault(t, dstPath) // dest has real data we must not lose

	// cfg points at an unrelated home so neither bare DSN matches the config
	// vault — attachment dirs stay unresolved.
	cfg = &config.Config{HomeDir: t.TempDir(), Data: config.DataConfig{DataDir: t.TempDir()}}

	resetMigrateFlags()
	migrateFrom = srcPath
	migrateTo = dstPath
	migrateTruncateDest = true
	// attachments default ON (migrateAttachments=true), so the unresolved dir is
	// a hard precondition failure.

	err := runMigrate(&cobra.Command{}, nil)
	require.Error(err, "must error on unresolved attachments dir")
	assert.Contains(err.Error(), "attachments directory", "error names the attachments dir")

	// The destination must still hold its original data (NOT truncated).
	dst, err := store.Open(dstPath)
	require.NoError(err, "reopen dest")
	defer func() { _ = dst.Close() }()
	var n int64
	require.NoError(dst.DB().QueryRow("SELECT COUNT(*) FROM messages").Scan(&n), "count dest")
	assert.Equal(int64(1), n, "destination data must be intact (not truncated)")
}

// TestRunMigrateDryRunCreatesNothing (M1) asserts a --dry-run to a fresh
// destination path creates NO file (no open/init/seed) and modifies nothing.
func TestRunMigrateDryRunCreatesNothing(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	savedCfg := cfg
	defer func() { cfg = savedCfg; resetMigrateFlags() }()

	srcDir := t.TempDir()
	dstDir := t.TempDir()
	srcPath := filepath.Join(srcDir, "src.db")
	dstPath := filepath.Join(dstDir, "dst.db")
	populateSQLiteVault(t, srcPath)

	cfg = &config.Config{HomeDir: t.TempDir(), Data: config.DataConfig{DataDir: t.TempDir()}}

	resetMigrateFlags()
	migrateFrom = srcPath
	migrateTo = dstPath
	migrateDryRun = true
	// attachments default ON, but a dry run must not even reach attachment-dir
	// resolution — it should succeed without touching the destination.

	require.NoError(runMigrate(&cobra.Command{}, nil), "dry run must succeed")

	// The destination file must NOT have been created.
	_, statErr := os.Stat(dstPath)
	assert.True(os.IsNotExist(statErr), "dry run must not create the destination file")
}
