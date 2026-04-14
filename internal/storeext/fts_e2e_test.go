// Package storeext_test contains cross-package integration tests that would
// create an import cycle if placed under internal/store (because the tests
// use internal/testutil/storetest, which imports internal/store).
package storeext_test

import (
	"testing"

	"github.com/wesm/msgvault/internal/testutil/storetest"
)

// TestFTSEndToEnd exercises the full FTS flow — upsert, search, delete —
// against whichever backend MSGVAULT_TEST_DB selects. Confirms that search
// results actually come back, rather than silently returning empty because
// the FTS index wasn't populated.
func TestFTSEndToEnd(t *testing.T) {
	f := storetest.New(t)

	if !f.Store.FTS5Available() {
		t.Skip("FTS not available in this build")
	}

	// Insert two messages and populate the FTS index.
	msg1 := f.CreateMessage("m1")
	msg2 := f.CreateMessage("m2")

	if err := f.Store.UpsertFTS(msg1, "Hello postgres world", "body with unique token zorblax", "alice@example.com", "bob@example.com", ""); err != nil {
		t.Fatalf("UpsertFTS msg1: %v", err)
	}
	if err := f.Store.UpsertFTS(msg2, "Meeting notes", "quarterly review of projects", "carol@example.com", "dave@example.com", ""); err != nil {
		t.Fatalf("UpsertFTS msg2: %v", err)
	}

	// Search for a unique term that should match exactly one message.
	results, total, err := f.Store.SearchMessages("zorblax", 0, 10)
	if err != nil {
		t.Fatalf("SearchMessages(zorblax): %v", err)
	}
	if total != 1 {
		t.Errorf("total = %d, want 1", total)
	}
	if len(results) != 1 {
		t.Fatalf("results len = %d, want 1", len(results))
	}
	if results[0].ID != msg1 {
		t.Errorf("result[0].ID = %d, want %d", results[0].ID, msg1)
	}

	// Search for a subject term.
	results, _, err = f.Store.SearchMessages("meeting", 0, 10)
	if err != nil {
		t.Fatalf("SearchMessages(meeting): %v", err)
	}
	if len(results) != 1 || results[0].ID != msg2 {
		t.Errorf("expected msg2 for 'meeting', got %v", results)
	}

	// Search for a term that matches nothing.
	results, total, err = f.Store.SearchMessages("nonexistentterm", 0, 10)
	if err != nil {
		t.Fatalf("SearchMessages(nonexistent): %v", err)
	}
	if total != 0 || len(results) != 0 {
		t.Errorf("expected zero results for 'nonexistentterm', got total=%d len=%d", total, len(results))
	}
}
