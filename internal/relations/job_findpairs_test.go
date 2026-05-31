package relations

import (
	"context"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// Common shape across find-pairs tests: ingest a handful of synthetic
// images, manually pin their phashes so the math is deterministic,
// then assert what the FindPairs walk produces.
func TestFindPairsBasic(t *testing.T) {
	database, _ := setupTestDB(t)
	a := insertImage(t, database, "a", 1000)
	b := insertImage(t, database, "b", 2000)
	c := insertImage(t, database, "c", 3000)
	for _, p := range []struct {
		id   int64
		hash int64
	}{
		{a, 0x00},
		{b, 0x01}, // distance 1 from a
		{c, 0xF0}, // distance 4 from a
	} {
		if _, err := database.Write.Exec(`UPDATE images SET phash = ? WHERE id = ?`, p.hash, p.id); err != nil {
			t.Fatal(err)
		}
	}
	tree := NewBKTree()
	added, err := FindPairs(context.Background(), database, tree, FindPairsOptions{Distance: 2}, nil)
	if err != nil {
		t.Fatalf("FindPairs: %v", err)
	}
	if added != 1 {
		t.Fatalf("added = %d, want 1 (only a-b within distance 2)", added)
	}
	var n int
	if err := database.Read.QueryRow(`SELECT COUNT(*) FROM potential_relation_pairs`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("queue size = %d, want 1", n)
	}
}

func TestFindPairsSkipsAlreadyRelated(t *testing.T) {
	database, svc := setupTestDB(t)
	a := insertImage(t, database, "a", 100)
	b := insertImage(t, database, "b", 200)
	c := insertImage(t, database, "c", 300)
	for _, p := range []struct {
		id   int64
		hash int64
	}{{a, 0}, {b, 1}, {c, 2}} {
		if _, err := database.Write.Exec(`UPDATE images SET phash = ? WHERE id = ?`, p.hash, p.id); err != nil {
			t.Fatal(err)
		}
	}
	// a-b is already a duplicate; FindPairs must not surface it.
	if err := svc.AddDuplicate(a, b); err != nil {
		t.Fatal(err)
	}
	// a-c is not-related; also skipped.
	if err := svc.AddNotRelated(a, c); err != nil {
		t.Fatal(err)
	}
	tree := NewBKTree()
	added, err := FindPairs(context.Background(), database, tree, FindPairsOptions{Distance: 5}, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Only b-c should be added.
	if added != 1 {
		t.Fatalf("added = %d, want 1 (only b-c is fresh)", added)
	}
	var ai, bi int64
	if err := database.Read.QueryRow(`SELECT a_image_id, b_image_id FROM potential_relation_pairs`).Scan(&ai, &bi); err != nil {
		t.Fatal(err)
	}
	if ai != b || bi != c {
		t.Fatalf("queue row = (%d, %d), want (%d, %d)", ai, bi, b, c)
	}
}

func TestFindPairsReplaceWipesQueue(t *testing.T) {
	database, _ := setupTestDB(t)
	a := insertImage(t, database, "a", 100)
	b := insertImage(t, database, "b", 200)
	for _, p := range []struct {
		id   int64
		hash int64
	}{{a, 0}, {b, 1}} {
		if _, err := database.Write.Exec(`UPDATE images SET phash = ? WHERE id = ?`, p.hash, p.id); err != nil {
			t.Fatal(err)
		}
	}
	// Pre-seed a stale row.
	if _, err := database.Write.Exec(
		`INSERT INTO potential_relation_pairs (a_image_id, b_image_id, distance, created_at) VALUES (?, ?, ?, 'stale')`,
		a, b, 99,
	); err != nil {
		t.Fatal(err)
	}
	tree := NewBKTree()
	if _, err := FindPairs(context.Background(), database, tree, FindPairsOptions{Distance: 2, Replace: true}, nil); err != nil {
		t.Fatal(err)
	}
	var dist int
	if err := database.Read.QueryRow(`SELECT distance FROM potential_relation_pairs`).Scan(&dist); err != nil {
		t.Fatal(err)
	}
	if dist != 1 {
		t.Fatalf("queue distance after replace = %d, want 1 (recomputed)", dist)
	}
}

func TestFindPairsLazyPhashCompute(t *testing.T) {
	// When an image's phash is NULL, FindPairs would attempt to compute
	// it from the thumbnail on disk. There's no thumbnail here, so the
	// compute fails silently and that image is dropped from the walk.
	// Verifies the lazy-compute branch doesn't crash on a missing
	// thumbnail.
	database, _ := setupTestDB(t)
	a := insertImage(t, database, "a", 100)
	b := insertImage(t, database, "b", 200)
	if _, err := database.Write.Exec(`UPDATE images SET phash = NULL WHERE id IN (?, ?)`, a, b); err != nil {
		t.Fatal(err)
	}
	tree := NewBKTree()
	added, err := FindPairs(context.Background(), database, tree, FindPairsOptions{Distance: 4, ThumbnailsPath: "/no/such/path"}, nil)
	if err != nil {
		t.Fatalf("FindPairs: %v", err)
	}
	if added != 0 {
		t.Fatalf("added = %d, want 0 (no decodable thumbnails, lazy compute fails)", added)
	}
}

// writeTestThumb writes a deterministic gradient JPEG at <dir>/<id>.jpg so
// FindPairs' lazy phash compute has something to decode.
func writeTestThumb(t *testing.T, dir string, id int64) {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 64, 64))
	for y := 0; y < 64; y++ {
		for x := 0; x < 64; x++ {
			v := uint8((x + y) * 255 / 126)
			img.SetRGBA(x, y, color.RGBA{R: v, G: v, B: v, A: 255})
		}
	}
	f, err := os.Create(filepath.Join(dir, strconv.FormatInt(id, 10)+".jpg"))
	if err != nil {
		t.Fatal(err)
	}
	if err := jpeg.Encode(f, img, &jpeg.Options{Quality: 85}); err != nil {
		f.Close()
		t.Fatal(err)
	}
	f.Close()
}

// Two images with NULL phash and identical thumbnails must surface as a
// pair. Their phashes are computed lazily during the run; the probe must
// still find the match even though the higher-id member is phashed inside
// the same run. Regression for the a < b canonicalisation dropping the
// pair when both members started NULL.
func TestFindPairsLazyComputeFindsPair(t *testing.T) {
	database, _ := setupTestDB(t)
	a := insertImage(t, database, "a", 1000)
	b := insertImage(t, database, "b", 2000)
	if _, err := database.Write.Exec(`UPDATE images SET phash = NULL WHERE id IN (?, ?)`, a, b); err != nil {
		t.Fatal(err)
	}
	thumbs := t.TempDir()
	writeTestThumb(t, thumbs, a)
	writeTestThumb(t, thumbs, b)
	tree := NewBKTree()
	added, err := FindPairs(context.Background(), database, tree, FindPairsOptions{Distance: 4, ThumbnailsPath: thumbs}, nil)
	if err != nil {
		t.Fatalf("FindPairs: %v", err)
	}
	if added != 1 {
		t.Fatalf("added = %d, want 1 (identical lazily-phashed thumbnails are a pair)", added)
	}
	var ai, bi int64
	if err := database.Read.QueryRow(`SELECT a_image_id, b_image_id FROM potential_relation_pairs`).Scan(&ai, &bi); err != nil {
		t.Fatal(err)
	}
	if ai != a || bi != b {
		t.Fatalf("queue row = (%d, %d), want (%d, %d)", ai, bi, a, b)
	}
}
