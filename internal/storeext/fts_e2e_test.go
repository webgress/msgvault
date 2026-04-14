// Package storeext_test contains cross-package integration tests that would
// create an import cycle if placed under internal/store (because the tests
// use internal/testutil/storetest, which imports internal/store).
//
// These tests exercise the Store API portably — they run identically
// against whichever backend MSGVAULT_TEST_DB selects (SQLite by default,
// PostgreSQL when set). The store package has some tests that directly
// inspect the SQLite-specific messages_fts virtual table; those are
// necessarily SQLite-only and skipped on PG. This file provides parallel
// coverage of the same behavior through the public Store API.
package storeext_test

import (
	"database/sql"
	"testing"

	"github.com/wesm/msgvault/internal/store"
	"github.com/wesm/msgvault/internal/testutil"
	"github.com/wesm/msgvault/internal/testutil/storetest"
)

// TestFTSEndToEnd exercises the full FTS flow — upsert, search — against
// whichever backend MSGVAULT_TEST_DB selects.
func TestFTSEndToEnd(t *testing.T) {
	f := storetest.New(t)
	if !f.Store.FTS5Available() {
		t.Skip("FTS not available in this build")
	}

	msg1 := f.CreateMessage("m1")
	msg2 := f.CreateMessage("m2")

	if err := f.Store.UpsertFTS(msg1, "Hello postgres world", "body with unique token zorblax", "alice@example.com", "bob@example.com", ""); err != nil {
		t.Fatalf("UpsertFTS msg1: %v", err)
	}
	if err := f.Store.UpsertFTS(msg2, "Meeting notes", "quarterly review of projects", "carol@example.com", "dave@example.com", ""); err != nil {
		t.Fatalf("UpsertFTS msg2: %v", err)
	}

	// Search for a unique body term.
	results, total, err := f.Store.SearchMessages("zorblax", 0, 10)
	if err != nil {
		t.Fatalf("SearchMessages(zorblax): %v", err)
	}
	if total != 1 {
		t.Errorf("total = %d, want 1", total)
	}
	if len(results) != 1 || results[0].ID != msg1 {
		t.Errorf("expected msg1 for 'zorblax', got %v", results)
	}

	// Search by subject term.
	results, _, err = f.Store.SearchMessages("meeting", 0, 10)
	if err != nil {
		t.Fatalf("SearchMessages(meeting): %v", err)
	}
	if len(results) != 1 || results[0].ID != msg2 {
		t.Errorf("expected msg2 for 'meeting', got %v", results)
	}

	// Unmatchable term → empty.
	results, total, err = f.Store.SearchMessages("nonexistentterm", 0, 10)
	if err != nil {
		t.Fatalf("SearchMessages(nonexistent): %v", err)
	}
	if total != 0 || len(results) != 0 {
		t.Errorf("expected zero results, got total=%d len=%d", total, len(results))
	}
}

// TestFTSUpsertReplacesPriorContent verifies that calling UpsertFTS a second
// time on the same message overwrites the prior indexed text. This is the
// portable equivalent of the TestStore_UpsertFTS "replace" assertion,
// which directly inspected SQLite's messages_fts virtual table.
func TestFTSUpsertReplacesPriorContent(t *testing.T) {
	f := storetest.New(t)
	if !f.Store.FTS5Available() {
		t.Skip("FTS not available in this build")
	}

	msgID := f.CreateMessage("upsert-replace")

	// Initial index content.
	if err := f.Store.UpsertFTS(msgID, "Initial Subject", "hello world body text", "alice@example.com", "bob@example.com", "carol@example.com"); err != nil {
		t.Fatalf("initial UpsertFTS: %v", err)
	}

	// Search for initial tokens — should find exactly one hit each.
	for _, term := range []string{"hello", "initial", "alice"} {
		_, total, err := f.Store.SearchMessages(term, 0, 10)
		if err != nil {
			t.Fatalf("search %q: %v", term, err)
		}
		if total != 1 {
			t.Errorf("initial search %q total = %d, want 1", term, total)
		}
	}

	// Replace with new content. The old tokens should no longer match;
	// the new tokens should.
	if err := f.Store.UpsertFTS(msgID, "Updated Subject", "completely different body text", "alice@example.com", "bob@example.com", ""); err != nil {
		t.Fatalf("replace UpsertFTS: %v", err)
	}

	// Old body token "hello" should no longer match.
	_, total, err := f.Store.SearchMessages("hello", 0, 10)
	if err != nil {
		t.Fatalf("search 'hello' after replace: %v", err)
	}
	if total != 0 {
		t.Errorf("after replace, 'hello' total = %d, want 0 (old token should be gone)", total)
	}

	// New tokens should match.
	for _, term := range []string{"updated", "different"} {
		_, total, err := f.Store.SearchMessages(term, 0, 10)
		if err != nil {
			t.Fatalf("search %q after replace: %v", term, err)
		}
		if total != 1 {
			t.Errorf("after replace, %q total = %d, want 1", term, total)
		}
	}
}

// TestBackfillFTSRepopulatesIndex verifies that BackfillFTS rebuilds the
// search index from the messages/bodies/recipients tables. Portable
// equivalent of TestStore_BackfillFTS which used SQLite-direct DELETE
// FROM messages_fts; this test uses the dialect-provided clear path
// implicit in BackfillFTS (it clears before repopulating).
func TestBackfillFTSRepopulatesIndex(t *testing.T) {
	f := storetest.New(t)
	if !f.Store.FTS5Available() {
		t.Skip("FTS not available in this build")
	}

	// Seed two messages with distinctive tokens, using the public sync-like
	// flow (UpsertMessageBody + ReplaceMessageRecipients) — but DO NOT call
	// UpsertFTS. This simulates an older database that was populated before
	// FTS existed.
	msgID1 := f.CreateMessage("backfill-1")
	if err := f.Store.UpsertMessageBody(msgID1,
		sql.NullString{String: "first message contains token alphadelta", Valid: true},
		sql.NullString{}); err != nil {
		t.Fatalf("UpsertMessageBody 1: %v", err)
	}
	pid1 := f.EnsureParticipant("sender1@example.com", "Sender One", "example.com")
	if err := f.Store.ReplaceMessageRecipients(msgID1, "from", []int64{pid1}, []string{"Sender One"}); err != nil {
		t.Fatalf("ReplaceMessageRecipients 1: %v", err)
	}

	msgID2 := f.CreateMessage("backfill-2")
	if err := f.Store.UpsertMessageBody(msgID2,
		sql.NullString{String: "second message contains token bravoecho", Valid: true},
		sql.NullString{}); err != nil {
		t.Fatalf("UpsertMessageBody 2: %v", err)
	}
	pid2 := f.EnsureParticipant("sender2@example.com", "Sender Two", "example.com")
	if err := f.Store.ReplaceMessageRecipients(msgID2, "from", []int64{pid2}, []string{"Sender Two"}); err != nil {
		t.Fatalf("ReplaceMessageRecipients 2: %v", err)
	}

	// Without a backfill, neither token should be findable (no FTS rows yet).
	// Note: on a fresh DB with only these 2 messages, NeedsFTSBackfill may
	// report true OR false depending on the heuristic; we don't assert on
	// it here, we assert on actual search behavior.

	// Run backfill and verify rows were indexed.
	n, err := f.Store.BackfillFTS(nil)
	if err != nil {
		t.Fatalf("BackfillFTS: %v", err)
	}
	if n != 2 {
		t.Errorf("BackfillFTS indexed %d rows, want 2", n)
	}

	// Both tokens should now be searchable.
	for token, wantMsg := range map[string]int64{
		"alphadelta": msgID1,
		"bravoecho":  msgID2,
	} {
		results, total, err := f.Store.SearchMessages(token, 0, 10)
		if err != nil {
			t.Fatalf("search %q: %v", token, err)
		}
		if total != 1 {
			t.Errorf("search %q total = %d, want 1", token, total)
		}
		if len(results) != 1 || results[0].ID != wantMsg {
			t.Errorf("search %q: expected msg %d, got %v", token, wantMsg, results)
		}
	}

	// Sender email should also be indexed.
	results, _, err := f.Store.SearchMessages("sender1", 0, 10)
	if err != nil {
		t.Fatalf("search sender1: %v", err)
	}
	found := false
	for _, r := range results {
		if r.ID == msgID1 {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("sender1 search did not return msgID1; results=%v", results)
	}
}

// TestRemoveSourceClearsSearchResults is the portable equivalent of the
// SQLite-specific FTS assertion in TestStore_RemoveSource. It verifies
// that after RemoveSource, searches no longer return that source's messages.
func TestRemoveSourceClearsSearchResults(t *testing.T) {
	f := storetest.New(t)
	if !f.Store.FTS5Available() {
		t.Skip("FTS not available in this build")
	}

	// Create a message, index it, and confirm it's findable.
	msgID := f.CreateMessage("will-be-removed")
	if err := f.Store.UpsertFTS(msgID, "Initial", "uniquetoken xyzzyz", "a@b.com", "", ""); err != nil {
		t.Fatalf("UpsertFTS: %v", err)
	}

	_, total, err := f.Store.SearchMessages("xyzzyz", 0, 10)
	if err != nil {
		t.Fatalf("pre-remove search: %v", err)
	}
	if total != 1 {
		t.Fatalf("pre-remove total = %d, want 1", total)
	}

	// Remove the source (cascades to messages).
	if err := f.Store.RemoveSource(f.Source.ID); err != nil {
		t.Fatalf("RemoveSource: %v", err)
	}

	// Post-condition: the search result must be empty.
	results, total, err := f.Store.SearchMessages("xyzzyz", 0, 10)
	if err != nil {
		t.Fatalf("post-remove search: %v", err)
	}
	if total != 0 || len(results) != 0 {
		t.Errorf("after RemoveSource, search returned total=%d results=%d, want 0", total, len(results))
	}
}

// TestFTSSearchSQLInjection verifies that search terms containing SQL/FTS
// metacharacters don't cause a query error on either backend. This guards
// against regression in the dialect's search-term building (SQLite FTS5
// special chars, PostgreSQL tsquery operators).
func TestFTSSearchSQLInjection(t *testing.T) {
	f := storetest.New(t)
	if !f.Store.FTS5Available() {
		t.Skip("FTS not available in this build")
	}

	msgID := f.CreateMessage("inj")
	if err := f.Store.UpsertFTS(msgID, "Subject", "legitimate body text", "a@b.com", "", ""); err != nil {
		t.Fatalf("UpsertFTS: %v", err)
	}

	// These terms contain characters that could, if unescaped, produce
	// invalid tsquery (PG) or invalid FTS5 MATCH (SQLite) syntax.
	suspicious := []string{
		"legitimate & OR !",
		"' OR 1=1",
		"foo | bar",
		"foo(bar)",
		":*",
		"---",
		"",
	}
	for _, term := range suspicious {
		_, _, err := f.Store.SearchMessages(term, 0, 10)
		if err != nil {
			t.Errorf("SearchMessages(%q) returned error: %v", term, err)
		}
	}
}

// guard so the unused store import generates a real symbol reference.
var _ = store.Stats{}
var _ = testutil.IsPostgresTest
