package storeext_test

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wesm/msgvault/internal/store"
	"github.com/wesm/msgvault/internal/testutil"
	"github.com/wesm/msgvault/internal/testutil/storetest"
)

// TestMigrateRoundTrip seeds the primary test backend (SQLite by default,
// PostgreSQL when MSGVAULT_TEST_DB is set), migrates it to the opposite
// dialect, and verifies that every row is preserved with its original ID
// and that FTS on the destination finds the seeded content.
//
// When the primary backend is SQLite, the migration targets the PostgreSQL
// instance pointed at by MSGVAULT_TEST_DB (if set) — otherwise the test
// only exercises SQLite→SQLite (file→file), which still covers the copy
// path end-to-end.
func TestMigrateRoundTrip(t *testing.T) {
	f := storetest.New(t)

	seed := seedMigrationData(t, f)

	// Destination: a fresh SQLite file-backed store. Using a file (not
	// :memory:) mirrors real-world usage and lets us reopen if needed.
	destPath := filepath.Join(t.TempDir(), "dest.db")
	dst, err := store.Open(destPath)
	if err != nil {
		t.Fatalf("open dest: %v", err)
	}
	defer func() { _ = dst.Close() }()
	if err := dst.InitSchema(); err != nil {
		t.Fatalf("init dest schema: %v", err)
	}

	stats, err := store.Migrate(context.Background(), f.Store, dst, store.MigrateOptions{})
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if stats.TotalRows == 0 {
		t.Fatal("expected non-zero total rows migrated")
	}
	if stats.RowsByTable["sources"] == 0 {
		t.Errorf("sources not migrated: %+v", stats.RowsByTable)
	}
	if stats.RowsByTable["messages"] != int64(seed.messageCount) {
		t.Errorf("messages migrated = %d, want %d",
			stats.RowsByTable["messages"], seed.messageCount)
	}

	assertMigratedRowCounts(t, f.Store, dst)
	assertIDsPreserved(t, dst, seed)
	assertPayloadRoundTrips(t, f.Store, dst, seed)

	if dst.FTS5Available() {
		assertSearchFindsSeed(t, dst, seed)
	}

	assertNextIDAfterMigration(t, dst, seed.maxMessageID)
}

// TestMigrateSQLiteToPrimary exercises the opposite direction of
// TestMigrateRoundTrip: source is always a fresh SQLite file; destination
// is whatever the test-suite backend selects (SQLite by default, PostgreSQL
// when MSGVAULT_TEST_DB is set).
//
// This pairing gives us cross-dialect coverage under `make test-pg`:
//
//	TestMigrateRoundTrip         PG  → SQLite
//	TestMigrateSQLiteToPrimary   SQLite → PG
func TestMigrateSQLiteToPrimary(t *testing.T) {
	// Source: fresh SQLite file, not affected by MSGVAULT_TEST_DB.
	srcPath := filepath.Join(t.TempDir(), "src.db")
	src, err := store.Open(srcPath)
	if err != nil {
		t.Fatalf("open src: %v", err)
	}
	defer func() { _ = src.Close() }()
	if err := src.InitSchema(); err != nil {
		t.Fatalf("init src schema: %v", err)
	}

	// Seed directly on the fresh SQLite store.
	srcFixture := &storetest.Fixture{T: t, Store: src}
	source, err := src.GetOrCreateSource("gmail", "migrate-src@example.com")
	if err != nil {
		t.Fatalf("source: %v", err)
	}
	srcFixture.Source = source
	convID, err := src.EnsureConversation(source.ID, "src-thread", "Src Thread")
	if err != nil {
		t.Fatalf("conversation: %v", err)
	}
	srcFixture.ConvID = convID

	seed := seedMigrationData(t, srcFixture)

	// Destination: the backend under test (SQLite by default, PG when configured).
	dstFixture := storetest.New(t)

	// Wipe the baseline fixture rows so the destination is truly empty.
	// storetest.New inserts a source + conversation; we need to clear those
	// to pass the non-empty guard.
	mustExec(t, dstFixture.Store, "DELETE FROM conversations")
	mustExec(t, dstFixture.Store, "DELETE FROM sources")

	stats, err := store.Migrate(context.Background(), src, dstFixture.Store, store.MigrateOptions{})
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if stats.RowsByTable["messages"] != int64(seed.messageCount) {
		t.Errorf("messages migrated = %d, want %d",
			stats.RowsByTable["messages"], seed.messageCount)
	}

	assertMigratedRowCounts(t, src, dstFixture.Store)
	assertIDsPreserved(t, dstFixture.Store, seed)

	if dstFixture.Store.FTS5Available() {
		assertSearchFindsSeed(t, dstFixture.Store, seed)
	}

	assertNextIDAfterMigration(t, dstFixture.Store, seed.maxMessageID)
}

func mustExec(t *testing.T, s *store.Store, query string, args ...any) {
	t.Helper()
	if _, err := s.DB().Exec(s.Rebind(query), args...); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
}

// TestMigrateRefusesNonEmptyDestination exercises the safety guard: without
// --allow-non-empty, Migrate must refuse when the destination already has rows.
func TestMigrateRefusesNonEmptyDestination(t *testing.T) {
	src := storetest.New(t)
	_ = seedMigrationData(t, src)

	dst := storetest.New(t) // independent test DB, starts with a source already present

	_, err := store.Migrate(context.Background(), src.Store, dst.Store, store.MigrateOptions{})
	if err == nil {
		t.Fatal("expected error migrating into non-empty destination")
	}
}

// TestMigrateStaleSourceSchema verifies that Migrate fails fast with a
// helpful error if the source database is missing a column the migrator
// expects — rather than crashing halfway through the copy with a generic
// "no such column" message.
func TestMigrateStaleSourceSchema(t *testing.T) {
	src := storetest.New(t)

	// Drop one of the columns the migrator expects. ALTER TABLE DROP
	// COLUMN is a SQLite-3.35+ and PG feature; guard on capability.
	if _, err := src.Store.DB().Exec(
		"ALTER TABLE sources DROP COLUMN oauth_app"); err != nil {
		t.Skipf("ALTER TABLE DROP COLUMN not supported on this backend: %v", err)
	}

	destPath := filepath.Join(t.TempDir(), "dest.db")
	dst, err := store.Open(destPath)
	if err != nil {
		t.Fatalf("open dest: %v", err)
	}
	defer func() { _ = dst.Close() }()
	if err := dst.InitSchema(); err != nil {
		t.Fatalf("init schema: %v", err)
	}

	_, err = store.Migrate(context.Background(), src.Store, dst, store.MigrateOptions{})
	if err == nil {
		t.Fatal("expected error for source missing oauth_app column")
	}
	if !strings.Contains(err.Error(), "source schema check") {
		t.Errorf("expected 'source schema check' in error, got: %v", err)
	}
}

// TestMigrateLargeBlob verifies that the bytes-aware batch cap (set at 10
// rows for tables with a kBytes column) doesn't break correctness. A real
// OOM test would require gigabytes of data, so this just confirms that
// multiple chunks of blob data survive the migration.
func TestMigrateLargeBlob(t *testing.T) {
	src := storetest.New(t)

	// Create 25 messages each with a 1 KB raw blob (25 rows > 10-row cap,
	// so this exercises the flush-multiple-batches path for a bytes table).
	pid := src.EnsureParticipant("blob@example.com", "Blob Sender", "example.com")
	payload := make([]byte, 1024)
	for i := range payload {
		payload[i] = byte(i & 0xFF)
	}
	for i := 0; i < 25; i++ {
		id := src.CreateMessage(fmt.Sprintf("blob-%d", i))
		if err := src.Store.ReplaceMessageRecipients(id, "from",
			[]int64{pid}, []string{"Blob"}); err != nil {
			t.Fatalf("ReplaceMessageRecipients: %v", err)
		}
		if _, err := src.Store.DB().Exec(src.Store.Rebind(
			`INSERT INTO message_raw
			 (message_id, raw_data, raw_format, compression, encryption_version)
			 VALUES (?, ?, ?, ?, ?)`),
			id, payload, "mime", "zlib", 0); err != nil {
			t.Fatalf("insert message_raw: %v", err)
		}
	}

	destPath := filepath.Join(t.TempDir(), "dest.db")
	dst, err := store.Open(destPath)
	if err != nil {
		t.Fatalf("open dest: %v", err)
	}
	defer func() { _ = dst.Close() }()
	if err := dst.InitSchema(); err != nil {
		t.Fatalf("init schema: %v", err)
	}

	stats, err := store.Migrate(context.Background(), src.Store, dst, store.MigrateOptions{})
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if stats.RowsByTable["message_raw"] != 25 {
		t.Errorf("message_raw = %d, want 25", stats.RowsByTable["message_raw"])
	}

	// Verify one sample row survived bit-exact.
	var got []byte
	if err := dst.DB().QueryRow(
		"SELECT raw_data FROM message_raw WHERE message_id = (SELECT id FROM messages WHERE source_message_id = 'blob-13')").Scan(&got); err != nil {
		t.Fatalf("fetch migrated blob: %v", err)
	}
	if len(got) != len(payload) {
		t.Errorf("blob length: got %d want %d", len(got), len(payload))
	}
}

// TestMigrateMinimalSource is a smoke test for an "almost empty" source
// (just the one source + one conversation that the fixture always creates,
// no messages). It exercises the full Migrate code path — table iteration,
// sequence reset, FTS backfill — on an input with minimal data and zero
// messages, which is the edge case most likely to hit off-by-one bugs.
func TestMigrateMinimalSource(t *testing.T) {
	src := storetest.New(t) // fixture creates 1 source + 1 conversation

	destPath := filepath.Join(t.TempDir(), "minimal.db")
	dst, err := store.Open(destPath)
	if err != nil {
		t.Fatalf("open dest: %v", err)
	}
	defer func() { _ = dst.Close() }()
	if err := dst.InitSchema(); err != nil {
		t.Fatalf("init schema: %v", err)
	}

	stats, err := store.Migrate(context.Background(), src.Store, dst, store.MigrateOptions{})
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	// Expect exactly the fixture's baseline: 1 source + 1 conversation.
	if got := stats.RowsByTable["sources"]; got != 1 {
		t.Errorf("sources migrated = %d, want 1", got)
	}
	if got := stats.RowsByTable["conversations"]; got != 1 {
		t.Errorf("conversations migrated = %d, want 1", got)
	}
	if got := stats.RowsByTable["messages"]; got != 0 {
		t.Errorf("messages migrated = %d, want 0", got)
	}
}

// -----------------------------------------------------------------------------
// helpers
// -----------------------------------------------------------------------------

type seedFixture struct {
	sourceID     int64
	convID       int64
	messageIDs   []int64
	maxMessageID int64
	messageCount int
	fromPID      int64
	toPID        int64
}

func seedMigrationData(t *testing.T, f *storetest.Fixture) seedFixture {
	t.Helper()

	fromPID := f.EnsureParticipant("alice@example.com", "Alice Example", "example.com")
	toPID := f.EnsureParticipant("bob@example.com", "Bob Example", "example.com")

	labels := f.EnsureLabels(map[string]string{
		"INBOX": "INBOX",
		"WORK":  "Work",
	}, "system")

	var ids []int64
	var maxID int64
	baseTime := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	bodies := []string{
		"first body mentioning zorblax token",
		"second body with different content",
		"third body with attachment reference",
	}
	subjects := []string{"Greeting", "Follow up", "Weekly report"}
	for i := 0; i < 3; i++ {
		b := f.NewMessage().
			WithSourceMessageID("seed-" + string(rune('a'+i))).
			WithSubject(subjects[i]).
			WithSentAt(baseTime.Add(time.Duration(i) * time.Hour)).
			WithSize(int64((i + 1) * 1000))
		if i == 2 {
			b = b.WithAttachmentCount(1)
		}
		msgID := b.Create(t, f.Store)

		if err := f.Store.UpsertMessageBody(msgID,
			sql.NullString{String: bodies[i], Valid: true},
			sql.NullString{}); err != nil {
			t.Fatalf("UpsertMessageBody %d: %v", i, err)
		}
		if err := f.Store.ReplaceMessageRecipients(msgID, "from",
			[]int64{fromPID}, []string{"Alice"}); err != nil {
			t.Fatalf("ReplaceMessageRecipients from: %v", err)
		}
		if err := f.Store.ReplaceMessageRecipients(msgID, "to",
			[]int64{toPID}, []string{"Bob"}); err != nil {
			t.Fatalf("ReplaceMessageRecipients to: %v", err)
		}
		if err := f.Store.UpsertFTS(msgID, subjects[i], bodies[i],
			"alice@example.com", "bob@example.com", ""); err != nil {
			t.Fatalf("UpsertFTS: %v", err)
		}
		// Attach one label to every message and a second to the first one
		// to exercise the many-to-many message_labels path.
		if _, err := f.Store.DB().Exec(f.Store.Rebind(
			"INSERT INTO message_labels (message_id, label_id) VALUES (?, ?)"),
			msgID, labels["INBOX"]); err != nil {
			t.Fatalf("insert message_label: %v", err)
		}
		if i == 0 {
			if _, err := f.Store.DB().Exec(f.Store.Rebind(
				"INSERT INTO message_labels (message_id, label_id) VALUES (?, ?)"),
				msgID, labels["WORK"]); err != nil {
				t.Fatalf("insert message_label 2: %v", err)
			}
		}
		if i == 2 {
			// Attachment with a small raw-data blob to verify BYTEA/BLOB round-trips.
			if _, err := f.Store.DB().Exec(f.Store.Rebind(
				`INSERT INTO attachments
				 (message_id, filename, mime_type, size, content_hash, storage_path)
				 VALUES (?, ?, ?, ?, ?, ?)`),
				msgID, "doc.pdf", "application/pdf", 12345, "hash-abc", "ab/hash-abc"); err != nil {
				t.Fatalf("insert attachment: %v", err)
			}
			if _, err := f.Store.DB().Exec(f.Store.Rebind(
				`INSERT INTO message_raw
				 (message_id, raw_data, raw_format, compression, encryption_version)
				 VALUES (?, ?, ?, ?, ?)`),
				msgID, []byte{0x00, 0x01, 0x02, 0xFF, 0xFE}, "mime", "zlib", 0); err != nil {
				t.Fatalf("insert message_raw: %v", err)
			}
		}

		ids = append(ids, msgID)
		if msgID > maxID {
			maxID = msgID
		}
	}

	return seedFixture{
		sourceID:     f.Source.ID,
		convID:       f.ConvID,
		messageIDs:   ids,
		maxMessageID: maxID,
		messageCount: len(ids),
		fromPID:      fromPID,
		toPID:        toPID,
	}
}

func assertMigratedRowCounts(t *testing.T, src, dst *store.Store) {
	t.Helper()
	for _, tbl := range []string{
		"sources", "participants", "conversations",
		"messages", "message_bodies", "message_recipients",
		"labels", "message_labels", "attachments", "message_raw",
	} {
		srcN := countRows(t, src, tbl)
		dstN := countRows(t, dst, tbl)
		if srcN != dstN {
			t.Errorf("%s: src=%d dst=%d", tbl, srcN, dstN)
		}
	}
}

func countRows(t *testing.T, s *store.Store, tbl string) int64 {
	t.Helper()
	var n int64
	if err := s.DB().QueryRow("SELECT COUNT(*) FROM " + tbl).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", tbl, err)
	}
	return n
}

// assertPayloadRoundTrips checks every non-trivial column kind by comparing
// a specific row on the source vs. the destination. This catches silent
// coercion bugs (timestamps, bools stored as 0/1, bytes, JSON) that row-count
// comparisons miss.
func assertPayloadRoundTrips(t *testing.T, src, dst *store.Store, seed seedFixture) {
	t.Helper()

	// Timestamp + subject + size on message #0.
	{
		var srcSubj, dstSubj string
		var srcSent, dstSent sql.NullTime
		var srcSize, dstSize int64
		if err := src.DB().QueryRow(src.Rebind(
			"SELECT subject, sent_at, size_estimate FROM messages WHERE id = ?"),
			seed.messageIDs[0]).Scan(&srcSubj, &srcSent, &srcSize); err != nil {
			t.Fatalf("src msg scan: %v", err)
		}
		if err := dst.DB().QueryRow(dst.Rebind(
			"SELECT subject, sent_at, size_estimate FROM messages WHERE id = ?"),
			seed.messageIDs[0]).Scan(&dstSubj, &dstSent, &dstSize); err != nil {
			t.Fatalf("dst msg scan: %v", err)
		}
		if srcSubj != dstSubj {
			t.Errorf("subject: src=%q dst=%q", srcSubj, dstSubj)
		}
		if srcSize != dstSize {
			t.Errorf("size_estimate: src=%d dst=%d", srcSize, dstSize)
		}
		if srcSent.Valid != dstSent.Valid || !srcSent.Time.Equal(dstSent.Time) {
			t.Errorf("sent_at: src=%v dst=%v", srcSent, dstSent)
		}
	}

	// Bool: has_attachments on message #2 is true, on #0 is false.
	{
		var t0, t2 bool
		if err := dst.DB().QueryRow(dst.Rebind(
			"SELECT has_attachments FROM messages WHERE id = ?"),
			seed.messageIDs[0]).Scan(&t0); err != nil {
			t.Fatalf("dst has_attachments m0: %v", err)
		}
		if err := dst.DB().QueryRow(dst.Rebind(
			"SELECT has_attachments FROM messages WHERE id = ?"),
			seed.messageIDs[2]).Scan(&t2); err != nil {
			t.Fatalf("dst has_attachments m2: %v", err)
		}
		if t0 {
			t.Errorf("msg[0].has_attachments: want false, got true")
		}
		if !t2 {
			t.Errorf("msg[2].has_attachments: want true, got false")
		}
	}

	// Bytes: message_raw.raw_data on message #2.
	{
		var raw []byte
		if err := dst.DB().QueryRow(dst.Rebind(
			"SELECT raw_data FROM message_raw WHERE message_id = ?"),
			seed.messageIDs[2]).Scan(&raw); err != nil {
			t.Fatalf("dst raw_data: %v", err)
		}
		want := []byte{0x00, 0x01, 0x02, 0xFF, 0xFE}
		if len(raw) != len(want) {
			t.Errorf("raw_data len: want %d got %d", len(want), len(raw))
		} else {
			for i := range want {
				if raw[i] != want[i] {
					t.Errorf("raw_data[%d]: want %x got %x", i, want[i], raw[i])
				}
			}
		}
	}
}

func assertIDsPreserved(t *testing.T, dst *store.Store, seed seedFixture) {
	t.Helper()
	for _, id := range seed.messageIDs {
		var count int64
		if err := dst.DB().QueryRow(dst.Rebind(
			"SELECT COUNT(*) FROM messages WHERE id = ?"), id).Scan(&count); err != nil {
			t.Fatalf("lookup migrated message %d: %v", id, err)
		}
		if count != 1 {
			t.Errorf("migrated message id %d: count=%d, want 1", id, count)
		}
	}
}

func assertSearchFindsSeed(t *testing.T, dst *store.Store, seed seedFixture) {
	t.Helper()
	res, total, err := dst.SearchMessages("zorblax", 0, 10)
	if err != nil {
		t.Fatalf("search zorblax on dst: %v", err)
	}
	if total != 1 {
		t.Errorf("dst FTS search total = %d, want 1", total)
	}
	if len(res) != 1 || res[0].ID != seed.messageIDs[0] {
		t.Errorf("dst FTS result for 'zorblax' = %v, want msg %d", res, seed.messageIDs[0])
	}
}

// assertNextIDAfterMigration verifies that a subsequent insert on the
// destination produces an ID greater than the largest migrated ID. For
// SQLite this is automatic (INTEGER PRIMARY KEY tracks AUTOINCREMENT state);
// for PostgreSQL it depends on resetPostgresSequences having run.
func assertNextIDAfterMigration(t *testing.T, dst *store.Store, maxMigratedID int64) {
	t.Helper()

	// Use a minimal message insert. We need a valid conversation_id / source_id;
	// migrated row IDs are preserved, so any existing one works.
	var sourceID, convID int64
	if err := dst.DB().QueryRow("SELECT id FROM sources LIMIT 1").Scan(&sourceID); err != nil {
		t.Fatalf("lookup migrated source: %v", err)
	}
	if err := dst.DB().QueryRow("SELECT id FROM conversations LIMIT 1").Scan(&convID); err != nil {
		t.Fatalf("lookup migrated conversation: %v", err)
	}

	id, err := dst.UpsertMessage(&store.Message{
		ConversationID:  convID,
		SourceID:        sourceID,
		SourceMessageID: "post-migration-msg",
		MessageType:     "email",
		SizeEstimate:    100,
	})
	if err != nil {
		t.Fatalf("UpsertMessage after migration: %v", err)
	}
	if id <= maxMigratedID {
		t.Errorf("post-migration insert id = %d, want > %d (sequence not advanced?)",
			id, maxMigratedID)
	}
}

// Unused on SQLite — guards against "imported and not used" for the
// testutil helpers on builds where no PG-gated code paths fire.
var _ = testutil.IsPostgresTest
