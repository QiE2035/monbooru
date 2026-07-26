package db

import (
	"context"
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

// InWriteTx runs work inside a write transaction, committing on success
// and rolling back via defer on any error path. work's first error
// short-circuits the commit.
func InWriteTx(w *sql.DB, work func(*sql.Tx) error) error {
	tx, err := w.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := work(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// Querier is the read surface both *sql.DB and *sql.Tx satisfy, so a
// helper can run against a pooled connection or an open transaction.
type Querier interface {
	Query(query string, args ...any) (*sql.Rows, error)
}

// CtxQuerier is Querier's context-carrying twin.
type CtxQuerier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// QueryIDs runs query and collects its single int64 column, folding the
// Query + error-guard + Close dance every id-list read repeats.
func QueryIDs(q Querier, query string, args ...any) ([]int64, error) {
	rows, err := q.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	return ScanIDs(rows)
}

// QueryIDsContext is QueryIDs on a cancellable read.
func QueryIDsContext(ctx context.Context, q CtxQuerier, query string, args ...any) ([]int64, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	return ScanIDs(rows)
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

// EscapeLike escapes the SQLite LIKE metacharacters (`_`, `%`) and the
// escape character (`\`) so operator-supplied input matches literally
// when concatenated with `%`/`_` wildcards. Callers must pair it with
// `ESCAPE '\'` on the LIKE clause.
func EscapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `_`, `\_`, `%`, `\%`)
	return r.Replace(s)
}
