package store_test

import (
	"context"
	"fmt"
	"testing"

	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

// attachmentCorpus seeds a multi-source corpus of messages with attachments
// to exercise content-hash dedup, cross-source dedup, and ON DELETE CASCADE
// against the live store (SQLite or PostgreSQL via MSGVAULT_TEST_DB).
type attachmentCorpus struct {
	t       *testing.T
	store   *store.Store
	srcA    *store.Source
	srcB    *store.Source
	convA   int64
	convB   int64
	msgRows map[string]int64 // gmail id → message row id
}

func newAttachmentCorpus(t *testing.T) *attachmentCorpus {
	t.Helper()
	st := testutil.NewTestStore(t)

	srcA, err := st.GetOrCreateSource("gmail", "alice@example.com")
	if err != nil {
		t.Fatalf("GetOrCreateSource A: %v", err)
	}
	srcB, err := st.GetOrCreateSource("gmail", "bob@example.com")
	if err != nil {
		t.Fatalf("GetOrCreateSource B: %v", err)
	}
	convA, err := st.EnsureConversation(srcA.ID, "thread-A", "Thread A")
	if err != nil {
		t.Fatalf("EnsureConversation A: %v", err)
	}
	convB, err := st.EnsureConversation(srcB.ID, "thread-B", "Thread B")
	if err != nil {
		t.Fatalf("EnsureConversation B: %v", err)
	}

	return &attachmentCorpus{
		t:       t,
		store:   st,
		srcA:    srcA,
		srcB:    srcB,
		convA:   convA,
		convB:   convB,
		msgRows: make(map[string]int64),
	}
}

func (c *attachmentCorpus) addMessage(gmailID string, sourceID, convID int64) int64 {
	c.t.Helper()
	id, err := c.store.UpsertMessage(&store.Message{
		ConversationID:  convID,
		SourceID:        sourceID,
		SourceMessageID: gmailID,
		MessageType:     "email",
		SizeEstimate:    100,
	})
	if err != nil {
		c.t.Fatalf("UpsertMessage(%s): %v", gmailID, err)
	}
	c.msgRows[gmailID] = id
	return id
}

func (c *attachmentCorpus) addAttachment(gmailID, filename, hash string) {
	c.t.Helper()
	msgID, ok := c.msgRows[gmailID]
	if !ok {
		c.t.Fatalf("addAttachment: unknown gmail id %q", gmailID)
	}
	storagePath := hash[:2] + "/" + hash
	if err := c.store.UpsertAttachment(msgID, filename, "application/pdf",
		storagePath, hash, 100); err != nil {
		c.t.Fatalf("UpsertAttachment(%s, %s): %v", gmailID, filename, err)
	}
}

func (c *attachmentCorpus) attachmentRowCount() int {
	c.t.Helper()
	var n int
	if err := c.store.DB().QueryRow(`SELECT COUNT(*) FROM attachments`).Scan(&n); err != nil {
		c.t.Fatalf("attachmentRowCount: %v", err)
	}
	return n
}

func (c *attachmentCorpus) attachmentRowsForHash(hash string) int {
	c.t.Helper()
	var n int
	err := c.store.DB().QueryRow(
		c.store.Rebind(`SELECT COUNT(*) FROM attachments WHERE content_hash = ?`),
		hash,
	).Scan(&n)
	if err != nil {
		c.t.Fatalf("attachmentRowsForHash(%s): %v", hash, err)
	}
	return n
}

// hashes used throughout the suite. Real values are 64-char hex; the values
// here are fixed sentinels that round-trip cleanly through the DB and the
// content_hash column has no parsing constraints inside the store layer.
const (
	hashShared = "h1sharedhash0000000000000000000000000000000000000000000000000abc"
	hashUniqA  = "h2uniqueA00000000000000000000000000000000000000000000000000000de"
	hashUniqB  = "h3uniqueB000000000000000000000000000000000000000000000000000ab12"
)

// TestAttachment_E2E_MultiMessageDedup verifies that multiple messages within
// a single source can reference the same content_hash via UpsertAttachment
// and that the helper is idempotent (re-upserting the same (message_id,
// content_hash) pair is a no-op).
func TestAttachment_E2E_MultiMessageDedup(t *testing.T) {
	c := newAttachmentCorpus(t)

	// Three messages in source A referencing the same content hash.
	c.addMessage("msg-1", c.srcA.ID, c.convA)
	c.addMessage("msg-2", c.srcA.ID, c.convA)
	c.addMessage("msg-3", c.srcA.ID, c.convA)
	c.addAttachment("msg-1", "shared.pdf", hashShared)
	c.addAttachment("msg-2", "shared.pdf", hashShared)
	c.addAttachment("msg-3", "shared.pdf", hashShared)

	// One row per message, all referencing the same hash.
	if got := c.attachmentRowsForHash(hashShared); got != 3 {
		t.Errorf("rows for hashShared = %d, want 3", got)
	}

	// Idempotent re-upsert: existing (message_id, content_hash) is a no-op.
	c.addAttachment("msg-2", "shared.pdf", hashShared)
	if got := c.attachmentRowsForHash(hashShared); got != 3 {
		t.Errorf("rows for hashShared after re-upsert = %d, want 3", got)
	}

	// IsAttachmentPathReferenced reports the hash storage path as referenced.
	referenced, err := c.store.IsAttachmentPathReferenced(hashShared[:2] + "/" + hashShared)
	if err != nil {
		t.Fatalf("IsAttachmentPathReferenced: %v", err)
	}
	if !referenced {
		t.Error("expected referenced=true while messages still hold the hash")
	}
}

// TestAttachment_E2E_CascadeOnMessageDelete verifies that deleting a message
// row removes its attachment row via ON DELETE CASCADE — but leaves other
// messages' attachment rows that reference the same content_hash intact.
func TestAttachment_E2E_CascadeOnMessageDelete(t *testing.T) {
	c := newAttachmentCorpus(t)

	// Two messages in source A referencing the shared hash plus one with a
	// unique hash.
	c.addMessage("msg-1", c.srcA.ID, c.convA)
	c.addMessage("msg-2", c.srcA.ID, c.convA)
	c.addMessage("msg-3", c.srcA.ID, c.convA)
	c.addAttachment("msg-1", "shared.pdf", hashShared)
	c.addAttachment("msg-2", "shared.pdf", hashShared)
	c.addAttachment("msg-3", "unique.pdf", hashUniqA)

	if got := c.attachmentRowCount(); got != 3 {
		t.Errorf("initial attachment count = %d, want 3", got)
	}

	// Permanently delete msg-1; its attachment row cascades.
	if err := c.store.MarkMessageDeletedByGmailID(true, "msg-1"); err != nil {
		t.Fatalf("MarkMessageDeletedByGmailID(permanent, msg-1): %v", err)
	}

	if got := c.attachmentRowsForHash(hashShared); got != 1 {
		t.Errorf("rows for hashShared after delete = %d, want 1", got)
	}

	// The shared storage path is still referenced (msg-2 holds it).
	referenced, err := c.store.IsAttachmentPathReferenced(hashShared[:2] + "/" + hashShared)
	if err != nil {
		t.Fatalf("IsAttachmentPathReferenced: %v", err)
	}
	if !referenced {
		t.Error("shared path should remain referenced via msg-2 after msg-1 delete")
	}

	// Now delete the last referrer of hashShared.
	if err := c.store.MarkMessageDeletedByGmailID(true, "msg-2"); err != nil {
		t.Fatalf("MarkMessageDeletedByGmailID(permanent, msg-2): %v", err)
	}
	if got := c.attachmentRowsForHash(hashShared); got != 0 {
		t.Errorf("rows for hashShared after both deleted = %d, want 0", got)
	}
	referenced, err = c.store.IsAttachmentPathReferenced(hashShared[:2] + "/" + hashShared)
	if err != nil {
		t.Fatalf("IsAttachmentPathReferenced: %v", err)
	}
	if referenced {
		t.Error("shared path should be unreferenced after both messages deleted")
	}
}

// TestAttachment_E2E_CrossSourceDedupPromotion verifies that
// AttachmentPathsUniqueToSource handles the cross-source case correctly:
// a hash shared with another source is NOT reported as unique. After the
// other source is removed, the same hash becomes unique.
func TestAttachment_E2E_CrossSourceDedupPromotion(t *testing.T) {
	c := newAttachmentCorpus(t)

	// Layout:
	//   source A: msg-a1 (shared hash), msg-a2 (unique-A hash)
	//   source B: msg-b1 (shared hash), msg-b2 (unique-B hash)
	c.addMessage("msg-a1", c.srcA.ID, c.convA)
	c.addMessage("msg-a2", c.srcA.ID, c.convA)
	c.addMessage("msg-b1", c.srcB.ID, c.convB)
	c.addMessage("msg-b2", c.srcB.ID, c.convB)
	c.addAttachment("msg-a1", "shared.pdf", hashShared)
	c.addAttachment("msg-a2", "unique-a.pdf", hashUniqA)
	c.addAttachment("msg-b1", "shared.pdf", hashShared)
	c.addAttachment("msg-b2", "unique-b.pdf", hashUniqB)

	// Before removing B: A's unique-set is just hashUniqA.
	pathsA, err := c.store.AttachmentPathsUniqueToSource(c.srcA.ID)
	if err != nil {
		t.Fatalf("AttachmentPathsUniqueToSource(A): %v", err)
	}
	wantA := hashUniqA[:2] + "/" + hashUniqA
	if len(pathsA) != 1 || pathsA[0] != wantA {
		t.Errorf("pathsA before B removal = %v, want [%s]", pathsA, wantA)
	}

	// Symmetric: B has only unique-B as a unique path.
	pathsB, err := c.store.AttachmentPathsUniqueToSource(c.srcB.ID)
	if err != nil {
		t.Fatalf("AttachmentPathsUniqueToSource(B): %v", err)
	}
	wantB := hashUniqB[:2] + "/" + hashUniqB
	if len(pathsB) != 1 || pathsB[0] != wantB {
		t.Errorf("pathsB before A removal = %v, want [%s]", pathsB, wantB)
	}

	// Remove source B. The shared hash is now unique to A.
	if err := c.store.RemoveSource(c.srcB.ID); err != nil {
		t.Fatalf("RemoveSource(B): %v", err)
	}

	pathsA, err = c.store.AttachmentPathsUniqueToSource(c.srcA.ID)
	if err != nil {
		t.Fatalf("AttachmentPathsUniqueToSource(A) after B removal: %v", err)
	}
	got := testutil.MakeSet(pathsA...)
	for _, want := range []string{hashShared[:2] + "/" + hashShared, wantA} {
		if !got[want] {
			t.Errorf("paths missing %q after B removal; got %v", want, pathsA)
		}
	}
	if len(pathsA) != 2 {
		t.Errorf("pathsA len after B removal = %d, want 2; got %v", len(pathsA), pathsA)
	}
}

// TestAttachment_E2E_RemoveSourceCascadesAttachmentRows verifies that
// removing a source cascades all of its attachment rows but leaves rows
// in other sources alone — even when they share content_hash.
func TestAttachment_E2E_RemoveSourceCascadesAttachmentRows(t *testing.T) {
	c := newAttachmentCorpus(t)

	c.addMessage("msg-a1", c.srcA.ID, c.convA)
	c.addMessage("msg-b1", c.srcB.ID, c.convB)
	c.addAttachment("msg-a1", "shared.pdf", hashShared)
	c.addAttachment("msg-b1", "shared.pdf", hashShared)

	if got := c.attachmentRowCount(); got != 2 {
		t.Errorf("initial attachment count = %d, want 2", got)
	}
	if got := c.attachmentRowsForHash(hashShared); got != 2 {
		t.Errorf("initial rows for shared hash = %d, want 2", got)
	}

	if err := c.store.RemoveSource(c.srcA.ID); err != nil {
		t.Fatalf("RemoveSource(A): %v", err)
	}

	if got := c.attachmentRowCount(); got != 1 {
		t.Errorf("attachment count after A removed = %d, want 1", got)
	}
	if got := c.attachmentRowsForHash(hashShared); got != 1 {
		t.Errorf("rows for shared hash after A removed = %d, want 1 (B keeps reference)", got)
	}

	// IsAttachmentPathReferenced still reports the shared path as referenced
	// (B's row).
	referenced, err := c.store.IsAttachmentPathReferenced(hashShared[:2] + "/" + hashShared)
	if err != nil {
		t.Fatalf("IsAttachmentPathReferenced: %v", err)
	}
	if !referenced {
		t.Error("shared path should remain referenced via source B")
	}
}

// TestAttachment_E2E_OrphanCleanupLifecycle simulates the full orphan-cleanup
// pipeline in remove_account.go for a multi-source corpus: collect candidate
// paths, run the source removal, then verify per-file reference checks against
// the post-removal DB state.
func TestAttachment_E2E_OrphanCleanupLifecycle(t *testing.T) {
	c := newAttachmentCorpus(t)

	// Source A has one unique attachment + one shared with B.
	// Source B has its own unique + the shared one.
	c.addMessage("msg-a1", c.srcA.ID, c.convA)
	c.addMessage("msg-a2", c.srcA.ID, c.convA)
	c.addMessage("msg-b1", c.srcB.ID, c.convB)
	c.addMessage("msg-b2", c.srcB.ID, c.convB)
	c.addAttachment("msg-a1", "shared.pdf", hashShared)
	c.addAttachment("msg-a2", "unique-a.pdf", hashUniqA)
	c.addAttachment("msg-b1", "shared.pdf", hashShared)
	c.addAttachment("msg-b2", "unique-b.pdf", hashUniqB)

	// Pipeline step 1: collect candidate paths for source A *before* the
	// cascade — matching remove_account.go's ordering.
	candidates, err := c.store.AttachmentPathsUniqueToSource(c.srcA.ID)
	if err != nil {
		t.Fatalf("AttachmentPathsUniqueToSource(A): %v", err)
	}
	wantUniqAPath := hashUniqA[:2] + "/" + hashUniqA
	if len(candidates) != 1 || candidates[0] != wantUniqAPath {
		t.Errorf("candidates for A = %v, want [%s]", candidates, wantUniqAPath)
	}

	// Pipeline step 2: cascade-delete source A.
	hadActive, err := c.store.RemoveSourceSerialized(context.Background(), c.srcA.ID)
	if err != nil {
		t.Fatalf("RemoveSourceSerialized(A): %v", err)
	}
	if hadActive {
		t.Error("hadActiveSync = true, want false (no sync running in fixture)")
	}

	// Pipeline step 3: per-candidate reference recheck. The candidate path
	// for A is now unreferenced (msg-a2 row is gone); the shared path is
	// still referenced by source B.
	referenced, err := c.store.IsAttachmentPathReferenced(wantUniqAPath)
	if err != nil {
		t.Fatalf("IsAttachmentPathReferenced(uniqA): %v", err)
	}
	if referenced {
		t.Error("uniqA path should be unreferenced after source A removed")
	}

	sharedPath := hashShared[:2] + "/" + hashShared
	referenced, err = c.store.IsAttachmentPathReferenced(sharedPath)
	if err != nil {
		t.Fatalf("IsAttachmentPathReferenced(shared): %v", err)
	}
	if !referenced {
		t.Error("shared path should remain referenced after source A removed")
	}
}

// TestAttachment_E2E_NullAndEmptyHashesIgnored verifies that attachments with
// NULL content_hash or empty storage_path are excluded from
// AttachmentPathsUniqueToSource (mirroring the existing focused test but in
// a multi-message context).
func TestAttachment_E2E_NullAndEmptyHashesIgnored(t *testing.T) {
	c := newAttachmentCorpus(t)

	c.addMessage("msg-a1", c.srcA.ID, c.convA)
	c.addMessage("msg-a2", c.srcA.ID, c.convA)
	c.addMessage("msg-a3", c.srcA.ID, c.convA)

	// Normal attachment with a unique content hash.
	c.addAttachment("msg-a1", "good.pdf", hashUniqA)

	// Attachment with NULL content_hash — must NOT appear in unique set.
	_, err := c.store.DB().Exec(c.store.Rebind(fmt.Sprintf(
		`INSERT INTO attachments (message_id, filename, mime_type, storage_path, content_hash, size, created_at)
		 VALUES (?, 'null-hash.pdf', 'application/pdf', 'nn/nullpath', NULL, 0, %s)`,
		"CURRENT_TIMESTAMP",
	)), c.msgRows["msg-a2"])
	if err != nil {
		t.Fatalf("insert null-hash attachment: %v", err)
	}

	// Attachment with empty storage_path — also excluded.
	if err := c.store.UpsertAttachment(c.msgRows["msg-a3"], "empty.pdf",
		"application/pdf", "", "emptypathhash", 0); err != nil {
		t.Fatalf("UpsertAttachment(empty): %v", err)
	}

	paths, err := c.store.AttachmentPathsUniqueToSource(c.srcA.ID)
	if err != nil {
		t.Fatalf("AttachmentPathsUniqueToSource: %v", err)
	}
	want := hashUniqA[:2] + "/" + hashUniqA
	if len(paths) != 1 || paths[0] != want {
		t.Errorf("paths = %v, want [%s] only", paths, want)
	}
}
