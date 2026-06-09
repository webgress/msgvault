package query_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

// TestQueryEngine_PostgresPortability exercises the three SQL shapes
// the external review flagged as failing on PostgreSQL:
//
//   - Aggregate uses `FROM ( … ) AS agg` (PG rejects unaliased derived
//     tables in FROM).
//   - GetGmailIDsByFilter avoids SELECT DISTINCT entirely (PG rejects
//     DISTINCT when ORDER BY references columns missing from SELECT).
//   - ListMessages sorts via expressions that textually match the
//     SELECT list (PG enforces this for SELECT DISTINCT).
//
// Runs against whichever backend testutil.NewTestStore picks up — on
// CI / dev machines that's SQLite; setting MSGVAULT_TEST_DB to a
// postgres:// DSN exercises the PG path that the bugs were specific
// to. The test never asserts on dialect-specific error text; a bug
// would surface as a generic Scan/Exec failure on PG.
func TestQueryEngine_PostgresPortability(t *testing.T) {
	require := requirepkg.New(t)
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("gmail", "pgcompat@example.com")
	require.NoError(err, "GetOrCreateSource")

	convID, err := st.EnsureConversation(src.ID, "thread-1", "Thread 1")
	require.NoError(err, "EnsureConversation")

	aliceID, err := st.EnsureParticipant("alice@example.com", "Alice", "example.com")
	require.NoError(err, "EnsureParticipant alice")
	bobID, err := st.EnsureParticipant("bob@example.com", "Bob", "example.com")
	require.NoError(err, "EnsureParticipant bob")

	labelID, err := st.EnsureLabel(src.ID, "Label_1", "Important", "user")
	require.NoError(err, "EnsureLabel")

	base := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	for i := range 4 {
		mid, err := st.UpsertMessage(&store.Message{
			ConversationID:  convID,
			SourceID:        src.ID,
			SourceMessageID: gmailSourceID(i),
			MessageType:     "email",
			SentAt:          sql.NullTime{Time: base.Add(time.Duration(i) * time.Hour), Valid: true},
			Subject:         sql.NullString{String: subjectFor(i), Valid: true},
			Snippet:         sql.NullString{String: "snippet", Valid: true},
			SizeEstimate:    int64(1000 + i*250),
		})
		require.NoError(err, "UpsertMessage")
		require.NoError(st.ReplaceMessageRecipients(mid, "from", []int64{aliceID}, []string{"Alice"}),
			"ReplaceMessageRecipients from")
		require.NoError(st.ReplaceMessageRecipients(mid, "to", []int64{bobID}, []string{"Bob"}),
			"ReplaceMessageRecipients to")
		require.NoError(st.ReplaceMessageLabels(mid, []int64{labelID}),
			"ReplaceMessageLabels")
	}

	eng := query.NewEngine(st.DB(), st.IsPostgreSQL())
	ctx := context.Background()

	// (1) Aggregate — must not error with "syntax error near ')'" on PG.
	t.Run("aggregate_senders", func(t *testing.T) {
		rows, err := eng.Aggregate(ctx, query.ViewSenders, query.AggregateOptions{
			SortField:     query.SortByCount,
			SortDirection: query.SortDesc,
			Limit:         50,
		})
		requirepkg.NoError(t, err, "Aggregate")
		requirepkg.NotEmpty(t, rows, "Aggregate returned no rows; expected at least the Alice sender bucket")
	})

	// (2) GetGmailIDsByFilter — must not error from a SELECT DISTINCT +
	// ORDER BY collision on PG. Use a label filter to exercise the
	// previously-multiplying join (now an EXISTS subquery).
	t.Run("gmail_ids_by_filter_label", func(t *testing.T) {
		ids, err := eng.GetGmailIDsByFilter(ctx, query.MessageFilter{
			SourceID: &src.ID,
			Label:    "Important",
			Sorting: query.MessageSorting{
				Field:     query.MessageSortByDate,
				Direction: query.SortDesc,
			},
		})
		requirepkg.NoError(t, err, "GetGmailIDsByFilter")
		assertpkg.Len(t, ids, 4, "label join must not multiply")
		// Confirm no duplicates after dropping DISTINCT — every message
		// row should appear exactly once because the label filter is an
		// EXISTS subquery, not a 1:N JOIN.
		seen := make(map[string]struct{}, len(ids))
		for _, id := range ids {
			_, dup := seen[id]
			assertpkg.False(t, dup, "duplicate id %q in result; EXISTS conversion broken", id)
			seen[id] = struct{}{}
		}
	})

	// (3) ListMessages sorted by size and subject — both previously
	// bound raw column references in ORDER BY that did not match the
	// COALESCE-wrapped expressions in the SELECT list. PG rejects that
	// combination under SELECT DISTINCT.
	for _, sort := range []struct {
		name  string
		field query.MessageSortField
	}{
		{"sort_by_size", query.MessageSortBySize},
		{"sort_by_subject", query.MessageSortBySubject},
		{"sort_by_date", query.MessageSortByDate},
	} {
		t.Run("list_messages_"+sort.name, func(t *testing.T) {
			msgs, err := eng.ListMessages(ctx, query.MessageFilter{
				SourceID: &src.ID,
				Sorting: query.MessageSorting{
					Field:     sort.field,
					Direction: query.SortDesc,
				},
				Pagination: query.Pagination{Limit: 50},
			})
			requirepkg.NoError(t, err, "ListMessages %s", sort.name)
			assertpkg.Len(t, msgs, 4, "ListMessages %s", sort.name)
		})
	}
}

// TestQueryEngine_CaseInsensitiveSearch_Subject verifies that
// `subject:` terms passed through query.Engine.Search match
// case-insensitively on both SQLite and PostgreSQL. SQLite's LIKE is
// ASCII-case-insensitive by default; PostgreSQL's LIKE is
// case-sensitive, so an unwrapped `m.subject LIKE ?` would mis-miss
// rows that the equivalent store API path (which lowercases) returns.
// Bare-LIKE divergence was H3 in the codex review.
func TestQueryEngine_CaseInsensitiveSearch_Subject(t *testing.T) {
	require := requirepkg.New(t)
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("gmail", "case-search@example.com")
	require.NoError(err, "GetOrCreateSource")
	convID, err := st.EnsureConversation(src.ID, "case-thread", "case thread")
	require.NoError(err, "EnsureConversation")
	mid, err := st.UpsertMessage(&store.Message{
		ConversationID:  convID,
		SourceID:        src.ID,
		SourceMessageID: "case-msg-1",
		MessageType:     "email",
		SentAt:          sql.NullTime{Time: time.Date(2024, 7, 1, 12, 0, 0, 0, time.UTC), Valid: true},
		Subject:         sql.NullString{String: "Quarterly Invoice", Valid: true},
		Snippet:         sql.NullString{String: "see attached", Valid: true},
		SizeEstimate:    1024,
	})
	require.NoError(err, "UpsertMessage")
	_ = mid

	eng := query.NewEngine(st.DB(), st.IsPostgreSQL())
	ctx := context.Background()

	for _, term := range []string{"invoice", "INVOICE", "Invoice"} {
		got, err := eng.Search(ctx,
			&search.Query{SubjectTerms: []string{term}}, 50, 0)
		require.NoError(err, "Search subject=%q", term)
		assertpkg.Len(t, got, 1, "subject:%q against stored subject %q", term, "Quarterly Invoice")
	}
}

// TestQueryEngine_MultiFromNoDuplication is the real regression guard for
// the from-side EXISTS conversion (dialect-parity-store-query-1). It builds
// ONE message that legitimately has TWO 'from' participants sharing both a
// domain and a display name — the exact shape that a plain 1:N
// message_recipients JOIN would multiply into two result rows once
// SELECT DISTINCT was dropped. It asserts that:
//
//   - ListMessages under filter.Domain returns exactly one row,
//   - ListMessages under filter.SenderName returns exactly one row, and
//   - SubAggregate (which shares buildFilterJoinsAndConditions) counts the
//     message once, not twice.
//
// Runs on whichever backend testutil.NewTestStore selects; setting
// MSGVAULT_TEST_DB to a postgres:// DSN exercises the PG path too, since
// NewPostgreSQLEngine wraps the same dialect-parameterized builder.
func TestQueryEngine_MultiFromNoDuplication(t *testing.T) {
	require := requirepkg.New(t)
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("gmail", "multifrom@example.com")
	require.NoError(err, "GetOrCreateSource")
	convID, err := st.EnsureConversation(src.ID, "thread-mf", "Thread MF")
	require.NoError(err, "EnsureConversation")

	// Two distinct 'from' participants that share a domain AND a display
	// name. A single message will carry both as 'from' rows.
	const dupDomain = "dup.example"
	const dupName = "Dup Sender"
	from1, err := st.EnsureParticipant("first@"+dupDomain, dupName, dupDomain)
	require.NoError(err, "EnsureParticipant from1")
	from2, err := st.EnsureParticipant("second@"+dupDomain, dupName, dupDomain)
	require.NoError(err, "EnsureParticipant from2")
	// An unrelated message in another domain so aggregates have >1 bucket.
	otherFrom, err := st.EnsureParticipant("other@other.example", "Other", "other.example")
	require.NoError(err, "EnsureParticipant other")

	base := time.Date(2024, 7, 1, 9, 0, 0, 0, time.UTC)
	multiID, err := st.UpsertMessage(&store.Message{
		ConversationID:  convID,
		SourceID:        src.ID,
		SourceMessageID: "gmail-multi-from",
		MessageType:     "email",
		SentAt:          sql.NullTime{Time: base, Valid: true},
		Subject:         sql.NullString{String: "multi from", Valid: true},
		Snippet:         sql.NullString{String: "snippet", Valid: true},
		SizeEstimate:    1234,
	})
	require.NoError(err, "UpsertMessage multi")
	// The crux: two 'from' rows on one message, both in dupDomain/dupName.
	require.NoError(st.ReplaceMessageRecipients(multiID, "from",
		[]int64{from1, from2}, []string{dupName, dupName}),
		"ReplaceMessageRecipients two from rows")

	otherID, err := st.UpsertMessage(&store.Message{
		ConversationID:  convID,
		SourceID:        src.ID,
		SourceMessageID: "gmail-other-from",
		MessageType:     "email",
		SentAt:          sql.NullTime{Time: base.Add(time.Hour), Valid: true},
		Subject:         sql.NullString{String: "other from", Valid: true},
		Snippet:         sql.NullString{String: "snippet", Valid: true},
		SizeEstimate:    1000,
	})
	require.NoError(err, "UpsertMessage other")
	require.NoError(st.ReplaceMessageRecipients(otherID, "from",
		[]int64{otherFrom}, []string{"Other"}), "ReplaceMessageRecipients other")

	eng := query.NewEngine(st.DB(), st.IsPostgreSQL())
	ctx := context.Background()

	t.Run("list_messages_domain", func(t *testing.T) {
		msgs, err := eng.ListMessages(ctx, query.MessageFilter{
			SourceID:   &src.ID,
			Domain:     dupDomain,
			Sorting:    query.MessageSorting{Field: query.MessageSortByDate, Direction: query.SortDesc},
			Pagination: query.Pagination{Limit: 50},
		})
		requirepkg.NoError(t, err, "ListMessages domain")
		assertpkg.Len(t, msgs, 1, "multi-from message must appear exactly once under Domain filter")
		assertpkg.Equal(t, multiID, msgs[0].ID, "the multi-from message")
	})

	t.Run("list_messages_sender_name", func(t *testing.T) {
		msgs, err := eng.ListMessages(ctx, query.MessageFilter{
			SourceID:   &src.ID,
			SenderName: dupName,
			Sorting:    query.MessageSorting{Field: query.MessageSortByDate, Direction: query.SortDesc},
			Pagination: query.Pagination{Limit: 50},
		})
		requirepkg.NoError(t, err, "ListMessages sender name")
		assertpkg.Len(t, msgs, 1, "multi-from message must appear exactly once under SenderName filter")
		assertpkg.Equal(t, multiID, msgs[0].ID, "the multi-from message")
	})

	// SubAggregate shares buildFilterJoinsAndConditions. Group by ViewTime
	// (one bucket per message's year — a single-valued grouping that does
	// not itself multiply) so the count isolates the Domain filter-join:
	// the multi-from message must be counted once, not once per 'from' row.
	t.Run("subaggregate_domain_count", func(t *testing.T) {
		rows, err := eng.SubAggregate(ctx,
			query.MessageFilter{SourceID: &src.ID, Domain: dupDomain},
			query.ViewTime,
			query.AggregateOptions{
				TimeGranularity: query.TimeYear,
				SortField:       query.SortByCount,
				SortDirection:   query.SortDesc,
				Limit:           50,
			},
		)
		requirepkg.NoError(t, err, "SubAggregate domain")
		var total int64
		for _, r := range rows {
			total += r.Count
		}
		assertpkg.Equal(t, int64(1), total,
			"Domain sub-filter must count the multi-from message once, not per 'from' row")
	})
}

func gmailSourceID(i int) string {
	return "gmail-msg-" + string(rune('a'+i))
}

func subjectFor(i int) string {
	switch i {
	case 0:
		return "alpha subject"
	case 1:
		return "beta subject"
	case 2:
		return "gamma subject"
	default:
		return "delta subject"
	}
}
