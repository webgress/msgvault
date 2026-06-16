package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// TableVerify is the per-table outcome of VerifyMigration.
type TableVerify struct {
	Table         string
	SrcRows       int64
	DstRows       int64
	SrcMinID      int64 // identity-PK tables only; 0 otherwise
	SrcMaxID      int64
	DstMinID      int64
	DstMaxID      int64
	HasID         bool
	IDHashOK      bool // identity-PK tables only: full id-set hash matched
	ContentHashOK bool // full per-table content hash matched (all tables)
	OK            bool
	Mismatch      string // non-empty describes the first detected problem
}

// VerifyResult summarizes a VerifyMigration run.
type VerifyResult struct {
	Tables       []TableVerify
	FTSAvailable bool
	FTSBackfill  bool // true == FTS still needs backfilling (a problem)
	ExpectFTS    bool // caller expected FTS to be rebuilt on the destination
	Problems     []string
}

// OK reports whether every check passed.
func (r *VerifyResult) OK() bool {
	return len(r.Problems) == 0
}

// VerifyMigration cross-checks a completed migration: row-count parity for all
// tables, MIN/MAX id parity for identity-PK tables, a full per-table CONTENT
// hash for EVERY table (idless junctions included), destination referential
// integrity, FTS readiness, and (on PostgreSQL) that each identity sequence is
// at least MAX(id). It returns a report; callers decide how to surface it. Any
// detected problem is appended to Problems and makes OK() false.
//
// expectFTS tells the verifier whether the caller rebuilt FTS on the
// destination. When true, an unavailable FTS index is itself a problem; when
// false (the caller passed --no-rebuild-fts), FTS readiness is not asserted.
func VerifyMigration(ctx context.Context, src, dst *Store, expectFTS bool) (*VerifyResult, error) {
	res := &VerifyResult{ExpectFTS: expectFTS}

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

			// Content check: count + MIN/MAX can collide for a different
			// interior id-set, so hash the FULL ordered id set and compare.
			// This also catches corrupted id values not at the extremes.
			sHash, err := idSetHash(ctx, src, table)
			if err != nil {
				return nil, fmt.Errorf("id-set hash source %s: %w", table, err)
			}
			dHash, err := idSetHash(ctx, dst, table)
			if err != nil {
				return nil, fmt.Errorf("id-set hash dest %s: %w", table, err)
			}
			tv.IDHashOK = sHash == dHash
			if !tv.IDHashOK {
				tv.OK = false
				m := "id-set content hash mismatch"
				if tv.Mismatch == "" {
					tv.Mismatch = m
				}
				res.Problems = append(res.Problems,
					fmt.Sprintf("%s: %s", table, m))
			}
		}

		// Full per-table CONTENT hash over the SAME columns the copier copies
		// (neverCopyColumns excluded), computed in Go with cross-backend
		// canonicalization so a logically-identical row hashes the same on
		// SQLite and PostgreSQL. This catches corruption in NON-id columns and
		// covers idless junction tables that have no id-set check at all.
		sContent, err := tableContentHash(ctx, src, table)
		if err != nil {
			return nil, fmt.Errorf("content hash source %s: %w", table, err)
		}
		dContent, err := tableContentHash(ctx, dst, table)
		if err != nil {
			return nil, fmt.Errorf("content hash dest %s: %w", table, err)
		}
		tv.ContentHashOK = sContent == dContent
		if !tv.ContentHashOK {
			tv.OK = false
			m := "content hash mismatch"
			if tv.Mismatch == "" {
				tv.Mismatch = m
			}
			res.Problems = append(res.Problems,
				fmt.Sprintf("%s: %s", table, m))
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

	// FTS readiness: when the caller rebuilt FTS (expectFTS), the destination
	// must have FTS available and not need a backfill. An UNAVAILABLE index
	// when one was expected is itself a problem — otherwise --verify would pass
	// with no usable search index. When FTS was not expected (--no-rebuild-fts),
	// readiness is not asserted.
	res.FTSAvailable = dst.dialect.FTSAvailable(dst.DB())
	if res.FTSAvailable {
		res.FTSBackfill = dst.dialect.FTSNeedsBackfill(dst.DB())
		if res.FTSBackfill && expectFTS {
			res.Problems = append(res.Problems, "FTS still needs backfill on destination")
		}
	} else if expectFTS {
		res.Problems = append(res.Problems, "FTS index unavailable on destination (rebuild expected)")
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

// idSetHash returns a hash of the FULL ordered id set of table, so two tables
// with the same row count and MIN/MAX id but a different interior id-set (or a
// corrupted id) hash differently. The ordered id list is concatenated in the
// database (string_agg/group_concat ORDER BY id) and the resulting string is
// hashed in Go so the value is identical across SQLite and PostgreSQL backends.
func idSetHash(ctx context.Context, st *Store, table string) (string, error) {
	var query string
	if st.IsPostgreSQL() {
		// string_agg carries its own ORDER BY for deterministic ordering.
		query = "SELECT COALESCE(string_agg(id::text, ',' ORDER BY id), '') FROM " + table
	} else {
		// SQLite group_concat gained an ORDER BY argument only in 3.44, so order
		// the rows in a subquery to keep the concatenation deterministic.
		query = "SELECT COALESCE(group_concat(id, ','), '') FROM " +
			"(SELECT id FROM " + table + " ORDER BY id)"
	}
	var concat string
	if err := st.DB().QueryRowContext(ctx, query).Scan(&concat); err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(concat))
	return hex.EncodeToString(sum[:]), nil
}

// contentHashSkipColumns names columns excluded from the per-table content hash
// because their value is destination-local and is NOT expected to match
// cross-backend.
//
// applied_migrations is idempotent bookkeeping that InitSchema seeds on BOTH
// backends independently; the copier ALWAYS conflict-skips it (see flush()), so
// the destination keeps its OWN seeded applied_at, never the source's. Only the
// name PK is meaningful to compare here — hashing applied_at would spuriously
// fail every cross-backend verify.
var contentHashSkipColumns = map[string]map[string]bool{
	"applied_migrations": {"applied_at": true},
}

// tableContentHash streams every row of table (ordered by its natural key so
// the order is identical on both backends) and folds a canonical byte encoding
// of each kept column into a rolling SHA-256. The projection drops the same
// neverCopyColumns the copier drops (e.g. messages.search_fts), so the hash is
// computed over exactly the columns that crossed backends.
//
// Cross-backend equality is the whole point: the source is one backend and the
// destination the other, so a naive `md5(t::text)` in SQL would NOT match
// (PostgreSQL renders bool as t/f and JSONB with normalized spacing/key order,
// SQLite renders 0/1 and verbatim JSON text; timestamp spellings differ too).
// canonicalCol normalizes each value to a backend-independent form so a
// logically-identical row produces identical canonical bytes everywhere.
func tableContentHash(ctx context.Context, st *Store, table string) (string, error) {
	order := pkOrderColumn[table]
	if order == "" {
		order = "1"
	}
	rows, err := st.DB().QueryContext(ctx, "SELECT * FROM "+table+" ORDER BY "+order)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", table, err)
	}
	defer func() { _ = rows.Close() }()

	allCols, err := rows.Columns()
	if err != nil {
		return "", fmt.Errorf("columns %s: %w", table, err)
	}

	// Kept-column projection, matching copyTable's neverCopyColumns drop so the
	// verify hash spans exactly the copied column set. contentHashSkipColumns
	// additionally drops columns whose value is destination-local and therefore
	// never expected to match cross-backend (see its doc).
	never := neverCopyColumns[table]
	skip := contentHashSkipColumns[table]
	keepIdx := make([]int, 0, len(allCols))
	keepCol := make([]string, 0, len(allCols))
	for i, c := range allCols {
		if never[c] || skip[c] {
			continue
		}
		keepIdx = append(keepIdx, i)
		keepCol = append(keepCol, c)
	}

	scan := make([]any, len(allCols))
	ptrs := make([]any, len(allCols))
	for i := range scan {
		ptrs[i] = &scan[i]
	}

	h := sha256.New()
	var buf bytes.Buffer
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if err := rows.Scan(ptrs...); err != nil {
			return "", fmt.Errorf("scan %s: %w", table, err)
		}
		buf.Reset()
		for j, idx := range keepIdx {
			enc := canonicalCol(table, keepCol[j], scan[idx])
			// Length-prefix each field so column boundaries are unambiguous and
			// "ab"+"c" can never collide with "a"+"bc".
			buf.WriteString(strconv.Itoa(len(enc)))
			buf.WriteByte(':')
			buf.WriteString(enc)
			buf.WriteByte('|')
		}
		buf.WriteByte('\n')
		h.Write(buf.Bytes())
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("iterate %s: %w", table, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// canonicalCol renders a scanned value as a backend-independent canonical
// string for content hashing. The dynamic type a value scans to differs by
// backend (e.g. JSON: PG []byte vs SQLite string; legacy SQLite bool int64 vs
// PG bool), so this normalizes by both the column's declared semantics and the
// runtime type. It never errors: any unrecognized type degrades to a
// deterministic fmt.Sprintf form.
func canonicalCol(table, col string, v any) string {
	if v == nil {
		return "\x00NULL"
	}
	// Boolean columns: normalize 0/1 (legacy SQLite storage) and native bool to
	// a single t/f token so the two backends agree.
	if boolColumns[table][col] {
		switch t := v.(type) {
		case bool:
			return boolToken(t)
		case int64:
			return boolToken(t != 0)
		case int:
			return boolToken(t != 0)
		case []byte:
			return boolToken(len(t) == 1 && (t[0] == '1' || t[0] == 't' || t[0] == 'T'))
		case string:
			return boolToken(t == "1" || t == "t" || t == "true" || t == "TRUE")
		}
	}
	// JSON/JSONB columns: PG returns re-serialized []byte (sorted keys, spaced),
	// SQLite returns the verbatim TEXT. Parse and re-encode canonically so a
	// logically-identical document hashes the same regardless of source spelling.
	if jsonColumns[table][col] {
		switch t := v.(type) {
		case []byte:
			return canonicalJSON(t)
		case string:
			return canonicalJSON([]byte(t))
		}
	}
	switch t := v.(type) {
	case bool:
		return boolToken(t)
	case int64:
		return strconv.FormatInt(t, 10)
	case int:
		return strconv.FormatInt(int64(t), 10)
	case float64:
		return strconv.FormatFloat(t, 'g', -1, 64)
	case float32:
		return strconv.FormatFloat(float64(t), 'g', -1, 32)
	case time.Time:
		// Both drivers decode TIMESTAMP/TIMESTAMPTZ to time.Time. Normalize to
		// UTC at microsecond precision (PostgreSQL's timestamp resolution) so
		// the two backends agree on a single instant spelling.
		return t.UTC().Truncate(time.Microsecond).Format(time.RFC3339Nano)
	case []byte:
		// BYTEA / BLOB: hash the raw bytes.
		return "\x01" + hex.EncodeToString(t)
	case string:
		return t
	default:
		return fmt.Sprintf("%v", t)
	}
}

// boolToken maps a Go bool to the canonical single-char token used for hashing.
func boolToken(b bool) string {
	if b {
		return "t"
	}
	return "f"
}

// canonicalJSON parses raw JSON and re-encodes it with sorted object keys and no
// insignificant whitespace, so PostgreSQL's JSONB normalization and SQLite's
// verbatim JSON text reduce to the same bytes. Numbers are preserved as their
// literal tokens (json.Number) to avoid float-formatting drift. Invalid JSON
// (or non-JSON TEXT that happens to live in a JSON column) falls back to the
// trimmed raw string so the hash is still deterministic. It never errors: any
// parse/marshal failure degrades to the deterministic raw-text form.
func canonicalJSON(raw []byte) string {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var doc any
	if err := dec.Decode(&doc); err != nil {
		// Not valid JSON: hash the raw text verbatim (deterministic either way).
		return string(bytes.TrimSpace(raw))
	}
	out, err := json.Marshal(doc) // encoding/json sorts map keys
	if err != nil {
		return string(bytes.TrimSpace(raw))
	}
	return string(out)
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

// pgForeignKey describes one foreign-key column edge discovered from the PG
// catalog: child.childCol -> parent.parentCol, under constraint name.
type pgForeignKey struct {
	constraint string
	childTable string
	childCol   string
	parentTbl  string
	parentCol  string
}

// pgForeignKeys enumerates every foreign-key column edge in the destination's
// current schema (search_path). It joins table_constraints,
// key_column_usage, and constraint_column_usage so all FK edges are covered,
// matching SQLite's PRAGMA foreign_key_check completeness rather than a hand
// curated subset. Composite FKs surface as one row per column, which is fine —
// each column edge gets its own orphan probe.
func pgForeignKeys(ctx context.Context, dst *Store) ([]pgForeignKey, error) {
	const q = `
SELECT tc.constraint_name,
       kcu.table_name  AS child_table,
       kcu.column_name AS child_col,
       ccu.table_name  AS parent_table,
       ccu.column_name AS parent_col
FROM information_schema.table_constraints tc
JOIN information_schema.key_column_usage kcu
  ON tc.constraint_name = kcu.constraint_name
 AND tc.constraint_schema = kcu.constraint_schema
JOIN information_schema.constraint_column_usage ccu
  ON tc.constraint_name = ccu.constraint_name
 AND tc.constraint_schema = ccu.constraint_schema
WHERE tc.constraint_type = 'FOREIGN KEY'
  AND tc.table_schema = current_schema()
ORDER BY tc.constraint_name, kcu.ordinal_position`
	rows, err := dst.DB().QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("enumerate foreign keys: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var fks []pgForeignKey
	for rows.Next() {
		var fk pgForeignKey
		if err := rows.Scan(&fk.constraint, &fk.childTable, &fk.childCol,
			&fk.parentTbl, &fk.parentCol); err != nil {
			return nil, fmt.Errorf("scan foreign key: %w", err)
		}
		fks = append(fks, fk)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate foreign keys: %w", err)
	}
	return fks, nil
}

// pgOrphanChecks checks EVERY foreign-key edge on a PostgreSQL destination for
// orphan rows, generating the probes dynamically from the catalog. PostgreSQL
// enforces FKs at insert time, so a non-zero count here indicates a genuine
// copy defect (or a constraint that was somehow disabled).
func pgOrphanChecks(ctx context.Context, dst *Store) ([]string, error) {
	fks, err := pgForeignKeys(ctx, dst)
	if err != nil {
		return nil, err
	}
	var problems []string
	for _, fk := range fks {
		child := pgQuoteIdent(fk.childTable)
		childCol := pgQuoteIdent(fk.childCol)
		parent := pgQuoteIdent(fk.parentTbl)
		parentCol := pgQuoteIdent(fk.parentCol)
		// A NULL FK value is not an orphan (the reference is simply absent), so
		// only probe non-NULL child columns.
		query := fmt.Sprintf(
			"SELECT COUNT(*) FROM %s c WHERE c.%s IS NOT NULL AND NOT EXISTS "+
				"(SELECT 1 FROM %s p WHERE p.%s = c.%s)",
			child, childCol, parent, parentCol, childCol)
		var n int64
		if err := dst.DB().QueryRowContext(ctx, query).Scan(&n); err != nil {
			return nil, fmt.Errorf("orphan check %s (%s.%s->%s.%s): %w",
				fk.constraint, fk.childTable, fk.childCol, fk.parentTbl, fk.parentCol, err)
		}
		if n > 0 {
			problems = append(problems, fmt.Sprintf(
				"%d orphan rows in %s.%s->%s.%s",
				n, fk.childTable, fk.childCol, fk.parentTbl, fk.parentCol))
		}
	}
	return problems, nil
}

// pgQuoteIdent wraps a PostgreSQL identifier in double quotes, doubling any
// embedded quotes. The identifiers here come from the catalog (not user input),
// but quoting keeps the generated SQL safe and handles reserved words.
func pgQuoteIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

// pgSequenceAtLeastMax reports whether table's identity sequence is positioned
// so the next insert yields an id strictly greater than the current MAX(id).
//
// "Positioned" accounts for is_called: a sequence's effective next value is
// last_value + (is_called ? 1 : 0). An UNCALLED sequence at last_value=N hands
// out N next, so for an empty table we want last_value=1 is_called=false
// (effective next = 1). For a non-empty table we want effective next > MAX(id).
// An empty table is trivially OK.
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
	// Resolve a fully-qualified, quote-safe relation name for the sequence from
	// the catalog (keyed by its regclass oid), so the name is never interpolated
	// raw. last_value and is_called are only available by selecting from the
	// sequence relation itself (pg_sequences omits is_called).
	var qname string
	if err := dst.DB().QueryRowContext(ctx,
		"SELECT quote_ident(n.nspname)||'.'||quote_ident(c.relname) "+
			"FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace "+
			"WHERE c.oid = $1::regclass", seq.String).Scan(&qname); err != nil {
		return false, "", fmt.Errorf("resolve sequence name %s: %w", table, err)
	}
	var lastVal int64
	var isCalled bool
	if err := dst.DB().QueryRowContext(ctx,
		"SELECT last_value, is_called FROM "+qname).Scan(&lastVal, &isCalled); err != nil {
		return false, "", fmt.Errorf("sequence state %s: %w", table, err)
	}
	effectiveNext := lastVal
	if isCalled {
		effectiveNext = lastVal + 1
	}
	if effectiveNext <= maxID {
		return false, fmt.Sprintf("effective_next=%d (last_value=%d is_called=%t) max_id=%d",
			effectiveNext, lastVal, isCalled, maxID), nil
	}
	return true, "", nil
}
