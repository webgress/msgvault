package store_test

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

// populated describes the fixture a buildSourceVault produces, so assertions
// can be written against known ids/values without re-querying the source.
type populated struct {
	sourceID    int64
	convID      int64
	aliceID     int64
	bobID       int64
	rootMsgID   int64
	replyMsgID  int64
	labelID     int64
	storagePath string
}

// buildSourceVault populates st with a small but representative archive that
// exercises every migrated table that a bidirectional copy must preserve:
// sources, participants (+identifiers), conversations, messages with a reply
// chain, recipients, attachments, bodies, raw blob, labels (+links),
// account_identities, sync_runs, and collections. Works on either backend.
func buildSourceVault(t *testing.T, st *store.Store) populated {
	t.Helper()
	require := require.New(t)

	src, err := st.GetOrCreateSource("gmail", "alice@example.com")
	require.NoError(err, "GetOrCreateSource")

	// Set a JSON config so the JSON/JSONB bridging path is exercised. The
	// source here is always SQLite (JSON is TEXT), so a plain string bind is
	// correct; the cross-backend copy applies the ?::JSONB cast on a PG dest.
	_, err = st.DB().Exec(
		st.Rebind("UPDATE sources SET sync_config = ? WHERE id = ?"),
		`{"label":"all"}`, src.ID)
	require.NoError(err, "set sync_config")

	convID, err := st.EnsureConversation(src.ID, "thread-1", "Hello")
	require.NoError(err, "EnsureConversation")

	aliceID := mustParticipant(t, st, "alice@example.com", "Alice", "example.com")
	bobID := mustParticipant(t, st, "bob@example.com", "Bob", "example.com")

	rootID, err := st.UpsertMessage(&store.Message{
		ConversationID:  convID,
		SourceID:        src.ID,
		SourceMessageID: "m1",
		MessageType:     "email",
		SenderID:        sql.NullInt64{Int64: aliceID, Valid: true},
		IsFromMe:        true,
		Subject:         sql.NullString{String: "Root msg", Valid: true},
		HasAttachments:  true,
		AttachmentCount: 1,
		SentAt:          sql.NullTime{Time: time.Date(2024, 1, 1, 10, 0, 0, 0, time.UTC), Valid: true},
	})
	require.NoError(err, "UpsertMessage root")

	replyID, err := st.UpsertMessage(&store.Message{
		ConversationID:  convID,
		SourceID:        src.ID,
		SourceMessageID: "m2",
		MessageType:     "email",
		SenderID:        sql.NullInt64{Int64: bobID, Valid: true},
		Subject:         sql.NullString{String: "Re: Root", Valid: true},
		SentAt:          sql.NullTime{Time: time.Date(2024, 1, 1, 11, 0, 0, 0, time.UTC), Valid: true},
	})
	require.NoError(err, "UpsertMessage reply")

	// Self-FK reply chain (no public setter; direct UPDATE is backend-agnostic
	// via Rebind).
	_, err = st.DB().Exec(
		st.Rebind("UPDATE messages SET reply_to_message_id = ? WHERE id = ?"),
		rootID, replyID)
	require.NoError(err, "set reply_to_message_id")

	require.NoError(st.UpsertMessageBody(rootID,
		sql.NullString{String: "hello world body", Valid: true},
		sql.NullString{String: "<p>hello</p>", Valid: true}), "body root")
	require.NoError(st.UpsertMessageBody(replyID,
		sql.NullString{String: "reply body invoice", Valid: true},
		sql.NullString{}), "body reply")

	require.NoError(st.UpsertMessageRaw(rootID, []byte{0x1, 0x2, 0x3, 0x4}), "raw root")

	require.NoError(st.ReplaceMessageRecipients(rootID, "to",
		[]int64{bobID}, []string{"Bob"}), "recipients root")
	require.NoError(st.ReplaceMessageRecipients(replyID, "to",
		[]int64{aliceID}, []string{"Alice"}), "recipients reply")

	const storagePath = "ab/abc123def456"
	require.NoError(st.UpsertAttachment(rootID, "a.pdf", "application/pdf",
		storagePath, "abc123def456", 5), "attachment")

	labelID, err := st.EnsureLabel(src.ID, "INBOX", "INBOX", "system")
	require.NoError(err, "EnsureLabel")
	require.NoError(st.AddMessageLabels(rootID, []int64{labelID}), "AddMessageLabels")

	require.NoError(st.AddAccountIdentity(src.ID, "alice@example.com", "manual"),
		"AddAccountIdentity")

	_, err = st.StartSync(src.ID, "full")
	require.NoError(err, "StartSync")

	return populated{
		sourceID:    src.ID,
		convID:      convID,
		aliceID:     aliceID,
		bobID:       bobID,
		rootMsgID:   rootID,
		replyMsgID:  replyID,
		labelID:     labelID,
		storagePath: storagePath,
	}
}

func mustParticipant(t *testing.T, st *store.Store, email, name, domain string) int64 {
	t.Helper()
	id, err := st.EnsureParticipant(email, name, domain)
	require.NoError(t, err, "EnsureParticipant "+email)
	return id
}

// newSQLiteStore opens a fresh on-disk SQLite store with an initialized schema.
func newSQLiteStore(t *testing.T) *store.Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "vault.db")
	st, err := store.OpenForTest(path)
	require.NoError(t, err, "open sqlite store")
	t.Cleanup(func() { _ = st.Close() })
	require.NoError(t, st.InitSchema(), "init schema")
	return st
}

// TestMigrateTableOrderCoversFKGraph asserts the copy order lists every data
// table exactly once and that every parent appears before its children for the
// FK edges that matter (no remapping means children must land after parents).
func TestMigrateTableOrderCoversFKGraph(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	order := store.MigrateTableOrderForTest()
	seen := map[string]int{}
	for i, tbl := range order {
		_, dup := seen[tbl]
		require.Falsef(dup, "table %q listed twice", tbl)
		seen[tbl] = i
	}

	// Parent -> child edges that must be ordered.
	edges := [][2]string{
		{"sources", "conversations"},
		{"sources", "messages"},
		{"sources", "labels"},
		{"participants", "participant_identifiers"},
		{"participants", "conversation_participants"},
		{"conversations", "conversation_participants"},
		{"conversations", "messages"},
		{"messages", "message_recipients"},
		{"messages", "reactions"},
		{"messages", "attachments"},
		{"messages", "message_labels"},
		{"labels", "message_labels"},
		{"messages", "message_bodies"},
		{"messages", "message_raw"},
		{"sources", "sync_runs"},
		{"sources", "sync_checkpoints"},
		{"sources", "source_import_items"},
		{"collections", "collection_sources"},
		{"sources", "collection_sources"},
		{"sources", "account_identities"},
	}
	for _, e := range edges {
		pi, ok1 := seen[e[0]]
		ci, ok2 := seen[e[1]]
		require.Truef(ok1, "parent %q missing from order", e[0])
		require.Truef(ok2, "child %q missing from order", e[1])
		assert.Lessf(pi, ci, "%q must be copied before %q", e[0], e[1])
	}
}

// TestMigrateSQLiteToSQLite is the core round-trip: a populated SQLite vault
// migrated into a fresh SQLite vault preserves every id, the reply chain, JSON,
// bool, time, and []byte values, and passes verification.
func TestMigrateSQLiteToSQLite(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := context.Background()

	src := newSQLiteStore(t)
	dst := newSQLiteStore(t)
	want := buildSourceVault(t, src)
	clearDefaultCollection(t, dst)

	res, err := store.MigrateVault(ctx, src, dst, store.MigrateOptions{Batch: 2})
	require.NoError(err, "MigrateVault")
	assert.Positive(res.RowsCopied(), "rows copied")
	rebuildFTSForTest(t, dst)

	// IDs preserved verbatim.
	assert.Equal(want.rootMsgID, scanInt(t, dst, "SELECT id FROM messages WHERE source_message_id = 'm1'"))
	assert.Equal(want.replyMsgID, scanInt(t, dst, "SELECT id FROM messages WHERE source_message_id = 'm2'"))

	// Two-pass self-FK applied.
	assert.Equal(want.rootMsgID,
		scanInt(t, dst, "SELECT reply_to_message_id FROM messages WHERE source_message_id = 'm2'"))
	assert.Equal(int64(0),
		scanNullInt(t, dst, "SELECT reply_to_message_id FROM messages WHERE source_message_id = 'm1'"))

	// JSON / bool / time / bytes survive.
	assert.Equal("hello world body",
		scanString(t, dst, "SELECT body_text FROM message_bodies WHERE message_id = ?", want.rootMsgID))
	assert.True(scanBool(t, dst, "SELECT is_from_me FROM messages WHERE id = ?", want.rootMsgID))
	assert.False(scanBool(t, dst, "SELECT is_from_me FROM messages WHERE id = ?", want.replyMsgID))
	// raw_data is stored zlib-compressed; the copy must preserve the stored
	// bytes verbatim, so compare dest against what the source actually holds.
	srcRaw := scanBytes(t, src, "SELECT raw_data FROM message_raw WHERE message_id = ?", want.rootMsgID)
	dstRaw := scanBytes(t, dst, "SELECT raw_data FROM message_raw WHERE message_id = ?", want.rootMsgID)
	assert.NotEmpty(dstRaw, "raw blob copied")
	assert.Equal(srcRaw, dstRaw, "raw blob bytes preserved verbatim")

	vr, err := store.VerifyMigration(ctx, src, dst, true)
	require.NoError(err, "VerifyMigration")
	assert.Truef(vr.OK(), "verify problems: %v", vr.Problems)
}

// TestMigrateResumeIdempotent runs the copy twice with --resume; row counts must
// be stable (the second run conflict-skips every row).
func TestMigrateResumeIdempotent(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := context.Background()

	src := newSQLiteStore(t)
	dst := newSQLiteStore(t)
	buildSourceVault(t, src)
	clearDefaultCollection(t, dst)

	_, err := store.MigrateVault(ctx, src, dst, store.MigrateOptions{Batch: 100, Resume: true})
	require.NoError(err, "first migrate")
	firstMsgs := scanInt(t, dst, "SELECT COUNT(*) FROM messages")

	_, err = store.MigrateVault(ctx, src, dst, store.MigrateOptions{Batch: 100, Resume: true})
	require.NoError(err, "second migrate")
	secondMsgs := scanInt(t, dst, "SELECT COUNT(*) FROM messages")

	assert.Equal(firstMsgs, secondMsgs, "message count stable across resume re-run")
	rebuildFTSForTest(t, dst)

	vr, err := store.VerifyMigration(ctx, src, dst, true)
	require.NoError(err, "VerifyMigration")
	assert.Truef(vr.OK(), "verify problems: %v", vr.Problems)
}

// TestVerifyCatchesMismatch injects an extra destination row and asserts
// VerifyMigration flags the row-count divergence.
func TestVerifyCatchesMismatch(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := context.Background()

	src := newSQLiteStore(t)
	dst := newSQLiteStore(t)
	buildSourceVault(t, src)
	clearDefaultCollection(t, dst)

	_, err := store.MigrateVault(ctx, src, dst, store.MigrateOptions{Batch: 100})
	require.NoError(err, "MigrateVault")

	// Inject an extra participant on the destination only.
	_, err = dst.EnsureParticipant("carol@example.com", "Carol", "example.com")
	require.NoError(err, "inject participant")

	vr, err := store.VerifyMigration(ctx, src, dst, false)
	require.NoError(err, "VerifyMigration")
	assert.False(vr.OK(), "verify should fail on injected mismatch")
	assert.NotEmpty(vr.Problems, "expected a recorded problem")
	found := slices.ContainsFunc(vr.Problems, func(p string) bool {
		return strings.Contains(p, "participants")
	})
	assert.True(found, "expected a participants problem, got: %v", vr.Problems)
}

// TestMigrateDryRunWritesNothing asserts a dry run reports source counts but
// leaves the destination empty.
func TestMigrateDryRunWritesNothing(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := context.Background()

	src := newSQLiteStore(t)
	dst := newSQLiteStore(t)
	buildSourceVault(t, src)

	res, err := store.MigrateVault(ctx, src, dst, store.MigrateOptions{DryRun: true})
	require.NoError(err, "dry run")
	assert.True(res.DryRun, "result flagged dry run")
	assert.Positive(res.RowsCopied(), "dry run reports source counts")

	// No messages written to destination.
	assert.Equal(int64(0), scanInt(t, dst, "SELECT COUNT(*) FROM messages"))
}

// TestCopyAttachmentsRoundTrip copies blobs between temp dirs and asserts
// idempotent skip-on-rerun.
func TestCopyAttachmentsRoundTrip(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := context.Background()

	src := newSQLiteStore(t)
	buildSourceVault(t, src)

	srcDir := t.TempDir()
	dstDir := t.TempDir()
	want := []byte("PDFDATA")
	testutil.WriteFile(t, srcDir, "ab/abc123def456", want)

	res, err := store.CopyAttachments(ctx, src, srcDir, dstDir)
	require.NoError(err, "CopyAttachments")
	assert.Equal(int64(1), res.Copied, "one blob copied")
	got := testutil.ReadFile(t, filepath.Join(dstDir, "ab/abc123def456"))
	assert.Equal(want, got, "blob bytes preserved")

	// Re-run: idempotent skip.
	res2, err := store.CopyAttachments(ctx, src, srcDir, dstDir)
	require.NoError(err, "CopyAttachments rerun")
	assert.Equal(int64(0), res2.Copied, "no re-copy")
	assert.Equal(int64(1), res2.Skipped, "skipped existing blob")
}

// TestCopyAttachmentsUnresolvedDirErrors asserts an empty dir is a hard error
// (never a silent drop).
func TestCopyAttachmentsUnresolvedDirErrors(t *testing.T) {
	require := require.New(t)
	ctx := context.Background()
	src := newSQLiteStore(t)
	buildSourceVault(t, src)

	_, err := store.CopyAttachments(ctx, src, "", t.TempDir())
	require.Error(err, "empty source dir must error")
	_, err = store.CopyAttachments(ctx, src, t.TempDir(), "")
	require.Error(err, "empty dest dir must error")
}

// TestCopyAttachmentsRejectsUnsafePaths (H3) asserts that a DB-supplied path
// that would traverse outside the destination dir is skipped-and-counted, never
// copied. The traversal target outside dstDir must NOT be created.
func TestCopyAttachmentsRejectsUnsafePaths(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := context.Background()

	src := newSQLiteStore(t)
	pop := buildSourceVault(t, src)

	srcDir := t.TempDir()
	dstParent := t.TempDir()
	dstDir := filepath.Join(dstParent, "dest")
	require.NoError(os.MkdirAll(dstDir, 0o755), "make dstDir")

	// Inject an attachment whose storage_path traverses out of the dest dir.
	// (UpsertAttachment stores the path verbatim; no validation at write time.)
	require.NoError(src.UpsertAttachment(pop.rootMsgID, "evil.pdf", "application/pdf",
		"../escape.pdf", "deadbeef", 4), "inject traversal path")
	// Provide a source file at the traversal location so only the path check —
	// not a missing-file skip — governs the outcome.
	require.NoError(os.WriteFile(filepath.Join(srcDir, "escape.pdf"), []byte("evil"), 0o600),
		"write would-be source")

	res, err := store.CopyAttachments(ctx, src, srcDir, dstDir)
	require.NoError(err, "CopyAttachments")
	assert.Positive(res.Rejected, "traversal path counted as rejected")
	assert.NotEmpty(res.RejectedSample, "rejected sample recorded")

	// The traversal target outside dstDir must not exist.
	_, statErr := os.Stat(filepath.Join(dstParent, "escape.pdf"))
	assert.True(os.IsNotExist(statErr), "traversal target must not be written")
}

// TestCopyAttachmentsRejectsSymlinkEscape (F4) asserts symlink-aware
// containment: a symlink planted INSIDE the destination dir that redirects a
// content-addressed subdir out of the tree must be rejected, not followed. The
// path is lexically clean ("ab/abc123def456" has no ".."), so only the
// symlink-resolving check can catch the escape.
func TestCopyAttachmentsRejectsSymlinkEscape(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := context.Background()

	src := newSQLiteStore(t)
	buildSourceVault(t, src) // references "ab/abc123def456"

	srcDir := t.TempDir()
	dstParent := t.TempDir()
	dstDir := filepath.Join(dstParent, "dest")
	require.NoError(os.MkdirAll(dstDir, 0o755), "make dstDir")

	// outside is a sibling of dstDir (NOT under it). Plant a symlink at
	// dstDir/ab -> outside, so dstDir/ab/abc123def456 resolves to
	// outside/abc123def456 — outside the destination tree.
	outside := filepath.Join(dstParent, "outside")
	require.NoError(os.MkdirAll(outside, 0o755), "make outside dir")
	require.NoError(os.Symlink(outside, filepath.Join(dstDir, "ab")), "plant escaping symlink")

	// Provide the source blob so a missing-file skip does not pre-empt the path
	// check (the fixture references ab/abc123def456 but never writes it).
	testutil.WriteFile(t, srcDir, "ab/abc123def456", []byte("PDFDATA"))

	res, err := store.CopyAttachments(ctx, src, srcDir, dstDir)
	require.NoError(err, "CopyAttachments")
	assert.Positive(res.Rejected, "symlink-escaping path counted as rejected")
	assert.Equal(int64(0), res.Copied, "nothing copied through the escaping symlink")

	// The escape target outside the dest tree must NOT have been written.
	_, statErr := os.Stat(filepath.Join(outside, "abc123def456"))
	assert.True(os.IsNotExist(statErr), "blob must not be written outside the dest tree")
}

// TestCopyAttachmentsCountsMissing (M2) asserts a blob referenced in the DB but
// absent on disk is counted as Missing (with a sample) instead of silently
// skipped.
func TestCopyAttachmentsCountsMissing(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := context.Background()

	src := newSQLiteStore(t)
	buildSourceVault(t, src) // references ab/abc123def456, never written to disk

	srcDir := t.TempDir() // intentionally empty
	dstDir := t.TempDir()

	res, err := store.CopyAttachments(ctx, src, srcDir, dstDir)
	require.NoError(err, "CopyAttachments")
	assert.Equal(int64(0), res.Copied, "nothing copied")
	assert.Positive(res.Missing, "missing blob counted")
	assert.NotEmpty(res.MissingSample, "missing sample recorded")
}

// TestCopyAttachmentsReCopiesOnContentDivergence (M3) asserts that a dest blob
// with a MATCHING SIZE but DIFFERENT content is not skipped — the copy detects
// the content divergence (via hash) and overwrites with the source bytes.
func TestCopyAttachmentsReCopiesOnContentDivergence(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := context.Background()

	src := newSQLiteStore(t)
	buildSourceVault(t, src) // attachment rel path "ab/abc123def456"

	srcDir := t.TempDir()
	dstDir := t.TempDir()
	const rel = "ab/abc123def456"
	want := []byte("AAAAAAA") // 7 bytes
	corrupt := []byte("BBBBBBB")
	require.Len(corrupt, len(want), "fixture sizes must match for the test")
	testutil.WriteFile(t, srcDir, rel, want)
	testutil.WriteFile(t, dstDir, rel, corrupt) // same size, different content

	res, err := store.CopyAttachments(ctx, src, srcDir, dstDir)
	require.NoError(err, "CopyAttachments")
	assert.Equal(int64(1), res.Copied, "diverged blob re-copied")
	assert.Equal(int64(0), res.Skipped, "must not skip on size-only match")
	got := testutil.ReadFile(t, filepath.Join(dstDir, rel))
	assert.Equal(want, got, "dest now holds source bytes")
}

// TestCopyAttachmentsCancelled (L1) asserts the copy honors a cancelled context.
func TestCopyAttachmentsCancelled(t *testing.T) {
	require := require.New(t)
	src := newSQLiteStore(t)
	buildSourceVault(t, src)

	srcDir := t.TempDir()
	dstDir := t.TempDir()
	testutil.WriteFile(t, srcDir, "ab/abc123def456", []byte("PDFDATA"))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := store.CopyAttachments(ctx, src, srcDir, dstDir)
	require.ErrorIs(err, context.Canceled, "cancelled copy must return context error")
}

// TestVerifyCatchesContentDivergence (H4) mutates the destination so a table's
// id-SET differs from the source while row count AND MIN/MAX id stay identical.
// The old count + id-range checks would pass; the new full id-set content hash
// must catch it. message_recipients is used because nothing references its id,
// so its id can be re-keyed freely under foreign_keys=ON.
func TestVerifyCatchesContentDivergence(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := context.Background()

	src := newSQLiteStore(t)
	dst := newSQLiteStore(t)
	pop := buildSourceVault(t, src)
	carolID := mustParticipant(t, src, "carol@example.com", "Carol", "example.com")

	// Give message_recipients a sparse id-set on the SOURCE (ids 10, 20, 30) so
	// the destination can later fill an interior hole with a DIFFERENT id while
	// keeping count/MIN/MAX identical. Each uses a DISTINCT participant to
	// satisfy UNIQUE(message_id, participant_id, recipient_type).
	pids := []int64{pop.aliceID, pop.bobID, carolID}
	for i, id := range []int64{10, 20, 30} {
		_, err := src.DB().Exec(
			"INSERT INTO message_recipients (id, message_id, participant_id, recipient_type, display_name) "+
				"VALUES (?,?,?,?,?)",
			id, pop.rootMsgID, pids[i], "cc", "X")
		require.NoError(err, "seed source recipient")
	}
	// Remove the recipients buildSourceVault added so the id-set is exactly
	// {10,20,30} and the count is predictable.
	_, err := src.DB().Exec("DELETE FROM message_recipients WHERE id NOT IN (10,20,30)")
	require.NoError(err, "trim seeded recipients")

	clearDefaultCollection(t, dst)
	_, err = store.MigrateVault(ctx, src, dst, store.MigrateOptions{Batch: 100})
	require.NoError(err, "MigrateVault")

	var srcCount, srcMin, srcMax int64
	require.NoError(src.DB().QueryRow("SELECT COUNT(*),MIN(id),MAX(id) FROM message_recipients").
		Scan(&srcCount, &srcMin, &srcMax), "src stats")
	require.Equal(int64(3), srcCount, "source has 3 recipients")
	require.Equal(int64(10), srcMin)
	require.Equal(int64(30), srcMax)

	// On the destination, re-key the interior row id=20 -> id=15. Count stays 3,
	// MIN stays 10, MAX stays 30, but the id-SET diverges ({10,15,30}).
	_, err = dst.DB().Exec("UPDATE message_recipients SET id = 15 WHERE id = 20")
	require.NoError(err, "rekey interior recipient")

	var dstCount, dstMin, dstMax int64
	require.NoError(dst.DB().QueryRow("SELECT COUNT(*),MIN(id),MAX(id) FROM message_recipients").
		Scan(&dstCount, &dstMin, &dstMax), "dst stats")
	require.Equal(srcCount, dstCount, "count unchanged")
	require.Equal(srcMin, dstMin, "min unchanged")
	require.Equal(srcMax, dstMax, "max unchanged")

	vr, err := store.VerifyMigration(ctx, src, dst, false)
	require.NoError(err, "VerifyMigration")
	assert.False(vr.OK(), "verify must fail on diverged id-set")
	found := slices.ContainsFunc(vr.Problems, func(p string) bool {
		return strings.Contains(p, "message_recipients") && strings.Contains(p, "content hash")
	})
	assert.Truef(found, "expected a message_recipients content-hash problem, got: %v", vr.Problems)
}

// TestVerifyCatchesNonIDColumnCorruption (F1) mutates a NON-id column on a
// keyed table — message_recipients.display_name — leaving row count AND the
// full id-set identical. The id-set hash and id-range checks all pass; only the
// new per-table CONTENT hash (which spans every column, not just the id set)
// catches the corruption.
func TestVerifyCatchesNonIDColumnCorruption(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := context.Background()

	src := newSQLiteStore(t)
	dst := newSQLiteStore(t)
	buildSourceVault(t, src)
	clearDefaultCollection(t, dst)

	_, err := store.MigrateVault(ctx, src, dst, store.MigrateOptions{Batch: 100})
	require.NoError(err, "MigrateVault")

	// Corrupt a non-id column on the destination only. Counts and the entire
	// id-set are untouched, so every pre-existing check passes.
	res, err := dst.DB().Exec(
		"UPDATE message_recipients SET display_name = 'CORRUPTED' WHERE id = (SELECT MIN(id) FROM message_recipients)")
	require.NoError(err, "corrupt display_name")
	n, _ := res.RowsAffected()
	require.Equal(int64(1), n, "exactly one recipient corrupted")

	// Sanity: counts and id-set still match (so only the content hash can fail).
	var sc, sMin, sMax, dc, dMin, dMax int64
	require.NoError(src.DB().QueryRow("SELECT COUNT(*),COALESCE(MIN(id),0),COALESCE(MAX(id),0) FROM message_recipients").Scan(&sc, &sMin, &sMax))
	require.NoError(dst.DB().QueryRow("SELECT COUNT(*),COALESCE(MIN(id),0),COALESCE(MAX(id),0) FROM message_recipients").Scan(&dc, &dMin, &dMax))
	require.Equal(sc, dc, "count unchanged")
	require.Equal(sMin, dMin, "min id unchanged")
	require.Equal(sMax, dMax, "max id unchanged")

	vr, err := store.VerifyMigration(ctx, src, dst, false)
	require.NoError(err, "VerifyMigration")
	assert.False(vr.OK(), "verify must fail on corrupted non-id column")
	found := slices.ContainsFunc(vr.Problems, func(p string) bool {
		return strings.Contains(p, "message_recipients") && strings.Contains(p, "content hash")
	})
	assert.Truef(found, "expected a message_recipients content-hash problem, got: %v", vr.Problems)

	// And the id-set hash on the SAME table must still match — proving the
	// failure came from the content hash, not the id set.
	for _, tv := range vr.Tables {
		if tv.Table == "message_recipients" {
			assert.True(tv.IDHashOK, "id-set hash must still match (corruption is non-id)")
			assert.False(tv.ContentHashOK, "content hash must flag the corruption")
		}
	}
}

// TestVerifyCatchesIdlessJunctionCorruption (F1) corrupts an IDLESS table —
// message_labels, a pure (message_id, label_id) junction with no id column and
// thus no id-set check at all. The old verify gave idless tables only a
// row-count check, so a re-pointed junction row would pass. The per-table
// content hash must catch it.
func TestVerifyCatchesIdlessJunctionCorruption(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := context.Background()

	src := newSQLiteStore(t)
	dst := newSQLiteStore(t)
	pop := buildSourceVault(t, src)
	clearDefaultCollection(t, dst)

	// Add a SECOND label on the source so the destination junction row can be
	// re-pointed to a different (still valid) label while the row COUNT stays 1.
	label2, err := src.EnsureLabel(pop.sourceID, "SENT", "SENT", "system")
	require.NoError(err, "EnsureLabel SENT")

	_, err = store.MigrateVault(ctx, src, dst, store.MigrateOptions{Batch: 100})
	require.NoError(err, "MigrateVault")

	// Re-point the single message_labels row to label2 on the DESTINATION only.
	// Row count stays 1; only the (message_id, label_id) value changes — exactly
	// what an idless-table content hash must detect.
	res, err := dst.DB().Exec("UPDATE message_labels SET label_id = ? WHERE message_id = ?",
		label2, pop.rootMsgID)
	require.NoError(err, "re-point junction row")
	n, _ := res.RowsAffected()
	require.Equal(int64(1), n, "one junction row re-pointed")

	require.Equal(scanInt(t, src, "SELECT COUNT(*) FROM message_labels"),
		scanInt(t, dst, "SELECT COUNT(*) FROM message_labels"),
		"junction row count unchanged (only count check would pass)")

	vr, err := store.VerifyMigration(ctx, src, dst, false)
	require.NoError(err, "VerifyMigration")
	assert.False(vr.OK(), "verify must fail on corrupted idless junction row")
	found := slices.ContainsFunc(vr.Problems, func(p string) bool {
		return strings.Contains(p, "message_labels") && strings.Contains(p, "content hash")
	})
	assert.Truef(found, "expected a message_labels content-hash problem, got: %v", vr.Problems)
}

// TestVerifyExpectFTSFlagsUnavailable (F3) asserts that when the caller expected
// FTS to be rebuilt (expectFTS=true) but the destination index is not ready,
// verify records a problem instead of silently passing. With expectFTS=false
// the same not-ready state is NOT a problem.
func TestVerifyExpectFTSFlagsUnavailable(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := context.Background()

	src := newSQLiteStore(t)
	dst := newSQLiteStore(t)
	buildSourceVault(t, src)
	clearDefaultCollection(t, dst)

	_, err := store.MigrateVault(ctx, src, dst, store.MigrateOptions{Batch: 100})
	require.NoError(err, "MigrateVault")
	// Deliberately do NOT rebuild FTS on the destination, so the index needs a
	// backfill (or is unavailable). The data copy itself is clean.

	// expectFTS=false: FTS readiness is not asserted, so no FTS problem.
	vrNoFTS, err := store.VerifyMigration(ctx, src, dst, false)
	require.NoError(err, "VerifyMigration expectFTS=false")
	for _, p := range vrNoFTS.Problems {
		assert.NotContains(p, "FTS", "no FTS problem when rebuild was not expected: %s", p)
	}

	// expectFTS=true: an unready FTS index is a problem.
	vrFTS, err := store.VerifyMigration(ctx, src, dst, true)
	require.NoError(err, "VerifyMigration expectFTS=true")
	assert.False(vrFTS.OK(), "verify must fail when FTS expected but not ready")
	found := slices.ContainsFunc(vrFTS.Problems, func(p string) bool {
		return strings.Contains(p, "FTS")
	})
	assert.Truef(found, "expected an FTS problem, got: %v", vrFTS.Problems)
}

// TestContentHashIndependentOfPhysicalColumnOrder (G2) proves the per-table
// content hash is canonicalized by column NAME, not physical SELECT * order. Two
// SQLite stores hold IDENTICAL participant data, but store B's participants table
// has its display_name column physically moved to the END (via DROP+ADD COLUMN,
// exactly how a legacy DB that ALTER-appended a column ends up). Their
// rows.Columns() order therefore differs; before the fix the positional hash
// would FALSE-FAIL this common legacy->fresh shape. With name-sorted folding the
// two hashes must be EQUAL — and a real value change must still differ.
func TestContentHashIndependentOfPhysicalColumnOrder(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := context.Background()

	a := newSQLiteStore(t)
	b := newSQLiteStore(t)

	// Identical participant data in both stores.
	seed := func(st *store.Store) {
		_, err := st.DB().Exec(
			"INSERT INTO participants (id, email_address, phone_number, display_name, domain, canonical_id, created_at, updated_at) " +
				"VALUES (1,'alice@example.com',NULL,'Alice','example.com','c1','2024-01-01 00:00:00','2024-01-01 00:00:00')")
		require.NoError(err, "seed participant 1")
		_, err = st.DB().Exec(
			"INSERT INTO participants (id, email_address, phone_number, display_name, domain, canonical_id, created_at, updated_at) " +
				"VALUES (2,'bob@example.com',NULL,'Bob','example.com','c2','2024-01-01 00:00:00','2024-01-01 00:00:00')")
		require.NoError(err, "seed participant 2")
	}
	seed(a)
	seed(b)

	// Confirm the baseline physical column orders are identical, then DIVERGE
	// store B by physically relocating display_name to the end of the table. SQLite
	// DROP COLUMN + ADD COLUMN appends, mirroring a legacy ALTER-appended column.
	require.Equal(physicalColumns(t, a), physicalColumns(t, b),
		"baseline column order identical before reordering")

	_, err := b.DB().Exec("ALTER TABLE participants DROP COLUMN display_name")
	require.NoError(err, "drop display_name on B")
	_, err = b.DB().Exec("ALTER TABLE participants ADD COLUMN display_name TEXT")
	require.NoError(err, "re-add display_name on B")
	_, err = b.DB().Exec("UPDATE participants SET display_name = 'Alice' WHERE id = 1")
	require.NoError(err, "restore display_name 1 on B")
	_, err = b.DB().Exec("UPDATE participants SET display_name = 'Bob' WHERE id = 2")
	require.NoError(err, "restore display_name 2 on B")

	// Physical orders now DIFFER (this is what would break a positional hash).
	colsA := physicalColumns(t, a)
	colsB := physicalColumns(t, b)
	require.NotEqual(colsA, colsB, "physical column order must differ after reorder")
	require.ElementsMatch(colsA, colsB, "same column SET, only order differs")

	hashA, err := store.TableContentHashForTest(ctx, a, "participants")
	require.NoError(err, "hash A")
	hashB, err := store.TableContentHashForTest(ctx, b, "participants")
	require.NoError(err, "hash B")
	assert.Equal(hashA, hashB,
		"content hash must be identical despite differing physical column order")

	// Teeth: a genuine value change still produces a DIFFERENT hash (the
	// canonicalization must not collapse real differences).
	_, err = b.DB().Exec("UPDATE participants SET display_name = 'CHANGED' WHERE id = 1")
	require.NoError(err, "mutate display_name on B")
	hashBChanged, err := store.TableContentHashForTest(ctx, b, "participants")
	require.NoError(err, "hash B after change")
	assert.NotEqual(hashA, hashBChanged,
		"a real value change must still flip the content hash")
}

// physicalColumns returns the participants table's columns in physical
// (CREATE/ALTER) order via PRAGMA table_info, so a test can assert two stores
// have a different physical layout for the same logical schema.
func physicalColumns(t *testing.T, st *store.Store) []string {
	t.Helper()
	rows, err := st.DB().Query("PRAGMA table_info(participants)")
	require.NoError(t, err, "table_info")
	defer func() { _ = rows.Close() }()
	var cols []string
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt sql.NullString
		require.NoError(t, rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk), "scan table_info")
		cols = append(cols, name)
	}
	require.NoError(t, rows.Err(), "iterate table_info")
	return cols
}

// --- small scan helpers (kept local; not assertion wrappers) ---

func scanInt(t *testing.T, st *store.Store, query string) int64 {
	t.Helper()
	var n int64
	err := st.DB().QueryRow(st.Rebind(query)).Scan(&n)
	require.NoError(t, err, "scanInt: "+query)
	return n
}

func scanNullInt(t *testing.T, st *store.Store, query string, args ...any) int64 {
	t.Helper()
	var n sql.NullInt64
	err := st.DB().QueryRow(st.Rebind(query), args...).Scan(&n)
	require.NoError(t, err, "scanNullInt: "+query)
	return n.Int64
}

func scanString(t *testing.T, st *store.Store, query string, args ...any) string {
	t.Helper()
	var s string
	err := st.DB().QueryRow(st.Rebind(query), args...).Scan(&s)
	require.NoError(t, err, "scanString: "+query)
	return s
}

func scanBool(t *testing.T, st *store.Store, query string, args ...any) bool {
	t.Helper()
	var b bool
	err := st.DB().QueryRow(st.Rebind(query), args...).Scan(&b)
	require.NoError(t, err, "scanBool: "+query)
	return b
}

func scanBytes(t *testing.T, st *store.Store, query string, args ...any) []byte {
	t.Helper()
	var b []byte
	err := st.DB().QueryRow(st.Rebind(query), args...).Scan(&b)
	require.NoError(t, err, "scanBytes: "+query)
	return b
}

// rebuildFTSForTest rebuilds the destination FTS index, mirroring what the
// migrate command does after a copy (MigrateVault itself never touches FTS).
func rebuildFTSForTest(t *testing.T, dst *store.Store) {
	t.Helper()
	_, err := dst.RebuildFTS(nil)
	require.NoError(t, err, "RebuildFTS")
}

// clearDefaultCollection removes the InitSchema-seeded default collection from
// dst so a copy preserving the source's collection ids doesn't collide. The
// migrate command does this via prepareDest; tests call MigrateVault directly.
func clearDefaultCollection(t *testing.T, dst *store.Store) {
	t.Helper()
	_, err := dst.DB().Exec("DELETE FROM collection_sources")
	require.NoError(t, err, "clear collection_sources")
	_, err = dst.DB().Exec("DELETE FROM collections")
	require.NoError(t, err, "clear collections")
}
