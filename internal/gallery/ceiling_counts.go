package gallery

import (
	"database/sql"
	"fmt"

	"github.com/leqwin/monbooru/internal/db"
)

// excludeNotExists returns the `AND NOT EXISTS (...)` fragment that
// gates a per-image scan on the absence of any tag in excludeIDs. The
// fragment is empty when excludeIDs is empty so callers can splice it
// straight into an existing WHERE clause without juggling the
// boundary.
//
// The bound column name is the caller's responsibility - some
// aggregates query `images i` (column `i.id`), others use a join with
// no alias. Keeping the join column out of the helper avoids a second
// flavour of the same predicate when a new aggregate joins under a
// different alias.
func excludeNotExists(imageCol string, excludeIDs []int64) (string, []any) {
	if len(excludeIDs) == 0 {
		return "", nil
	}
	placeholders, args := db.InPlaceholders(excludeIDs)
	return ` AND NOT EXISTS (SELECT 1 FROM image_tags it WHERE it.image_id = ` + imageCol + ` AND it.tag_id IN (` + placeholders + `))`, args
}

// scalarCountUnder is the shared COUNT(*) shape behind the four
// *Under accessors. baseWhere is the per-counter predicate; the
// ceiling NOT EXISTS leg is appended only when excludeIDs is set.
func scalarCountUnder(database *db.DB, baseWhere string, excludeIDs []int64) (int, error) {
	where, args := excludeNotExists("i.id", excludeIDs)
	var n int
	if err := database.Read.QueryRow(
		`SELECT COUNT(*) FROM images i WHERE `+baseWhere+where, args...,
	).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// InboxCountUnder counts non-missing inbox images (is_inbox = 1),
// dropping any whose tag list intersects excludeIDs (the rating ceiling).
func InboxCountUnder(database *db.DB, excludeIDs []int64) (int, error) {
	return scalarCountUnder(database, "i.is_missing = 0 AND i.is_inbox = 1", excludeIDs)
}

// PhashMissingUnder is the relations-hub "PhashMissing" analogue:
// non-missing images with NULL phash, minus the ceiling-hidden ones.
func PhashMissingUnder(database *db.DB, excludeIDs []int64) (int, error) {
	return scalarCountUnder(database, "i.phash IS NULL AND i.is_missing = 0", excludeIDs)
}

// topLabelCountsUnder is the shared SELECT col, COUNT(*) shape behind
// the four top-N sidebar queries. convert builds the per-caller
// struct so the helper stays type-free. limit <= 0 defaults to 25.
func topLabelCountsUnder[T any](
	database *db.DB,
	col string,
	excludeIDs []int64,
	limit int,
	convert func(label string, count int) T,
) ([]T, error) {
	if limit <= 0 {
		limit = 25
	}
	where, args := excludeNotExists("i.id", excludeIDs)
	args = append(args, limit)
	rows, err := database.Read.Query(
		`SELECT i.`+col+`, COUNT(*) c FROM images i
		 WHERE i.is_missing = 0 AND i.`+col+` != ''`+where+`
		 GROUP BY i.`+col+` ORDER BY c DESC, i.`+col+` ASC LIMIT ?`,
		args...,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []T
	for rows.Next() {
		var label string
		var count int
		if err := rows.Scan(&label, &count); err != nil {
			return out, err
		}
		out = append(out, convert(label, count))
	}
	return out, rows.Err()
}

// SourceLabelCountsUnderQuery mirrors SourceLabelCountsQuery with the
// ceiling predicate.
func SourceLabelCountsUnderQuery(database *db.DB, limit int, excludeIDs []int64) ([]SourceLabelCount, error) {
	return topLabelCountsUnder(database, "source", excludeIDs, limit,
		func(label string, count int) SourceLabelCount { return SourceLabelCount{Source: label, Count: count} })
}

// FolderTreeUnder mirrors FolderTree with the ceiling predicate folded
// into the per-folder GROUP BY. The post-processing (ancestor
// reconstruction, count rollup, pointer-to-value tree copy) reuses the
// same code as the blind variant via the small helper below so the
// two stay in sync.
func FolderTreeUnder(database *db.DB, excludeIDs []int64) ([]FolderNode, error) {
	if len(excludeIDs) == 0 {
		return FolderTree(database)
	}
	where, args := excludeNotExists("i.id", excludeIDs)
	rows, err := database.Read.Query(
		`SELECT COALESCE(i.folder_path, ''), COUNT(*) FROM images i
		 WHERE i.is_missing = 0`+where+`
		 GROUP BY i.folder_path ORDER BY i.folder_path`,
		args...,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	flat, err := scanFolderRows(rows)
	if err != nil {
		return nil, err
	}
	return buildFolderTree(flat), nil
}

type folderCount struct {
	path  string
	count int
}

// scanFolderRows drains the (path, count) result set produced by
// FolderTree's and FolderTreeUnder's SELECTs.
func scanFolderRows(rows *sql.Rows) ([]folderCount, error) {
	var flat []folderCount
	for rows.Next() {
		var fc folderCount
		if err := rows.Scan(&fc.path, &fc.count); err != nil {
			return nil, fmt.Errorf("scanning folder row: %w", err)
		}
		flat = append(flat, fc)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return flat, nil
}
