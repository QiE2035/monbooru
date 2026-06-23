package relations

import (
	"context"
	"testing"
)

// TestBackfillPhashes seeds rows with NULL phash and decodable
// thumbnails on disk, runs the compute-phashes backfill, and asserts the
// returned processed/updated counts plus that every seeded row now
// carries a non-NULL phash. Mirrors job_findpairs_test.go's thumbnail
// setup (writeTestThumb writes <id>.jpg, which is exactly what
// gallery.ThumbnailPath / RecomputeAndStorePhash read).
func TestBackfillPhashes(t *testing.T) {
	database, _ := setupTestDB(t)
	a := insertImage(t, database, "a", 1000)
	b := insertImage(t, database, "b", 2000)
	c := insertImage(t, database, "c", 3000)
	// insertImage leaves phash NULL; make the precondition explicit so the
	// test still means something if the column default ever changes.
	if _, err := database.Write.Exec(`UPDATE images SET phash = NULL WHERE id IN (?, ?, ?)`, a, b, c); err != nil {
		t.Fatal(err)
	}
	thumbs := t.TempDir()
	writeTestThumb(t, thumbs, a)
	writeTestThumb(t, thumbs, b)
	writeTestThumb(t, thumbs, c)

	processed, updated, err := BackfillPhashes(context.Background(), database, thumbs, nil)
	if err != nil {
		t.Fatalf("BackfillPhashes: %v", err)
	}
	if processed != 3 {
		t.Fatalf("processed = %d, want 3", processed)
	}
	if updated != 3 {
		t.Fatalf("updated = %d, want 3 (all thumbnails decodable)", updated)
	}

	var nullCount int
	if err := database.Read.QueryRow(`SELECT COUNT(*) FROM images WHERE phash IS NULL`).Scan(&nullCount); err != nil {
		t.Fatal(err)
	}
	if nullCount != 0 {
		t.Fatalf("rows still NULL = %d, want 0 (every seeded row backfilled)", nullCount)
	}
}

// TestBackfillPhashesSkipsMissingAndUndecodable confirms the job counts
// every candidate as processed but only the ones whose thumbnail decodes
// as updated, and that an is_missing=1 row is never a candidate at all.
func TestBackfillPhashesSkipsMissingAndUndecodable(t *testing.T) {
	database, _ := setupTestDB(t)
	good := insertImage(t, database, "good", 1000)
	noThumb := insertImage(t, database, "nothumb", 2000) // candidate, but its thumbnail is absent
	missing := insertImage(t, database, "missing", 3000) // excluded by is_missing = 0 filter
	if _, err := database.Write.Exec(`UPDATE images SET phash = NULL`); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Write.Exec(`UPDATE images SET is_missing = 1 WHERE id = ?`, missing); err != nil {
		t.Fatal(err)
	}
	thumbs := t.TempDir()
	writeTestThumb(t, thumbs, good)
	// Deliberately no thumbnail for noThumb so its compute fails and the
	// row stays at NULL (logged at debug, processed but not updated).

	processed, updated, err := BackfillPhashes(context.Background(), database, thumbs, nil)
	if err != nil {
		t.Fatalf("BackfillPhashes: %v", err)
	}
	// good + noThumb are candidates (NULL, not missing); missing is filtered out.
	if processed != 2 {
		t.Fatalf("processed = %d, want 2 (good + noThumb; missing excluded)", processed)
	}
	if updated != 1 {
		t.Fatalf("updated = %d, want 1 (only good has a decodable thumbnail)", updated)
	}

	var phashGood, phashNoThumb, phashMissing interface{}
	if err := database.Read.QueryRow(`SELECT phash FROM images WHERE id = ?`, good).Scan(&phashGood); err != nil {
		t.Fatal(err)
	}
	if phashGood == nil {
		t.Fatal("good row still NULL, want a computed phash")
	}
	if err := database.Read.QueryRow(`SELECT phash FROM images WHERE id = ?`, noThumb).Scan(&phashNoThumb); err != nil {
		t.Fatal(err)
	}
	if phashNoThumb != nil {
		t.Fatal("noThumb row got a phash, want NULL (no thumbnail to decode)")
	}
	if err := database.Read.QueryRow(`SELECT phash FROM images WHERE id = ?`, missing).Scan(&phashMissing); err != nil {
		t.Fatal(err)
	}
	if phashMissing != nil {
		t.Fatal("missing (is_missing=1) row got a phash, want NULL (never a candidate)")
	}
}

// TestBackfillPhashesReportsProgress checks the progress callback fires
// and the final call reports the terminal processed/total figures the
// status bar reads.
func TestBackfillPhashesReportsProgress(t *testing.T) {
	database, _ := setupTestDB(t)
	a := insertImage(t, database, "a", 1000)
	b := insertImage(t, database, "b", 2000)
	if _, err := database.Write.Exec(`UPDATE images SET phash = NULL WHERE id IN (?, ?)`, a, b); err != nil {
		t.Fatal(err)
	}
	thumbs := t.TempDir()
	writeTestThumb(t, thumbs, a)
	writeTestThumb(t, thumbs, b)

	var lastProcessed, lastTotal, calls int
	progress := func(processed, total int, _ string) {
		calls++
		lastProcessed, lastTotal = processed, total
	}
	processed, updated, err := BackfillPhashes(context.Background(), database, thumbs, progress)
	if err != nil {
		t.Fatalf("BackfillPhashes: %v", err)
	}
	if calls == 0 {
		t.Fatal("progress callback never fired")
	}
	if lastTotal != 2 {
		t.Fatalf("final total = %d, want 2", lastTotal)
	}
	if lastProcessed != processed || lastProcessed != 2 {
		t.Fatalf("final processed = %d, want %d (== returned processed, == 2)", lastProcessed, processed)
	}
	if updated != 2 {
		t.Fatalf("updated = %d, want 2", updated)
	}
}
