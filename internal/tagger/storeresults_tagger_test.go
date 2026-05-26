//go:build tagger

package tagger

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/leqwin/monbooru/internal/db"
)

// setupStoreResultsDB opens a fresh per-test DB, bootstraps the
// schema, and returns it. Mirrors the helper in internal/tags so the
// test is self-contained and doesn't drag in a Service.
func setupStoreResultsDB(t *testing.T) *db.DB {
	t.Helper()
	dir := t.TempDir()
	database, err := db.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := db.Bootstrap(database); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	return database
}

// insertTaggerTestImage inserts a non-missing image and returns its id.
func insertTaggerTestImage(t *testing.T, database *db.DB, sha string) int64 {
	t.Helper()
	res, err := database.Write.Exec(
		`INSERT INTO images (sha256, canonical_path, folder_path, file_type, file_size, is_missing)
		 VALUES (?, ?, '', 'png', 0, 0)`,
		sha, "/tmp/"+sha+".png",
	)
	if err != nil {
		t.Fatalf("insert image: %v", err)
	}
	id, _ := res.LastInsertId()
	return id
}

// catID returns the id of a tag_categories row by name.
func catIDForName(t *testing.T, database *db.DB, name string) int64 {
	t.Helper()
	var id int64
	if err := database.Read.QueryRow(`SELECT id FROM tag_categories WHERE name = ?`, name).Scan(&id); err != nil {
		t.Fatalf("category %q: %v", name, err)
	}
	return id
}

// TestStoreResults_ScopesDeleteToTaggerNames pins the central
// invariant: an inference run by tagger A must remove A's prior auto
// tags that aren't in the new merged set, but leave tagger B's rows
// alone. The pipeline is build-tag-gated; this test runs only under
// `-tags tagger`.
func TestStoreResults_ScopesDeleteToTaggerNames(t *testing.T) {
	database := setupStoreResultsDB(t)
	general := catIDForName(t, database, "general")
	imageID := insertTaggerTestImage(t, database, "sr1")

	// Seed three image_tags: one manual, one auto from tagger-A
	// (will be deleted because the new merged set drops it), one auto
	// from tagger-B (must survive because the run is scoped to A).
	mustExec := func(q string, args ...any) {
		t.Helper()
		if _, err := database.Write.Exec(q, args...); err != nil {
			t.Fatalf("exec %q: %v", q, err)
		}
	}
	insertTag := func(name string) int64 {
		t.Helper()
		res, err := database.Write.Exec(
			`INSERT INTO tags (name, category_id, usage_count) VALUES (?, ?, 0)`, name, general,
		)
		if err != nil {
			t.Fatalf("insert tag %q: %v", name, err)
		}
		id, _ := res.LastInsertId()
		return id
	}
	manualID := insertTag("manual_tag")
	staleID := insertTag("stale_auto_a")
	keepID := insertTag("keep_auto_b")

	mustExec(`INSERT INTO image_tags (image_id, tag_id, is_auto, tagger_name) VALUES (?, ?, 0, NULL)`, imageID, manualID)
	mustExec(`INSERT INTO image_tags (image_id, tag_id, is_auto, tagger_name) VALUES (?, ?, 1, ?)`, imageID, staleID, "tagger-A")
	mustExec(`INSERT INTO image_tags (image_id, tag_id, is_auto, tagger_name) VALUES (?, ?, 1, ?)`, imageID, keepID, "tagger-B")

	// New merged set from a tagger-A run: a single tag (different
	// from the prior auto-A one). storeResults should:
	//   - delete stale_auto_a (in scope, not in target set)
	//   - keep manual_tag (not auto, never in scope)
	//   - keep keep_auto_b (not in scope)
	//   - insert the new tag
	merged := map[TagKey]Scored{
		{Name: "fresh_auto_a", CatID: general}: {Score: 0.9, TaggerName: "tagger-A"},
	}

	if err := storeResults(context.Background(), database, imageID, merged, []string{"tagger-A"}, 0); err != nil {
		t.Fatalf("storeResults: %v", err)
	}

	got := map[string]bool{}
	rows, err := database.Read.Query(
		`SELECT t.name FROM image_tags it JOIN tags t ON t.id = it.tag_id WHERE it.image_id = ?`,
		imageID,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		got[name] = true
	}
	for _, want := range []string{"manual_tag", "keep_auto_b", "fresh_auto_a"} {
		if !got[want] {
			t.Errorf("expected %q to be present after storeResults; got %v", want, got)
		}
	}
	if got["stale_auto_a"] {
		t.Errorf("stale_auto_a should have been deleted by the tagger-A run; got %v", got)
	}

	// auto_tagged_at must land so the user-facing badge surfaces.
	var autoTaggedAt *string
	if err := database.Read.QueryRow(
		`SELECT auto_tagged_at FROM images WHERE id = ?`, imageID,
	).Scan(&autoTaggedAt); err != nil {
		t.Fatal(err)
	}
	if autoTaggedAt == nil {
		t.Errorf("auto_tagged_at is NULL after storeResults; expected a timestamp")
	}
}

// TestStoreResults_RatingPruneKeepsHighestRank pins the rating-row
// invariant: a tagger run that emits multiple ratings (or merges into
// an image carrying a lower rating) must keep only the highest rank.
func TestStoreResults_RatingPruneKeepsHighestRank(t *testing.T) {
	database := setupStoreResultsDB(t)
	ratingCat := catIDForName(t, database, "rating")
	imageID := insertTaggerTestImage(t, database, "sr2")

	// Resolve the seeded rating tag ids.
	tagID := func(name string) int64 {
		t.Helper()
		var id int64
		if err := database.Read.QueryRow(
			`SELECT id FROM tags WHERE name = ? AND category_id = ?`, name, ratingCat,
		).Scan(&id); err != nil {
			t.Fatalf("rating tag %q: %v", name, err)
		}
		return id
	}
	generalRatingID := tagID("general")
	explicitRatingID := tagID("explicit")
	_ = explicitRatingID

	// Pre-existing low-rank rating from a prior run.
	if _, err := database.Write.Exec(
		`INSERT INTO image_tags (image_id, tag_id, is_auto, tagger_name) VALUES (?, ?, 1, ?)`,
		imageID, generalRatingID, "tagger-A",
	); err != nil {
		t.Fatal(err)
	}

	merged := map[TagKey]Scored{
		{Name: "explicit", CatID: ratingCat}: {Score: 0.95, TaggerName: "tagger-A"},
	}
	if err := storeResults(context.Background(), database, imageID, merged, []string{"tagger-A"}, ratingCat); err != nil {
		t.Fatalf("storeResults: %v", err)
	}

	rows, err := database.Read.Query(
		`SELECT t.name FROM image_tags it JOIN tags t ON t.id = it.tag_id
		 WHERE it.image_id = ? AND t.category_id = ?`,
		imageID, ratingCat,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var ratings []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		ratings = append(ratings, n)
	}
	if len(ratings) != 1 || ratings[0] != "explicit" {
		t.Errorf("post-storeResults ratings = %v, want [explicit] only", ratings)
	}
}
