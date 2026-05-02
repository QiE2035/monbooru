package db

import (
	"context"
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
	t.Cleanup(func() { db.Close() })
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
		"sd_metadata", "comfyui_metadata", "saved_searches",
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
