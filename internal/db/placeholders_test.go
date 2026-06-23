package db

import (
	"reflect"
	"testing"
)

// TestInPlaceholders pins the load-bearing empty-input contract
// (InPlaceholders([]) -> ("", nil)), which lets callers skip emitting a
// SQLite-rejected `IN ()`, plus the single / many shapes and the
// matching []any args slice.
func TestInPlaceholders(t *testing.T) {
	t.Run("empty returns blank body and nil args", func(t *testing.T) {
		body, args := InPlaceholders([]int64{})
		if body != "" {
			t.Errorf("body = %q, want %q", body, "")
		}
		if args != nil {
			t.Errorf("args = %#v, want nil", args)
		}
	})

	t.Run("nil slice returns blank body and nil args", func(t *testing.T) {
		var xs []int64
		body, args := InPlaceholders(xs)
		if body != "" {
			t.Errorf("body = %q, want %q", body, "")
		}
		if args != nil {
			t.Errorf("args = %#v, want nil", args)
		}
	})

	t.Run("single element has no trailing comma", func(t *testing.T) {
		body, args := InPlaceholders([]int64{7})
		if body != "?" {
			t.Errorf("body = %q, want %q", body, "?")
		}
		if !reflect.DeepEqual(args, []any{int64(7)}) {
			t.Errorf("args = %#v, want [7]", args)
		}
	})

	t.Run("many elements join with commas and no trailing comma", func(t *testing.T) {
		body, args := InPlaceholders([]int64{1, 2, 3})
		if body != "?,?,?" {
			t.Errorf("body = %q, want %q", body, "?,?,?")
		}
		if !reflect.DeepEqual(args, []any{int64(1), int64(2), int64(3)}) {
			t.Errorf("args = %#v, want [1 2 3]", args)
		}
	})

	t.Run("preserves element type and order with strings", func(t *testing.T) {
		body, args := InPlaceholders([]string{"a", "b"})
		if body != "?,?" {
			t.Errorf("body = %q, want %q", body, "?,?")
		}
		if !reflect.DeepEqual(args, []any{"a", "b"}) {
			t.Errorf("args = %#v, want [a b]", args)
		}
	})
}

// TestChunked walks the boundary sizes: empty (no calls), single,
// exact-multiple, and non-multiple (short trailing chunk). It also
// asserts the chunks reconstruct the input in order and that an error
// from the callback short-circuits the walk.
func TestChunked(t *testing.T) {
	collect := func(xs []int, chunkSize int) [][]int {
		var got [][]int
		err := Chunked(xs, chunkSize, func(chunk []int) error {
			// Copy: the callback receives a backing-array view.
			cp := append([]int(nil), chunk...)
			got = append(got, cp)
			return nil
		})
		if err != nil {
			t.Fatalf("Chunked: %v", err)
		}
		return got
	}

	t.Run("empty input makes no calls", func(t *testing.T) {
		got := collect([]int{}, 3)
		if len(got) != 0 {
			t.Errorf("chunks = %v, want none", got)
		}
	})

	t.Run("single element single chunk", func(t *testing.T) {
		got := collect([]int{9}, 3)
		want := [][]int{{9}}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("chunks = %v, want %v", got, want)
		}
	})

	t.Run("exact multiple of chunk size", func(t *testing.T) {
		got := collect([]int{1, 2, 3, 4}, 2)
		want := [][]int{{1, 2}, {3, 4}}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("chunks = %v, want %v", got, want)
		}
	})

	t.Run("non-multiple leaves a short trailing chunk", func(t *testing.T) {
		got := collect([]int{1, 2, 3, 4, 5}, 2)
		want := [][]int{{1, 2}, {3, 4}, {5}}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("chunks = %v, want %v", got, want)
		}
	})

	t.Run("chunk larger than input yields one chunk", func(t *testing.T) {
		got := collect([]int{1, 2}, 10)
		want := [][]int{{1, 2}}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("chunks = %v, want %v", got, want)
		}
	})

	t.Run("callback error short-circuits and propagates", func(t *testing.T) {
		wantErr := errSentinel
		calls := 0
		err := Chunked([]int{1, 2, 3, 4, 5}, 2, func(chunk []int) error {
			calls++
			return wantErr
		})
		if err != wantErr {
			t.Errorf("err = %v, want %v", err, wantErr)
		}
		if calls != 1 {
			t.Errorf("calls = %d, want 1 (should stop on first error)", calls)
		}
	})
}

var errSentinel = sentinelErr("boom")

type sentinelErr string

func (e sentinelErr) Error() string { return string(e) }

// TestScanIDs runs the helper against a real SQLite rows iterator for
// zero, one, and many rows, and confirms a scan over a non-int64 column
// surfaces the error.
func TestScanIDs(t *testing.T) {
	database := openTestDB(t)
	if err := Bootstrap(database); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	for _, id := range []int64{10, 20, 30} {
		if _, err := database.Write.Exec(
			`INSERT INTO images (id, sha256, canonical_path, file_type, file_size) VALUES (?, ?, ?, 'png', 1)`,
			id, "sha"+string(rune('a'+id)), "/p/"+string(rune('a'+id)),
		); err != nil {
			t.Fatalf("seed image %d: %v", id, err)
		}
	}

	t.Run("empty result set returns nil slice", func(t *testing.T) {
		rows, err := database.Read.Query(`SELECT id FROM images WHERE id < 0 ORDER BY id`)
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		defer func() { _ = rows.Close() }()
		ids, err := ScanIDs(rows)
		if err != nil {
			t.Fatalf("ScanIDs: %v", err)
		}
		if ids != nil {
			t.Errorf("ids = %#v, want nil for empty result", ids)
		}
	})

	t.Run("single row", func(t *testing.T) {
		rows, err := database.Read.Query(`SELECT id FROM images WHERE id = 20`)
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		defer func() { _ = rows.Close() }()
		ids, err := ScanIDs(rows)
		if err != nil {
			t.Fatalf("ScanIDs: %v", err)
		}
		if !reflect.DeepEqual(ids, []int64{20}) {
			t.Errorf("ids = %v, want [20]", ids)
		}
	})

	t.Run("many rows in query order", func(t *testing.T) {
		rows, err := database.Read.Query(`SELECT id FROM images ORDER BY id`)
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		defer func() { _ = rows.Close() }()
		ids, err := ScanIDs(rows)
		if err != nil {
			t.Fatalf("ScanIDs: %v", err)
		}
		if !reflect.DeepEqual(ids, []int64{10, 20, 30}) {
			t.Errorf("ids = %v, want [10 20 30]", ids)
		}
	})

	t.Run("non-integer column surfaces a scan error", func(t *testing.T) {
		rows, err := database.Read.Query(`SELECT canonical_path FROM images ORDER BY id`)
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		defer func() { _ = rows.Close() }()
		if _, err := ScanIDs(rows); err == nil {
			t.Error("expected scan error on TEXT column, got nil")
		}
	})
}

// TestEscapeLike covers each LIKE metacharacter plus the escape char,
// asserts plain input is untouched, and verifies the escaped pattern
// actually matches literally through SQLite's `ESCAPE '\'` clause.
func TestEscapeLike(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"plain text untouched", "hello", "hello"},
		{"percent escaped", "50%", `50\%`},
		{"underscore escaped", "a_b", `a\_b`},
		{"backslash escaped", `a\b`, `a\\b`},
		{"all metacharacters together", `a_b%c\d`, `a\_b\%c\\d`},
		{"escape char doubled before metachar", `\%`, `\\\%`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := EscapeLike(c.in); got != c.want {
				t.Errorf("EscapeLike(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}

	// End-to-end: an escaped pattern paired with ESCAPE '\' must match the
	// literal string and only it - the wildcards must not run wild.
	t.Run("escaped pattern matches literally under ESCAPE clause", func(t *testing.T) {
		database := openTestDB(t)
		if err := Bootstrap(database); err != nil {
			t.Fatalf("Bootstrap: %v", err)
		}
		// "a_b" must match literally; "axb" (where _ would be a wildcard)
		// must not. Both carry a canonical_path so the LIKE has something
		// to bite on, but we match on the sha256.
		for i, sha := range []string{"a_b", "axb"} {
			if _, err := database.Write.Exec(
				`INSERT INTO images (sha256, canonical_path, file_type, file_size) VALUES (?, ?, 'png', 1)`,
				sha, "/p/"+string(rune('a'+i)),
			); err != nil {
				t.Fatalf("seed %q: %v", sha, err)
			}
		}
		pattern := EscapeLike("a_b") + "%"
		rows, err := database.Read.Query(
			`SELECT sha256 FROM images WHERE sha256 LIKE ? ESCAPE '\' ORDER BY sha256`, pattern,
		)
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		defer func() { _ = rows.Close() }()
		var got []string
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				t.Fatalf("scan: %v", err)
			}
			got = append(got, s)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows.Err: %v", err)
		}
		if !reflect.DeepEqual(got, []string{"a_b"}) {
			t.Errorf("matched %v, want only [a_b] (underscore must be literal, not a wildcard)", got)
		}
	})
}
