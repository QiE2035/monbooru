package db

import (
	"database/sql"
	"strings"
)

// Chunked walks xs in fixed-size slices, calling fn on each. The
// callback receives a backing-array view, so any retained reference
// (e.g. via append) must be copied. Used by query loops that batch IN
// clauses below SQLite's parameter cap; the per-chunk progress / job
// cancellation hook lives in the web-package chunkedJob.
func Chunked[T any](xs []T, chunkSize int, fn func(chunk []T) error) error {
	for start := 0; start < len(xs); start += chunkSize {
		end := start + chunkSize
		if end > len(xs) {
			end = len(xs)
		}
		if err := fn(xs[start:end]); err != nil {
			return err
		}
	}
	return nil
}

// ScanIDs walks rows and collects an []int64 column. Rows is closed by
// the caller; this helper does not close it so the caller can compose
// scans against the same rows iterator if needed.
func ScanIDs(rows *sql.Rows) ([]int64, error) {
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// InPlaceholders builds the `?,?,?` body for a SQL `IN (...)` clause and
// the matching []any argument slice in one pass. Returns ("", nil) on
// an empty input; callers should guard their IN clause against that
// since SQLite rejects `IN ()`.
func InPlaceholders[T any](xs []T) (string, []any) {
	if len(xs) == 0 {
		return "", nil
	}
	args := make([]any, len(xs))
	for i, x := range xs {
		args[i] = x
	}
	return strings.Repeat("?,", len(xs)-1) + "?", args
}
