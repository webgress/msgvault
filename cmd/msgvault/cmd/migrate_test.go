package cmd

import (
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
