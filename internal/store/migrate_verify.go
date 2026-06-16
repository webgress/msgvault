package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// TableVerify is the per-table outcome of VerifyMigration.
type TableVerify struct {
	Table    string
	SrcRows  int64
	DstRows  int64
	SrcMinID int64 // identity-PK tables only; 0 otherwise
	SrcMaxID int64
	DstMinID int64
	DstMaxID int64
	HasID    bool
	OK       bool
	Mismatch string // non-empty describes the first detected problem
}

// VerifyResult summarizes a VerifyMigration run.
type VerifyResult struct {
	Tables       []TableVerify
	FTSAvailable bool
	FTSBackfill  bool // true == FTS still needs backfilling (a problem)
	Problems     []string
}

// OK reports whether every check passed.
func (r *VerifyResult) OK() bool {
	return len(r.Problems) == 0
}

// VerifyMigration cross-checks a completed migration: row-count parity for all
// tables, MIN/MAX id parity for identity-PK tables, destination referential
// integrity, FTS readiness, and (on PostgreSQL) that each identity sequence is
// at least MAX(id). It returns a report; callers decide how to surface it. Any
// detected problem is appended to Problems and makes OK() false.
func VerifyMigration(ctx context.Context, src, dst *Store) (*VerifyResult, error) {
	res := &VerifyResult{}

	idTables := make(map[string]bool, len(identityPKTables))
	for _, t := range identityPKTables {
		idTables[t] = true
	}

	for _, table := range migrateTableOrder {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		tv := TableVerify{Table: table, OK: true}

		srcN, err := countRows(ctx, src, table)
		if err != nil {
			return nil, fmt.Errorf("count source %s: %w", table, err)
		}
		dstN, err := countRows(ctx, dst, table)
		if err != nil {
			return nil, fmt.Errorf("count dest %s: %w", table, err)
		}
		tv.SrcRows, tv.DstRows = srcN, dstN
		if srcN != dstN {
			tv.OK = false
			tv.Mismatch = fmt.Sprintf("row count %d != %d", srcN, dstN)
			res.Problems = append(res.Problems,
				fmt.Sprintf("%s: %s", table, tv.Mismatch))
		}

		if idTables[table] {
			tv.HasID = true
			sMin, sMax, err := idRange(ctx, src, table)
			if err != nil {
				return nil, fmt.Errorf("id range source %s: %w", table, err)
			}
			dMin, dMax, err := idRange(ctx, dst, table)
			if err != nil {
				return nil, fmt.Errorf("id range dest %s: %w", table, err)
			}
			tv.SrcMinID, tv.SrcMaxID = sMin, sMax
			tv.DstMinID, tv.DstMaxID = dMin, dMax
			if sMin != dMin || sMax != dMax {
				tv.OK = false
				m := fmt.Sprintf("id range [%d,%d] != [%d,%d]", sMin, sMax, dMin, dMax)
				if tv.Mismatch == "" {
					tv.Mismatch = m
				}
				res.Problems = append(res.Problems,
					fmt.Sprintf("%s: %s", table, m))
			}
		}

		res.Tables = append(res.Tables, tv)
	}

	// Destination referential integrity.
	if dst.IsPostgreSQL() {
		if orphans, err := pgOrphanChecks(ctx, dst); err != nil {
			return nil, err
		} else if len(orphans) > 0 {
			res.Problems = append(res.Problems, orphans...)
		}
	} else {
		if violations, err := sqliteForeignKeyCheck(ctx, dst); err != nil {
			return nil, err
		} else if len(violations) > 0 {
			res.Problems = append(res.Problems,
				"foreign key violations: "+strings.Join(violations, "; "))
		}
	}

	// FTS readiness: the destination should have FTS available and not need a
	// backfill (the migrate command rebuilds FTS before verifying).
	res.FTSAvailable = dst.dialect.FTSAvailable(dst.DB())
	if res.FTSAvailable {
		res.FTSBackfill = dst.dialect.FTSNeedsBackfill(dst.DB())
		if res.FTSBackfill {
			res.Problems = append(res.Problems, "FTS still needs backfill on destination")
		}
	}

	// PostgreSQL sequence sanity: each identity sequence >= MAX(id).
	if dst.IsPostgreSQL() {
		for _, table := range identityPKTables {
			ok, detail, err := pgSequenceAtLeastMax(ctx, dst, table)
			if err != nil {
				return nil, err
			}
			if !ok {
				res.Problems = append(res.Problems,
					fmt.Sprintf("%s sequence behind max id: %s", table, detail))
			}
		}
	}

	return res, nil
}

// idRange returns COALESCE(MIN(id),0), COALESCE(MAX(id),0) for table.
func idRange(ctx context.Context, st *Store, table string) (int64, int64, error) {
	var lo, hi int64
	err := st.DB().QueryRowContext(ctx,
		"SELECT COALESCE(MIN(id),0), COALESCE(MAX(id),0) FROM "+table).Scan(&lo, &hi)
	if err != nil {
		return 0, 0, err
	}
	return lo, hi, nil
}

// sqliteForeignKeyCheck runs PRAGMA foreign_key_check and returns any
// violations as "table(rowid)->parent" strings.
func sqliteForeignKeyCheck(ctx context.Context, dst *Store) ([]string, error) {
	rows, err := dst.DB().QueryContext(ctx, "PRAGMA foreign_key_check")
	if err != nil {
		return nil, fmt.Errorf("foreign_key_check: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var violations []string
	for rows.Next() {
		var table, rowid, parent, fkid sql.NullString
		if err := rows.Scan(&table, &rowid, &parent, &fkid); err != nil {
			return nil, fmt.Errorf("scan foreign_key_check: %w", err)
		}
		violations = append(violations,
			fmt.Sprintf("%s(rowid=%s)->%s", table.String, rowid.String, parent.String))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate foreign_key_check: %w", err)
	}
	return violations, nil
}

// pgOrphanChecks spot-checks the most load-bearing FK edges on a PostgreSQL
// destination for orphan rows. PostgreSQL enforces FKs at insert time, so a
// non-zero count here indicates a genuine copy defect.
func pgOrphanChecks(ctx context.Context, dst *Store) ([]string, error) {
	checks := []struct {
		name, query string
	}{
		{"messages->conversations",
			"SELECT COUNT(*) FROM messages m WHERE NOT EXISTS (SELECT 1 FROM conversations c WHERE c.id = m.conversation_id)"},
		{"messages->sources",
			"SELECT COUNT(*) FROM messages m WHERE NOT EXISTS (SELECT 1 FROM sources s WHERE s.id = m.source_id)"},
		{"messages.reply_to->messages",
			"SELECT COUNT(*) FROM messages m WHERE m.reply_to_message_id IS NOT NULL AND NOT EXISTS (SELECT 1 FROM messages p WHERE p.id = m.reply_to_message_id)"},
		{"message_recipients->messages",
			"SELECT COUNT(*) FROM message_recipients mr WHERE NOT EXISTS (SELECT 1 FROM messages m WHERE m.id = mr.message_id)"},
		{"attachments->messages",
			"SELECT COUNT(*) FROM attachments a WHERE NOT EXISTS (SELECT 1 FROM messages m WHERE m.id = a.message_id)"},
		{"message_labels->labels",
			"SELECT COUNT(*) FROM message_labels ml WHERE NOT EXISTS (SELECT 1 FROM labels l WHERE l.id = ml.label_id)"},
	}
	var problems []string
	for _, c := range checks {
		var n int64
		if err := dst.DB().QueryRowContext(ctx, c.query).Scan(&n); err != nil {
			return nil, fmt.Errorf("orphan check %s: %w", c.name, err)
		}
		if n > 0 {
			problems = append(problems, fmt.Sprintf("%d orphan rows in %s", n, c.name))
		}
	}
	return problems, nil
}

// pgSequenceAtLeastMax reports whether table's identity sequence last_value is
// at least MAX(id). An empty table is trivially OK.
func pgSequenceAtLeastMax(ctx context.Context, dst *Store, table string) (bool, string, error) {
	var seq sql.NullString
	if err := dst.DB().QueryRowContext(ctx,
		"SELECT pg_get_serial_sequence($1, 'id')", table).Scan(&seq); err != nil {
		return false, "", fmt.Errorf("get sequence %s: %w", table, err)
	}
	if !seq.Valid || seq.String == "" {
		return true, "no sequence", nil
	}
	var maxID int64
	if err := dst.DB().QueryRowContext(ctx,
		"SELECT COALESCE(MAX(id),0) FROM "+table).Scan(&maxID); err != nil {
		return false, "", fmt.Errorf("max id %s: %w", table, err)
	}
	if maxID == 0 {
		return true, "empty table", nil
	}
	var lastVal int64
	if err := dst.DB().QueryRowContext(ctx,
		"SELECT last_value FROM "+seq.String).Scan(&lastVal); err != nil {
		return false, "", fmt.Errorf("sequence last_value %s: %w", table, err)
	}
	if lastVal < maxID {
		return false, fmt.Sprintf("last_value=%d max_id=%d", lastVal, maxID), nil
	}
	return true, "", nil
}
