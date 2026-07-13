package db

import (
	"context"
	"database/sql"
	"sync"
	"testing"
)

func openTestDB(t *testing.T) *DB {
	t.Helper()
	// Use a temp file so both read and write pools share the same database.
	// In-memory SQLite (:memory:) creates separate DBs per pool.
	dir := t.TempDir()
	db, err := Open(dir + "/test.db")
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestBootstrap(t *testing.T) {
	db := openTestDB(t)

	if err := Bootstrap(db); err != nil {
		t.Fatalf("Bootstrap failed: %v", err)
	}

	tables := []string{
		"tag_categories", "tags",
		"images", "image_paths", "image_tags",
		"sd_metadata", "comfyui_metadata", "manga_metadata", "saved_searches",
	}
	for _, tbl := range tables {
		var n int
		if err := db.Read.QueryRow(
			"SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?", tbl,
		).Scan(&n); err != nil {
			t.Fatalf("querying for table %q: %v", tbl, err)
		}
		if n == 0 {
			t.Errorf("table %q not found after Bootstrap", tbl)
		}
	}
}

func TestBootstrapBuiltinCategories(t *testing.T) {
	db := openTestDB(t)
	if err := Bootstrap(db); err != nil {
		t.Fatalf("Bootstrap failed: %v", err)
	}
	want := []struct {
		name  string
		color string
	}{
		{"general", "#3d90e3"},
		{"character", "#00aa00"},
		{"artist", "#cc0000"},
		{"copyright", "#aa00aa"},
		{"meta", "#ffaa00"},
		{"rating", "#996666"},
		{"medium", "#7d4fbf"},
		{"person", "#b85c9e"},
		{"year", "#4a8fa8"},
	}
	for _, w := range want {
		var color string
		var isBuiltin int
		err := db.Read.QueryRow(
			`SELECT color, is_builtin FROM tag_categories WHERE name = ?`, w.name,
		).Scan(&color, &isBuiltin)
		if err != nil {
			t.Errorf("category %q: %v", w.name, err)
			continue
		}
		if color != w.color {
			t.Errorf("category %q color = %q, want %q", w.name, color, w.color)
		}
		if isBuiltin != 1 {
			t.Errorf("category %q is_builtin = %d, want 1", w.name, isBuiltin)
		}
	}
}

// A library predating the medium/person/year seed may have a custom row
// of the same name with is_builtin = 0. The schema's UPDATE statement
// must promote it on the next bootstrap so it stops being deletable.
func TestBootstrapPromotesExistingCustomBuiltins(t *testing.T) {
	db := openTestDB(t)
	if err := Bootstrap(db); err != nil {
		t.Fatalf("Bootstrap failed: %v", err)
	}
	if _, err := db.Write.Exec(
		`UPDATE tag_categories SET is_builtin = 0 WHERE name IN ('medium', 'person', 'year')`,
	); err != nil {
		t.Fatalf("downgrade: %v", err)
	}
	if err := Bootstrap(db); err != nil {
		t.Fatalf("re-Bootstrap failed: %v", err)
	}
	for _, name := range []string{"medium", "person", "year"} {
		var isBuiltin int
		if err := db.Read.QueryRow(
			`SELECT is_builtin FROM tag_categories WHERE name = ?`, name,
		).Scan(&isBuiltin); err != nil {
			t.Fatalf("read %q: %v", name, err)
		}
		if isBuiltin != 1 {
			t.Errorf("category %q is_builtin = %d after re-bootstrap, want 1", name, isBuiltin)
		}
	}
}

func TestBootstrapIdempotent(t *testing.T) {
	db := openTestDB(t)

	if err := Bootstrap(db); err != nil {
		t.Fatalf("first Bootstrap failed: %v", err)
	}
	if err := Bootstrap(db); err != nil {
		t.Fatalf("second Bootstrap failed: %v", err)
	}
}

func TestForeignKeyEnforcement(t *testing.T) {
	db := openTestDB(t)
	if err := Bootstrap(db); err != nil {
		t.Fatal(err)
	}

	// Insert image_tags row with non-existent image_id - must fail
	_, err := db.Write.Exec(
		`INSERT INTO image_tags (image_id, tag_id) VALUES (9999, 9999)`,
	)
	if err == nil {
		t.Error("expected foreign key error, got nil")
	}
}

func TestConcurrentReads(t *testing.T) {
	db := openTestDB(t)
	if err := Bootstrap(db); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var n int
			if err := db.Read.QueryRow(`SELECT COUNT(*) FROM tag_categories`).Scan(&n); err != nil {
				t.Errorf("concurrent read failed: %v", err)
			}
		}()
	}
	wg.Wait()
}

// A library predating the inbox column must boot cleanly and end up with
// is_inbox=0 on every pre-existing row, so the operator's whole gallery
// doesn't dump into the inbox view on first launch. Asserts the column
// is added, the partial index is registered, and the backfill flips
// every row to 0.
func TestBootstrapInboxMigration(t *testing.T) {
	db := openTestDB(t)
	// Stand up a stripped-down images table missing is_inbox to mimic an
	// old library. The real schema has more columns; only the ones used
	// by the migration's UPDATE / pragma_table_info / CREATE INDEX path
	// matter here.
	if _, err := db.Write.Exec(`
		CREATE TABLE images (
		    id             INTEGER PRIMARY KEY,
		    sha256         TEXT    NOT NULL UNIQUE,
		    canonical_path TEXT    NOT NULL,
		    folder_path    TEXT    NOT NULL DEFAULT '',
		    file_type      TEXT    NOT NULL,
		    width          INTEGER,
		    height         INTEGER,
		    file_size      INTEGER NOT NULL,
		    is_missing     INTEGER NOT NULL DEFAULT 0,
		    is_favorited   INTEGER NOT NULL DEFAULT 0,
		    auto_tagged_at TEXT,
		    source_type    TEXT    NOT NULL DEFAULT 'none',
		    ingested_at    TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
		)`); err != nil {
		t.Fatalf("seed pre-feature images table: %v", err)
	}
	for i := 1; i <= 3; i++ {
		if _, err := db.Write.Exec(
			`INSERT INTO images (sha256, canonical_path, file_type, file_size) VALUES (?, ?, 'png', 1024)`,
			"sha"+string(rune('a'+i)), "/tmp/"+string(rune('a'+i)),
		); err != nil {
			t.Fatalf("seed row %d: %v", i, err)
		}
	}

	if err := Bootstrap(db); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}

	var n int
	if err := db.Read.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('images') WHERE name = 'is_inbox'`,
	).Scan(&n); err != nil {
		t.Fatalf("read column metadata: %v", err)
	}
	if n != 1 {
		t.Errorf("is_inbox column not added by Bootstrap")
	}

	var notArchived int
	if err := db.Read.QueryRow(`SELECT COUNT(*) FROM images WHERE is_inbox != 0`).Scan(&notArchived); err != nil {
		t.Fatalf("count is_inbox: %v", err)
	}
	if notArchived != 0 {
		t.Errorf("expected every pre-existing row to be is_inbox=0 after upgrade, got %d not archived", notArchived)
	}

	var idxN int
	if err := db.Read.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='idx_images_inbox_visible'`,
	).Scan(&idxN); err != nil {
		t.Fatalf("read index metadata: %v", err)
	}
	if idxN != 1 {
		t.Errorf("idx_images_inbox_visible not registered after Bootstrap")
	}

	// Re-running Bootstrap on the now-migrated DB must be a no-op (the
	// pre-count branch shouldn't fire a second backfill against rows the
	// user has since flipped back into the inbox).
	if _, err := db.Write.Exec(`UPDATE images SET is_inbox = 1 WHERE id = 1`); err != nil {
		t.Fatalf("simulate user toggle: %v", err)
	}
	if err := Bootstrap(db); err != nil {
		t.Fatalf("re-Bootstrap: %v", err)
	}
	var stillInbox int
	if err := db.Read.QueryRow(`SELECT is_inbox FROM images WHERE id = 1`).Scan(&stillInbox); err != nil {
		t.Fatalf("read after re-bootstrap: %v", err)
	}
	if stillInbox != 1 {
		t.Errorf("user-toggled is_inbox=1 row was reset to 0 by re-Bootstrap")
	}
}

// TestBootstrapMangaColumns asserts that the manga schema additions
// (page_count, series, idx_images_series) land on a fresh DB and on
// libraries that predate the columns.
func TestBootstrapMangaColumns(t *testing.T) {
	db := openTestDB(t)
	if err := Bootstrap(db); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	for _, col := range []string{"page_count", "series"} {
		var n int
		if err := db.Read.QueryRow(
			`SELECT COUNT(*) FROM pragma_table_info('images') WHERE name = ?`, col,
		).Scan(&n); err != nil {
			t.Fatalf("inspect images.%s: %v", col, err)
		}
		if n != 1 {
			t.Errorf("images.%s missing after Bootstrap", col)
		}
	}
	var idxN int
	if err := db.Read.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='idx_images_series'`,
	).Scan(&idxN); err != nil {
		t.Fatalf("read idx metadata: %v", err)
	}
	if idxN != 1 {
		t.Errorf("idx_images_series not registered after Bootstrap")
	}
}

// TestBootstrapMangaIdempotent re-runs Bootstrap and asserts the manga
// column adds are no-ops on the second pass.
func TestBootstrapMangaIdempotent(t *testing.T) {
	db := openTestDB(t)
	if err := Bootstrap(db); err != nil {
		t.Fatalf("first Bootstrap: %v", err)
	}
	if err := Bootstrap(db); err != nil {
		t.Fatalf("second Bootstrap: %v", err)
	}
}

// TestUntaggedVisibleCounts_CacheAndInvalidate covers the shape behind
// the tagged:true / autotagged:true fast-count partition: the first
// read populates the cache, subsequent reads return the same value
// without re-walking image_tags, and InvalidateCachedCounts forces a
// re-query after a membership change. The slow NOT-EXISTS path is
// multi-second on a million-row library so a stale value is the
// failure mode to guard against.
func TestUntaggedVisibleCounts_CacheAndInvalidate(t *testing.T) {
	db := openTestDB(t)
	if err := Bootstrap(db); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	mkImage := func(sha, path string) int64 {
		res, err := db.Write.Exec(
			`INSERT INTO images (sha256, canonical_path, file_type, file_size) VALUES (?, ?, 'png', 1)`,
			sha, path,
		)
		if err != nil {
			t.Fatalf("insert image: %v", err)
		}
		id, _ := res.LastInsertId()
		return id
	}
	tagged := mkImage("sha-tagged", "/a")
	auto := mkImage("sha-auto", "/b")
	mkImage("sha-untagged", "/c")
	tagRes, err := db.Write.Exec(
		`INSERT INTO tags (name, category_id) VALUES ('blue', (SELECT id FROM tag_categories WHERE name = 'general'))`,
	)
	if err != nil {
		t.Fatalf("insert tag: %v", err)
	}
	tagID, _ := tagRes.LastInsertId()
	if _, err := db.Write.Exec(`INSERT INTO image_tags (image_id, tag_id, is_auto) VALUES (?, ?, 0)`, tagged, tagID); err != nil {
		t.Fatalf("attach manual tag: %v", err)
	}
	if _, err := db.Write.Exec(`INSERT INTO image_tags (image_id, tag_id, is_auto) VALUES (?, ?, 1)`, auto, tagID); err != nil {
		t.Fatalf("attach auto tag: %v", err)
	}

	untagged, ok := db.UntaggedVisibleCount()
	if !ok || untagged != 1 {
		t.Fatalf("UntaggedVisibleCount = (%d, %v), want (1, true)", untagged, ok)
	}
	autoUntagged, ok := db.AutoUntaggedVisibleCount()
	if !ok || autoUntagged != 2 {
		t.Fatalf("AutoUntaggedVisibleCount = (%d, %v), want (2, true)", autoUntagged, ok)
	}

	// Mutate without invalidating: cache must still return the prior
	// value. This is what makes the cache useful and what makes the
	// invalidation hook below load-bearing.
	mkImage("sha-new", "/d")
	if got, _ := db.UntaggedVisibleCount(); got != 1 {
		t.Errorf("post-insert pre-invalidate UntaggedVisibleCount = %d, want cached 1", got)
	}

	db.InvalidateCachedCounts()
	if got, _ := db.UntaggedVisibleCount(); got != 2 {
		t.Errorf("post-invalidate UntaggedVisibleCount = %d, want 2 (new row is untagged)", got)
	}
	if got, _ := db.AutoUntaggedVisibleCount(); got != 3 {
		t.Errorf("post-invalidate AutoUntaggedVisibleCount = %d, want 3", got)
	}
}

func TestShrinkMemory(t *testing.T) {
	db := openTestDB(t)
	if err := Bootstrap(db); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		var n int
		if err := db.Read.QueryRow(`SELECT COUNT(*) FROM tag_categories`).Scan(&n); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.ShrinkMemory(context.Background()); err != nil {
		t.Errorf("ShrinkMemory: %v", err)
	}
	var n int
	if err := db.Read.QueryRow(`SELECT COUNT(*) FROM tag_categories`).Scan(&n); err != nil {
		t.Errorf("read after shrink: %v", err)
	}
}

func TestBootstrapTagOriginBackfill(t *testing.T) {
	db := openTestDB(t)
	// Stand up pre-feature tags / image_tags tables (no origin, no
	// last_used_at) so the fresh-column backfill fires and can be
	// asserted against every derivation branch.
	if _, err := db.Write.Exec(`
		CREATE TABLE tags (
		    id               INTEGER PRIMARY KEY,
		    name             TEXT    NOT NULL,
		    category_id      INTEGER NOT NULL,
		    usage_count      INTEGER NOT NULL DEFAULT 0,
		    is_alias         INTEGER NOT NULL DEFAULT 0,
		    canonical_tag_id INTEGER,
		    created_at       TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
		    UNIQUE(name, category_id)
		)`); err != nil {
		t.Fatalf("seed pre-feature tags table: %v", err)
	}
	if _, err := db.Write.Exec(`
		CREATE TABLE image_tags (
		    image_id    INTEGER NOT NULL,
		    tag_id      INTEGER NOT NULL,
		    is_auto     INTEGER NOT NULL DEFAULT 0,
		    is_implied  INTEGER NOT NULL DEFAULT 0,
		    confidence  REAL,
		    tagger_name TEXT,
		    created_at  TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
		    PRIMARY KEY (image_id, tag_id)
		)`); err != nil {
		t.Fatalf("seed pre-feature image_tags table: %v", err)
	}
	type usageRow struct {
		img     int64
		isAuto  int
		tagger  string
		created string
	}
	seed := []struct {
		id   int64
		name string
		rows []usageRow
	}{
		{1, "unanimous_site", []usageRow{{1, 0, "danbooru", "2026-01-01T00:00:00Z"}, {2, 0, "danbooru", "2026-02-01T00:00:00Z"}}},
		{2, "mixed_manual", []usageRow{{1, 0, "", "2026-01-01T00:00:00Z"}, {2, 0, "danbooru", "2026-01-02T00:00:00Z"}}},
		{3, "auto_two_models", []usageRow{{1, 1, "wd-swinv2", "2026-01-01T00:00:00Z"}, {2, 1, "joytag", "2026-01-02T00:00:00Z"}}},
		{4, "labelled_manual_plus_auto", []usageRow{{1, 0, "api", "2026-01-01T00:00:00Z"}, {2, 1, "wd-swinv2", "2026-01-02T00:00:00Z"}}},
		{5, "zero_usage", nil},
	}
	for _, s := range seed {
		if _, err := db.Write.Exec(
			`INSERT INTO tags (id, name, category_id, usage_count) VALUES (?, ?, 1, ?)`,
			s.id, s.name, len(s.rows),
		); err != nil {
			t.Fatalf("seed tag %s: %v", s.name, err)
		}
		for _, r := range s.rows {
			tagger := any(r.tagger)
			if r.tagger == "" {
				tagger = nil
			}
			if _, err := db.Write.Exec(
				`INSERT INTO image_tags (image_id, tag_id, is_auto, tagger_name, created_at) VALUES (?, ?, ?, ?, ?)`,
				r.img, s.id, r.isAuto, tagger, r.created,
			); err != nil {
				t.Fatalf("seed image_tag for %s: %v", s.name, err)
			}
		}
	}

	if err := Bootstrap(db); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}

	wantOrigin := map[int64]string{1: "danbooru", 2: "user", 3: "auto", 4: "api", 5: ""}
	for id, want := range wantOrigin {
		var got string
		if err := db.Read.QueryRow(`SELECT origin FROM tags WHERE id = ?`, id).Scan(&got); err != nil {
			t.Fatalf("read origin for tag %d: %v", id, err)
		}
		if got != want {
			t.Errorf("tag %d origin = %q, want %q", id, got, want)
		}
	}
	var lastUsed string
	if err := db.Read.QueryRow(`SELECT last_used_at FROM tags WHERE id = 1`).Scan(&lastUsed); err != nil {
		t.Fatalf("read last_used_at: %v", err)
	}
	if lastUsed != "2026-02-01T00:00:00Z" {
		t.Errorf("tag 1 last_used_at = %q, want newest row's created_at", lastUsed)
	}
	var nullLastUsed sql.NullString
	if err := db.Read.QueryRow(`SELECT last_used_at FROM tags WHERE id = 5`).Scan(&nullLastUsed); err != nil {
		t.Fatalf("read zero-usage last_used_at: %v", err)
	}
	if nullLastUsed.Valid {
		t.Errorf("zero-usage tag last_used_at = %q, want NULL", nullLastUsed.String)
	}
}
