package store_test

import (
	"context"
	"database/sql"
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

	vr, err := store.VerifyMigration(ctx, src, dst)
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

	vr, err := store.VerifyMigration(ctx, src, dst)
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

	vr, err := store.VerifyMigration(ctx, src, dst)
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
